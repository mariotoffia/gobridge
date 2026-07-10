package paho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
		// MQTT supports shared subscriptions ($share/<group>/<filter>): the
		// broker load-balances a topic's deliveries across the group members,
		// so multiple bridge instances can scale-out consumption of one
		// logical subscription. topic_match.go strips the $share prefix for
		// dispatch; advertise the capability so the runtime/operators can
		// discover the scale-out path.
		ports.CapSharedConsumer,
		// MQTT receivers subscribe ONLY when the session manager reconciles the
		// SessionPlan, so a receiver on an unmanaged session is silently inert;
		// the builder enforces a manager for these (ADV-P4-FU1).
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
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: %s", spec.ID, err))
	}
	opts := cfg.Session
	if opts.ClientID == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: client_id is required", spec.ID))
	}
	if len(opts.BrokerURLs) == 0 {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt session %q: at least one broker URL is required", spec.ID))
	}
	// HIGH-4: refuse to build a session that would ship username/password in
	// cleartext over a non-TLS broker unless allow_plaintext_credentials is
	// set. This is the build-time enforcement boundary — broker_urls are
	// populated here, so the gate is always active for a real session.
	if err := opts.validatePlaintextCredentials(); err != nil {
		return nil, shared.ErrInvalidPayload.Wrap(err).WithMessage(
			fmt.Sprintf("mqtt session %q: insecure credential transport", spec.ID))
	}
	if err := opts.Will.Validate(); err != nil {
		return nil, shared.ErrInvalidPayload.Wrap(err).WithMessage(
			fmt.Sprintf("mqtt session %q: invalid will configuration", spec.ID))
	}
	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// NewReceiver creates an MQTT Receiver bound to the given Session. The
// receiver's subscription topics become its router topic filters, so a
// shared session dispatches each publish only to the receivers whose
// filters cover it.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt receiver %q: session must be a non-nil MQTT session", spec.ID))
	}
	filters := make([]string, 0, len(spec.Subscriptions))
	for _, sub := range spec.Subscriptions {
		if sub.Topic != "" {
			filters = append(filters, sub.Topic)
		}
	}
	// A receiver with ZERO subscription topics is a configuration error, not
	// an implicit match-all. The router treats an empty filter set as
	// "match every topic" (matchesAnyFilter, topic_match.go), so a no-topic
	// receiver on a shared session would receive every publish, participate
	// in ACK splitting, and defeat orphan cleanup — flooding the route with
	// unintended traffic. Reject it here at the config-driven factory seam
	// (c4-notopic-matchall); the direct NewReceiver constructor keeps the
	// match-all default for tests/diagnostic taps.
	if len(filters) == 0 {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt receiver %q: at least one subscription topic is required "+
				"(a receiver with no topics would subscribe to everything)", spec.ID))
	}
	return NewReceiver(spec.ID, mqttSession, WithTopicFilters(filters...)), nil
}

// NewSender creates an MQTT Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	mqttSession, ok := session.(*Session)
	if !ok || mqttSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: session must be a non-nil MQTT session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("mqtt sender %q: %s", spec.ID, err))
	}
	opts := cfg.Sender
	if opts.QoS > 2 {
		return nil, shared.ErrInvalidPayload.WithMessage(
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
