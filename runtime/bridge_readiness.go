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
// The level is Role-aware. A STANDBY instance (exclusive
// sessions configured but none currently holding a lease) is not dispatching
// traffic for its exclusive routes, so it MUST NOT advertise LevelFull — the
// pre-traffic gate and failover routers treat LevelFull as "route real traffic
// here". Reporting Full from a standby with merely-connected sessions would
// mislead them into steering traffic at an instance that is not serving. A
// ready standby is therefore capped at LevelSubscribed (connected + subscribed,
// but not the primary) — a distinct "ready-standby" state below Full. The
// operational role remains explicitly observable via DeepHealth.Role / Role().
//
// Note: this is a snapshot — the level may change between this call and
// the next call as sessions reconnect / subscriptions complete, or as this
// instance wins/loses a lease and transitions between standby and active.
func (rt *Runtime) ReadinessLevel(ctx context.Context) ports.ReadinessLevel {
	if rt == nil {
		return ports.LevelDown
	}
	if !rt.IsRunning() || !rt.Healthy() {
		// Process is alive (we are answering) but the bridge is not running.
		return ports.LevelLive
	}
	// Delegate the snapshot→level derivation (including the standby cap) to the
	// pure ports.ReadinessLevelFromDeepHealth so /ready and /deephealth classify
	// identical runtime state identically — one algorithm, no drift.
	return ports.ReadinessLevelFromDeepHealth(rt.DeepHealth(ctx))
}

// AtLeast reports whether the runtime has achieved at least the
// requested level.
func (rt *Runtime) AtLeast(ctx context.Context, want ports.ReadinessLevel) bool {
	return rt.ReadinessLevel(ctx) >= want
}
