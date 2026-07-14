package bridge

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// validatePostAcquireActivationTimings rejects an exclusive transport timing
// profile before stores or transports are built when one configured phase cannot
// fit inside LeaseTTL minus the manager's reserved teardown margin. The live
// manager still enforces one aggregate deadline across the complete sequence.
func (b *Builder) validatePostAcquireActivationTimings() error {
	if b == nil || b.cfg == nil {
		return nil
	}
	validated := make(map[string]struct{})
	for _, route := range b.cfg.Routes {
		if route.Session != nil {
			sessionID := route.Session.SessionID
			if _, seen := validated[sessionID]; !seen {
				sc, err := toSessionConfigE(route.Session, deploymentClustered(b.cfg))
				if err != nil {
					return fmt.Errorf("bridge: route %q: %w", route.ID, err)
				}
				if err := validateSessionActivationTiming(route.ID, sessionID, sc, findSession(b.cfg, sessionID)); err != nil {
					return err
				}
				validated[sessionID] = struct{}{}
			}
		}

		// Binding-only session managers are also exclusive and always connect
		// after lease acquisition. Mirror wireRoutes' first-registration order.
		for _, bindingID := range route.Bindings {
			binding := findBinding(b.cfg, bindingID)
			if binding == nil || binding.SessionID == "" {
				continue
			}
			if _, seen := validated[binding.SessionID]; seen {
				continue
			}
			sc, err := b.bindingSessionConfig(route, binding.SessionID)
			if err != nil {
				return fmt.Errorf("bridge: route %q: binding %q session config: %w", route.ID, bindingID, err)
			}
			sc.ConnectAfterLease = true
			if err := validateSessionActivationTiming(route.ID, binding.SessionID, &sc, findSession(b.cfg, binding.SessionID)); err != nil {
				return err
			}
			validated[binding.SessionID] = struct{}{}
		}
	}
	return nil
}

func validateSessionActivationTiming(routeID, sessionID string, sc *session.Config, def *ports.SessionDef) error {
	if sc == nil || def == nil || ports.IsNilPluginConfig(def.Config) {
		return nil
	}
	timingConfig, ok := def.Config.(ports.PostAcquireActivationTimingConfig)
	if !ok {
		return nil
	}
	mode := connectivity.SessionMode(def.SessionMode)
	if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	timing := timingConfig.PostAcquireActivationTiming(mode)
	if !sc.ConnectAfterLease {
		timing.ConnectTimeout = 0
	}
	budget, teardownMargin := sc.PostAcquireActivationBudget()
	phases := []struct {
		name     string
		duration time.Duration
	}{
		{name: "connect_timeout", duration: timing.ConnectTimeout},
		{name: "reconnect_timeout", duration: timing.ReconnectTimeout},
		{name: "reconcile_timeout", duration: timing.ReconcileTimeout},
		{name: "replay_grace", duration: timing.ReplayGrace},
	}
	for _, phase := range phases {
		if phase.duration <= budget {
			continue
		}
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"bridge: route %q: session %q: configured %s=%s exceeds lease-safe post-acquire activation budget=%s "+
				"(LeaseTTL=%s - teardown_margin=%s)",
			routeID, sessionID, phase.name, phase.duration, budget, sc.LeaseTTL, teardownMargin,
		))
	}
	return nil
}
