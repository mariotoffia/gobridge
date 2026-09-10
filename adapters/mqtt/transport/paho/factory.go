package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.TransportFactory for MQTT via Eclipse Paho.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

var _ ports.TransportFactory = (*Factory)(nil)

var errInvalidConfig = errors.New("mqtt: spec.Config must be of type paho.Config")

// NewFactory creates an MQTT Paho transport factory.
func NewFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *Factory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &Factory{Logger: logger, Metrics: m}
}

// Capabilities returns the transport capabilities for MQTT.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapExclusiveIdentity,
		// MQTT has one serialized ingress dispatch and protocol-ACK domain per
		// session. One receiver per session confines route backpressure and
		// settlement failure to that receiver's broker connection.
		ports.CapDedicatedIngressSession,
		// MQTT supports shared subscriptions ($share/<group>/<filter>): the
		// broker load-balances a topic's deliveries across the group members,
		// so multiple bridge instances can scale-out consumption of one
		// logical subscription. topic_match.go strips the $share prefix for
		// dispatch; advertise the capability so the runtime/operators can
		// discover the scale-out path.
		ports.CapSharedConsumer,
		// MQTT receivers subscribe ONLY when the session manager reconciles the
		// SessionPlan, so a receiver on an unmanaged session is silently inert;
		// the builder gives every such session a manager, through the receiver's
		// own binding to it when no route session block or binding names it.
		ports.CapPlanDrivenSubscriptions,
	}
}

// AddressValidator returns the MQTT topic validator the runtime
// invokes against every fully-rendered publish topic. The validator
// is shared across all bindings backed by this transport.
func (f *Factory) AddressValidator() ports.AddressValidator {
	return NewAddressValidator()
}

// NewSession creates an MQTT Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("mqtt session %q: %s", spec.ID, err))
	}
	if err := cfg.Validate(); err != nil {
		return nil, shared.ErrInvalidConfig.Wrap(err).WithMessage(
			fmt.Sprintf("mqtt session %q: invalid configuration", spec.ID))
	}
	// Config.Validate deliberately defers receive-dependent validation when the
	// parser saw an omitted Receive Maximum so deployment profiles can derive a
	// safe value. Session construction is the final adapter build boundary:
	// require the complete effective window here as defense in depth even when a
	// caller bypasses bridge.Builder's resource-free preflight.
	if err := cfg.ValidateIngressMemory(0); err != nil {
		return nil, shared.ErrInvalidConfig.Wrap(err).WithMessage(
			fmt.Sprintf("mqtt session %q: invalid ingress memory configuration", spec.ID))
	}
	mode := spec.SessionMode
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	cfg.Session.normalizeBrokerURLs()
	if err := cfg.ValidateEffectiveSession(mode); err != nil {
		return nil, shared.ErrInvalidConfig.Wrap(err).WithMessage(
			fmt.Sprintf("mqtt session %q: invalid effective session configuration", spec.ID))
	}
	opts := cfg.Session
	// Expand a validated per-replica suffix only at construction. The
	// resource-free preflight above validates the token without resolving a
	// hostname or consuming randomness.
	if opts.ClientIDSuffix != "" {
		resolved, err := cfg.resolveClientIDSuffix(opts.ClientID, opts.ClientIDSuffix)
		if err != nil {
			return nil, shared.ErrInvalidConfig.Wrap(err).WithMessage(
				fmt.Sprintf("mqtt session %q: invalid client_id_suffix", spec.ID))
		}
		opts.ClientID = resolved
		// Persistent mode keys the broker's durable session (subscriptions +
		// queued offline QoS 1/2) to the effective client_id. A hostname suffix is
		// stable only where the hostname is (StatefulSet pods, VMs); on a Kubernetes
		// Deployment or ECS task every rollout mints a NEW pod/task name → new
		// client_id → new broker session, ORPHANING the old session's queued
		// messages until session_expiry_interval silently expires them — loss by
		// timeout, invisible to the bridge.
		//
		// A startup warning is not an admission boundary, so this
		// combination is now REJECTED at build time unless the operator explicitly
		// asserts a stable-host profile via assert_stable_client_identity. The
		// assertion is the operator vouching for StatefulSet/VM identity; it does
		// not make Deployment/ECS safe. See docs/transports/mqtt.md#deployment-identity
		// (StatefulSet, Exclusive mode, or Ephemeral+$share are the safe shapes).
		if mode == connectivity.SessionPersistent && opts.ClientIDSuffix == ClientIDSuffixHostname {
			if !opts.AssertStableClientIdentity {
				return nil, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
					"mqtt session %q: session_mode=persistent with client_id_suffix=hostname is unsafe on a "+
						"Kubernetes Deployment or ECS service — every rollout mints a new hostname → new "+
						"client_id → new broker session, stranding the previous session's queued QoS 1/2 "+
						"messages until session_expiry_interval silently expires them. Use a StatefulSet, "+
						"session_mode: exclusive, or ephemeral + $share; or set assert_stable_client_identity: "+
						"true to affirm a stable-host profile (StatefulSet/VM)", spec.ID))
			}
			if f.Logger != nil {
				f.Logger.Warn("mqtt: session_mode=persistent with client_id_suffix=hostname admitted only "+
					"because assert_stable_client_identity=true — the operator vouches for a stable hostname "+
					"(StatefulSet, VM). This is UNSAFE on a Deployment/ECS service (silent QoS 1/2 loss by "+
					"session_expiry).",
					"session_id", spec.ID,
					"client_id", opts.ClientID,
				)
			}
		}
	}
	if spec.ManagedSubscriptionsRequired {
		if mode == connectivity.SessionEphemeral {
			return nil, shared.ErrInvalidConfig.WithMessage("mqtt: ephemeral session cannot require managed subscription history")
		}
		if spec.ManagedSubscriptionStore == nil || spec.ManagedSubscriptionIdentity == "" {
			return nil, shared.ErrInvalidConfig.WithMessage("mqtt: durable managed subscription store and secret-safe identity are required")
		}
	}
	session := NewSession(opts, mode, f.Logger, f.Metrics)
	session.managedStore = spec.ManagedSubscriptionStore
	session.managedIdentity = spec.ManagedSubscriptionIdentity
	session.managedRequired = spec.ManagedSubscriptionsRequired
	return session, nil
}

