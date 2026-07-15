package bridge

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// validatePostAcquireActivationTimings rejects unsafe production lease floors
// and transport activation bounds before stores or transports are built. The
// live manager renews the lease throughout the bounded activation sequence.
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
				if err := configureSessionActivationTiming(route.ID, sessionID, sc, findSession(b.cfg, sessionID)); err != nil {
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
			if err := configureSessionActivationTiming(route.ID, binding.SessionID, &sc, findSession(b.cfg, binding.SessionID)); err != nil {
				return err
			}
			validated[binding.SessionID] = struct{}{}
		}
	}
	return nil
}

// configureSessionActivationTiming applies the transport's conservative bound
// to the runtime manager config. A direct caller may pin a larger hard bound;
// pinning one below the transport's effective worst case is rejected.
func configureSessionActivationTiming(routeID, sessionID string, sc *session.Config, def *ports.SessionDef) error {
	if sc == nil {
		return nil
	}
	ttl := sc.EffectiveLeaseTTL()
	if sc.Exclusive && ttl < session.MinimumProductionLeaseTTL {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"bridge: route %q: session %q: effective lease_ttl=%s is below production minimum=%s",
			routeID, sessionID, ttl, session.MinimumProductionLeaseTTL,
		))
	}
	if def == nil || ports.IsNilPluginConfig(def.Config) {
		return nil
	}
	timingConfig, ok := def.Config.(ports.PostAcquireActivationTimingConfig)
	if !ok {
		return nil
	}
	mode := connectivity.SessionMode(def.SessionMode)
	if sc.Exclusive {
		mode = connectivity.SessionExclusive
	} else if mode == "" {
		mode = connectivity.SessionEphemeral
	}
	worst := timingConfig.PostAcquireActivationTiming(mode).WorstCaseDuration
	if worst <= 0 {
		return nil
	}
	if sc.PostAcquireActivationTimeout == 0 {
		sc.PostAcquireActivationTimeout = worst
	}
	if sc.PostAcquireActivationTimeout < worst {
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"bridge: route %q: session %q: conservative post-acquire activation duration=%s exceeds configured post-acquire activation timeout=%s",
			routeID, sessionID, worst, sc.PostAcquireActivationTimeout,
		))
	}
	return nil
}
