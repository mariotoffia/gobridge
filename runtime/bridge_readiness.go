package runtime

import (
	"context"
	"strings"
)

// ReadinessLevel describes the highest operational level the runtime
// has currently achieved. Levels are strictly ordered: each level
// implies all lower levels are satisfied.
//
// Operators choose the level appropriate for their probe:
//   - K8s liveness: Live (always — the process answering is enough)
//   - K8s readiness: Connected or Subscribed (accepts intermittent broker hiccups)
//   - Pre-traffic gate: Full (every route handler registered, ready to dispatch)
type ReadinessLevel int

const (
	// LevelDown means the runtime is not running or has failed.
	LevelDown ReadinessLevel = iota
	// LevelLive means the process is alive and serving HTTP. Always
	// achievable when this method is called from inside the process.
	LevelLive
	// LevelRunning means rt.Start was called, the runtime is healthy,
	// and the supervisor reports it is the active runtime instance.
	LevelRunning
	// LevelConnected means LevelRunning plus every registered session
	// has SessionHealth.Connected == true. Per-session reconnect storms
	// drop us below this until the session reconnects.
	LevelConnected
	// LevelSubscribed means LevelConnected plus every session has
	// SubscriptionsActive == SubscriptionsWanted (broker has ACKed
	// every SUBSCRIBE). Routes can safely register handlers at this
	// point without missing messages from subsequent publishes.
	LevelSubscribed
	// LevelFull means LevelSubscribed plus every route has Ready == true
	// (route runner started AND receiver started AND, for MQTT,
	// HandlersRegistered > 0). Equivalent to ReadyForTraffic + ServiceLevelFull.
	LevelFull
)

// String returns the lowercase, human-readable name of the level.
// Stable for inclusion in JSON responses and structured log fields.
func (l ReadinessLevel) String() string {
	switch l {
	case LevelLive:
		return "live"
	case LevelRunning:
		return "running"
	case LevelConnected:
		return "connected"
	case LevelSubscribed:
		return "subscribed"
	case LevelFull:
		return "full"
	default:
		return "down"
	}
}

// ParseReadinessLevel parses a level string (case-insensitive) and
// reports whether the input is recognised. Used by HTTP handlers that
// accept a ?level= query parameter.
func ParseReadinessLevel(s string) (ReadinessLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "live":
		return LevelLive, true
	case "running":
		return LevelRunning, true
	case "connected":
		return LevelConnected, true
	case "subscribed":
		return LevelSubscribed, true
	case "full":
		return LevelFull, true
	default:
		return LevelDown, false
	}
}

// ReadinessLevel returns the highest level the runtime has currently
// achieved. Computed from a single DeepHealth snapshot so the result
// is internally consistent.
//
// Note: this is a snapshot — the level may change between this call and
// the next call as sessions reconnect / subscriptions complete.
func (rt *Runtime) ReadinessLevel(ctx context.Context) ReadinessLevel {
	if rt == nil {
		return LevelDown
	}
	if !rt.IsRunning() || !rt.Healthy() {
		// Process is alive (we are answering) but the bridge is not running.
		return LevelLive
	}
	dh := rt.DeepHealth(ctx)
	if !dh.Running || !dh.Healthy {
		return LevelLive
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
		return LevelRunning
	}
	if !allSubscribed {
		return LevelConnected
	}

	// Full requires every route runner to be Ready (handler registered).
	for _, rh := range dh.Routes {
		if !rh.Ready {
			return LevelSubscribed
		}
	}
	return LevelFull
}

// AtLeast reports whether the runtime has achieved at least the
// requested level.
func (rt *Runtime) AtLeast(ctx context.Context, want ReadinessLevel) bool {
	return rt.ReadinessLevel(ctx) >= want
}
