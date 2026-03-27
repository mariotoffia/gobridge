package bridge

import (
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

func toRoutePolicy(r config.RouteDef) domain.RoutePolicy {
	p := domain.RoutePolicy{
		DeliveryMode:       domain.DeliveryMode(r.DeliveryMode),
		DispatchMode:       domain.DispatchMode(r.DispatchMode),
		MaxInFlight:        r.Policy.MaxInFlight,
		MaxReplayAttempts:  r.Policy.MaxReplayAttempts,
		MaxOutboxDepth:     r.Policy.MaxOutboxDepth,
		AckAfter:           domain.AckBoundary(r.Policy.AckAfter),
		OnExpired:          domain.ExpiredAction(r.Policy.OnExpired),
		OnPermanentFailure: domain.FailureAction(r.Policy.OnPermanentFailure),
	}
	if r.Policy.Backoff.InitialInterval != "" {
		p.Backoff.InitialInterval, _ = time.ParseDuration(r.Policy.Backoff.InitialInterval)
	}
	if r.Policy.Backoff.MaxInterval != "" {
		p.Backoff.MaxInterval, _ = time.ParseDuration(r.Policy.Backoff.MaxInterval)
	}
	if r.Policy.Backoff.Multiplier != 0 {
		p.Backoff.Multiplier = r.Policy.Backoff.Multiplier
	}
	return p
}

func toSessionConfig(rs *config.RouteSessionDef) *runtime.SessionConfig {
	if rs == nil {
		return nil
	}

	sc := runtime.DefaultSessionConfig(rs.SessionID, true)
	sc.ConnectAfterLease = rs.ConnectAfterLease

	if rs.LeaseTTL != "" {
		if d, err := time.ParseDuration(rs.LeaseTTL); err == nil {
			sc.LeaseTTL = d
		}
	}
	if rs.RenewInterval != "" {
		if d, err := time.ParseDuration(rs.RenewInterval); err == nil {
			sc.RenewInterval = d
		}
	}
	if rs.MaxRenewFails > 0 {
		sc.MaxRenewFails = rs.MaxRenewFails
	}
	if rs.StepDownGrace != "" {
		if d, err := time.ParseDuration(rs.StepDownGrace); err == nil {
			sc.StepDownGrace = d
		}
	}
	sc.DrainStrategy = toDrainStrategy(rs)
	if rs.DrainBatchSize > 0 {
		sc.DrainBatchSize = rs.DrainBatchSize
	}

	return &sc
}

func toDrainStrategy(rs *config.RouteSessionDef) domain.DrainStrategy {
	if rs.DrainStrategy != nil {
		return buildDrainStrategy(rs.DrainStrategy)
	}
	if rs.DrainInterval != "" {
		if d, err := time.ParseDuration(rs.DrainInterval); err == nil {
			return domain.NewFixedPoll(d)
		}
	}
	return domain.NewFixedPoll(domain.DefaultFixedPollInterval)
}

func buildDrainStrategy(ds *config.DrainStrategyDef) domain.DrainStrategy {
	switch ds.Type {
	case "adaptive_backoff":
		var minD, maxD time.Duration
		if ds.MinInterval != "" {
			minD, _ = time.ParseDuration(ds.MinInterval)
		}
		if ds.MaxInterval != "" {
			maxD, _ = time.ParseDuration(ds.MaxInterval)
		}
		return domain.NewAdaptiveBackoff(minD, maxD, ds.Multiplier)

	default:
		var interval time.Duration
		if ds.Interval != "" {
			interval, _ = time.ParseDuration(ds.Interval)
		}
		return domain.NewFixedPoll(interval)
	}
}

func toBindings(cfg *config.BridgeConfig, bindingIDs []string) []domain.DestinationBinding {
	out := make([]domain.DestinationBinding, 0, len(bindingIDs))
	for _, id := range bindingIDs {
		bd := findBinding(cfg, id)
		if bd == nil {
			continue
		}
		out = append(out, domain.DestinationBinding{
			ID:        bd.ID,
			SessionID: bd.SessionID,
			SenderID:  bd.SenderID,
			Address:   bd.Address,
			Options:   bd.Options,
		})
	}
	return out
}

//nolint:unused // scaffolded for T13 config-driven construction
func toSessionSpec(s config.SessionDef) ports.SessionSpec {
	return ports.SessionSpec{
		ID:          s.ID,
		Transport:   s.Transport,
		SessionMode: domain.SessionMode(s.SessionMode),
		Options:     s.Options,
	}
}

//nolint:unused // scaffolded for T13 config-driven construction
func toReceiverSpec(r config.ReceiverDef) ports.ReceiverSpec {
	spec := ports.ReceiverSpec{
		ID:        r.ID,
		SessionID: r.SessionID,
		Options:   r.Options,
	}
	for _, t := range r.Topics {
		spec.Subscriptions = append(spec.Subscriptions, domain.SubscriptionPlan{
			Topic:   t.Topic,
			QoS:     t.QoS,
			Options: t.Options,
		})
	}
	return spec
}

//nolint:unused // scaffolded for T13 config-driven construction
func toSenderSpec(s config.SenderDef) ports.SenderSpec {
	return ports.SenderSpec{
		ID:        s.ID,
		SessionID: s.SessionID,
		Options:   s.Options,
	}
}

func findBinding(cfg *config.BridgeConfig, id string) *config.BindingDef {
	for i := range cfg.Bindings {
		if cfg.Bindings[i].ID == id {
			return &cfg.Bindings[i]
		}
	}
	return nil
}

func findSession(cfg *config.BridgeConfig, id string) *config.SessionDef {
	for i := range cfg.Sessions {
		if cfg.Sessions[i].ID == id {
			return &cfg.Sessions[i]
		}
	}
	return nil
}

func findReceiver(cfg *config.BridgeConfig, id string) *config.ReceiverDef {
	for i := range cfg.Receivers {
		if cfg.Receivers[i].ID == id {
			return &cfg.Receivers[i]
		}
	}
	return nil
}

//nolint:unused // scaffolded for T13 config-driven construction
func findSender(cfg *config.BridgeConfig, id string) *config.SenderDef {
	for i := range cfg.Senders {
		if cfg.Senders[i].ID == id {
			return &cfg.Senders[i]
		}
	}
	return nil
}
