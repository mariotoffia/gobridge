package runtime

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
)

// ReadinessLevel and its constants are defined in the ports package so
// driving adapters depend on the inner-ring contract; the aliases
// below preserve the runtime.LevelX spelling at existing call sites
// (tests, helpers) without forcing an import-site rename.
type ReadinessLevel = ports.ReadinessLevel

const (
	LevelDown       = ports.LevelDown
	LevelLive       = ports.LevelLive
	LevelRunning    = ports.LevelRunning
	LevelConnected  = ports.LevelConnected
	LevelSubscribed = ports.LevelSubscribed
	LevelFull       = ports.LevelFull
)

// ParseReadinessLevel is re-exported from ports so existing callers
// (HTTP handlers, CLI flags) keep compiling. New code should call
// ports.ParseReadinessLevel directly.
func ParseReadinessLevel(s string) (ReadinessLevel, bool) {
	return ports.ParseReadinessLevel(s)
}

// ReadinessLevel returns the highest level the runtime has currently
// achieved. Computed from a single DeepHealth snapshot so the result
// is internally consistent.
//
// Note: this is a snapshot — the level may change between this call and
// the next call as sessions reconnect / subscriptions complete.
func (rt *Runtime) ReadinessLevel(ctx context.Context) ports.ReadinessLevel {
	if rt == nil {
		return ports.LevelDown
	}
	if !rt.IsRunning() || !rt.Healthy() {
		// Process is alive (we are answering) but the bridge is not running.
		return ports.LevelLive
	}
	dh := rt.DeepHealth(ctx)
	if !dh.Running || !dh.Healthy {
		return ports.LevelLive
	}

	// Sender-only sessions report Connected without subscriptions; track
	// the strongest level all sessions satisfy.
	allConnected := true
	allSubscribed := true
	for _, sh := range dh.Sessions {
		if !sh.Connected {
			allConnected = false
			allSubscribed = false
			break
		}
		if sh.SubscriptionsActive != sh.SubscriptionsWanted {
			allSubscribed = false
		}
	}
	if !allConnected {
		return ports.LevelRunning
	}
	if !allSubscribed {
		return ports.LevelConnected
	}

	// Full requires every route runner to be Ready (handler registered).
	for _, rh := range dh.Routes {
		if !rh.Ready {
			return ports.LevelSubscribed
		}
	}
	return ports.LevelFull
}

// AtLeast reports whether the runtime has achieved at least the
// requested level.
func (rt *Runtime) AtLeast(ctx context.Context, want ports.ReadinessLevel) bool {
	return rt.ReadinessLevel(ctx) >= want
}