// NewReceiver creates the sole MQTT Receiver bound to the given Session. Its
// subscription topics become router filters; a second factory-created receiver
// is rejected because dispatch and protocol settlement are session-wide.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("mqtt receiver %q: session must be a non-nil MQTT session", spec.ID))
	}
	filters := make([]string, 0, len(spec.Subscriptions))
	for _, sub := range spec.Subscriptions {
		// Validate EVERY declared filter and QoS, not just the first non-empty
		// topic, and reject rather than skip an empty one: the session plan is
		// built from the SAME subscription list, so a topic this seam quietly
		// dropped would still be sent to the broker at reconcile. A subscription
		// reaches the broker only when the session manager reconciles, so an
		// unvalidated one fails after the process has already started serving —
		// and an out-of-range QoS does not fail at all, it is masked into a
		// weaker delivery guarantee.
		if err := ValidateMQTTSubscription(sub.Topic, sub.QoS); err != nil {
			return nil, shared.ErrInvalidConfig.Wrap(err).WithMessage(
				fmt.Sprintf("mqtt receiver %q: invalid subscription", spec.ID))
		}
		filters = append(filters, sub.Topic)
	}
	// A receiver with ZERO subscription topics is a configuration error, not
	// an implicit match-all. The router treats an empty filter set as
	// "match every topic" (matchesAnyFilter, topic_match.go), so a no-topic
	// receiver on a shared session would receive every publish, participate
	// in ACK splitting, and defeat orphan cleanup — flooding the route with
	// unintended traffic. Reject it here at the config-driven factory seam;
	// the direct NewReceiver constructor keeps the
	// match-all default for tests/diagnostic taps.
	if len(filters) == 0 {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("mqtt receiver %q: at least one subscription topic is required "+
				"(a receiver with no topics would subscribe to everything)", spec.ID))
	}
	// Reserve only after every fallible spec check. The reservation is stored on
	// the Session and protected by its mutex, so aliases and concurrent factory
	// callers cannot bypass the one-ingress-receiver contract.
	if err := mqttSession.reserveDedicatedIngressReceiver(spec.ID); err != nil {
		return nil, err
	}
	return NewReceiver(spec.ID, mqttSession, WithTopicFilters(filters...)), nil
}

// NewSender creates an MQTT Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("mqtt sender %q: session must be a non-nil MQTT session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("mqtt sender %q: %s", spec.ID, err))
	}
	opts := cfg.Sender
	if opts.QoS > 2 {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("mqtt sender %q: qos must be 0, 1, or 2", spec.ID))
	}
	// QoS is honoured as-is: the registry decode path (register.go)
	// pre-fills the default (1) so an omitted key arrives here as 1,
	// while an EXPLICIT `qos: 0` arrives as 0 and stays 0. Coercing
	// 0 → 1 here would make the documented at-most-once setting
	// unreachable.
	if opts.Timeout == 0 {
		opts.Timeout = DefaultSenderOptions().Timeout
	}
	if opts.ThrottleRetryAfter == 0 {
		opts.ThrottleRetryAfter = DefaultSenderOptions().ThrottleRetryAfter
	}
	// Validate a configured default_topic as an MQTT PUBLISH topic. It is
	// used verbatim as the publish topic when an outbound message carries no
	// Address (sender.go), bypassing the runtime AddressValidator that guards
	// resolved addresses. A wildcard, $-reserved, or otherwise malformed
	// default_topic would therefore only fail at first publish — as a broker
	// DISCONNECT (publishing to a wildcard is an MQTT protocol error) that tears
	// down the shared session for every sender on it. Fail closed at build time
	// instead. Empty means "no fallback" and is validated at Send.
	if opts.DefaultTopic != "" {
		if err := ValidateMQTTTopic(opts.DefaultTopic); err != nil {
			return nil, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("mqtt sender %q: default_topic %q is not a valid MQTT publish topic: %s",
					spec.ID, opts.DefaultTopic, err))
		}
	}
	return NewSender(mqttSession, opts), nil
}

// configFromSpec accepts both *Config and Config so registry-decoded
// pointers and hand-built test value fixtures both work.
func configFromSpec(pc ports.PluginConfig) (Config, error) {
	if pc == nil {
		return Config{}, errInvalidConfig
	}
	switch v := pc.(type) {
	case *Config:
		if v == nil {
			return Config{}, errInvalidConfig
		}
		return *v, nil
	case Config:
		return v, nil
	default:
		return Config{}, errInvalidConfig
	}
}
