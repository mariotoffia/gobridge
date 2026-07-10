package amqp10

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Factory implements ports.TransportFactory for AMQP 1.0.
type Factory struct {
	Logger  *slog.Logger
	Metrics ports.MetricsExporter
}

var _ ports.TransportFactory = (*Factory)(nil)

var errInvalidConfig = errors.New("amqp10: spec.Config must be of type amqp10.Config")

// NewFactory creates an AMQP 1.0 transport factory.
func NewFactory(logger *slog.Logger, metrics ...ports.MetricsExporter) *Factory {
	var m ports.MetricsExporter
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &Factory{Logger: logger, Metrics: m}
}

// Capabilities returns the transport capabilities for AMQP 1.0.
//
// CapSourceRedelivery is declared because an AMQP 1.0 broker fully
// redelivers unsettled deliveries: releasing (Retry) or simply losing
// the link before settlement puts the message back on the source queue.
// Without this declaration the route validator treats amqp10 as a
// no-retry transport and forces a DLQ store or AllowRetryDrop even
// though the transport never drops.
func (f *Factory) Capabilities() []ports.Capability {
	return []ports.Capability{
		ports.CapStatefulSession,
		ports.CapSourceRedelivery,
	}
}

// AddressValidator returns nil — AMQP 1.0 link target addresses have
// no runtime-enforceable rendered-address rules; the broker rejects
// invalid addresses on attach.
func (f *Factory) AddressValidator() ports.AddressValidator { return nil }

// NewSession creates an AMQP 1.0 Session from the given spec.
func (f *Factory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 session %q: %s", spec.ID, err))
	}
	opts := cfg.Session
	opts.applyDefaults()
	// Build boundary: credentials_uri has already been resolved (and
	// CredentialsURIRef cleared) by ApplyCredentials, so validate strictly
	// — any SASL EXTERNAL cert material must be present by now.
	if err := opts.validate(false); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 session %q: %s", spec.ID, err))
	}
	return NewSession(opts, spec.SessionMode, f.Logger, f.Metrics), nil
}

// NewReceiver creates an AMQP 1.0 Receiver bound to the given Session.
func (f *Factory) NewReceiver(_ context.Context, spec ports.ReceiverSpec, session ports.Session) (ports.Receiver, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: %s", spec.ID, err))
	}
	// AMQP 1.0 receiver links are address-bound; enforced here — the
	// build boundary — because parse-time Validate also runs on binding
	// overrides that legitimately omit the address.
	if cfg.Receiver.Address == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: receiver.address is required", spec.ID))
	}
	rc := ReceiverConfig{
		Address:          cfg.Receiver.Address,
		LinkCredit:       cfg.Receiver.LinkCredit,
		DurabilityMode:   cfg.Receiver.DurabilityMode,
		Routing:          cfg.Receiver.Routing,
		SubscriptionName: cfg.Receiver.SubscriptionName,
		Logger:           f.Logger,
		Metrics:          f.Metrics,
		Session:          amqpSession,
	}
	if rc.DurabilityMode > 0 {
		// HIGH-1: a durable subscription's broker identity is
		// container-id + link name. An auto-generated container-id is
		// stable across reconnects but CHANGES on process restart, so the
		// broker sees a NEW subscription after a restart and orphans every
		// message retained for the old identity — durable mode looks
		// configured but is not restart-safe. Fail closed unless an
		// explicit, stable container_id is set. A stable subscription_name
		// alone does not help: the container-id is part of the identity, so
		// a regenerated one changes the subscription regardless.
		if amqpSession.opts.containerIDGenerated {
			return nil, shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
				"amqp10 receiver %q: durability_mode=%d requires an explicit session.container_id — "+
					"a durable subscription is keyed by container-id + link name, and a generated "+
					"container-id changes across process restarts, orphaning the subscription and "+
					"missing every message published while the bridge was down", spec.ID, rc.DurabilityMode))
		}
	}
	// Construct and VALIDATE the receiver before reserving the link: a
	// config error (e.g. link_credit overflow in ReceiverConfig.validate)
	// must not leak a link reservation onto the session, which would then
	// falsely trip the dedicated-session gate for a LATER durable receiver
	// on the same session. Reserve only once construction has succeeded so
	// the reservation count stays exact (review-3).
	recv, err := NewReceiver(rc, amqpSession)
	if err != nil {
		return nil, err
	}
	// HIGH-3: enforce the dedicated-session contract at build time so a
	// durable receiver's session-wide teardown blast radius cannot reach
	// unrelated sibling links.
	if err := amqpSession.reserveLink(rc.DurabilityMode > 0); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 receiver %q: %s", spec.ID, err))
	}
	return recv, nil
}

// NewSender creates an AMQP 1.0 Sender bound to the given Session.
func (f *Factory) NewSender(_ context.Context, spec ports.SenderSpec, session ports.Session) (ports.Sender, error) {
	amqpSession, ok := session.(*Session)
	if !ok || amqpSession == nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: session must be a non-nil AMQP 1.0 session", spec.ID))
	}
	cfg, err := configFromSpec(spec.Config)
	if err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: %s", spec.ID, err))
	}
	// Sender links are address-bound; same build-boundary enforcement
	// as NewReceiver.
	if cfg.Sender.Address == "" {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: sender.address is required", spec.ID))
	}
	sc := SenderConfig{
		Address:        cfg.Sender.Address,
		Timeout:        cfg.Sender.Timeout,
		DurabilityMode: cfg.Sender.DurabilityMode,
		Routing:        cfg.Sender.Routing,
		Durable:        cfg.Sender.Durable,
		Logger:         f.Logger,
		Metrics:        f.Metrics,
		Session:        amqpSession,
	}
	if sc.Timeout == 0 {
		sc.Timeout = DefaultSenderOptions().Timeout
	}
	// Construct and VALIDATE before reserving so a config error cannot leak
	// a reservation onto the session (review-3, symmetric with NewReceiver).
	snd, err := NewSender(sc, amqpSession)
	if err != nil {
		return nil, err
	}
	// HIGH-3: a sender may not share a session with a durable receiver —
	// the durable receiver's close forces a full connection teardown that
	// would move this sender's in-flight publishes into unknown/duplicate
	// territory. Reserve the link so a shared-session topology fails closed
	// at build time (dedicated-session contract).
	if err := amqpSession.reserveLink(false); err != nil {
		return nil, shared.ErrInvalidPayload.WithMessage(
			fmt.Sprintf("amqp10 sender %q: %s", spec.ID, err))
	}
	return snd, nil
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
