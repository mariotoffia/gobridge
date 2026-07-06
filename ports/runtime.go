package ports

import (
	"context"
	"strings"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// RouteInfo describes a registered route for introspection. It is the
// read-side projection driving adapters (HTTP admin/monitor, CLI,
// future gRPC) consume via RuntimeQuery.Routes.
//
// The type lives in the ports package — not the runtime package —
// because driving adapters depend on ports, never on the concrete
// runtime. Keeping the projection here preserves the Dependency Rule:
// the inner ring describes what crosses the boundary; the runtime
// package merely populates it.
type RouteInfo struct {
	ID           string
	DeliveryMode routing.DeliveryMode
	DispatchMode routing.DispatchMode
	Policy       routing.RoutePolicy
}

// RouteHealth describes one route's runtime health snapshot. Part of
// the read-side contract surfaced through RuntimeQuery.DeepHealth.
type RouteHealth struct {
	ID           string
	DeliveryMode string
	Ready        bool // route runner started + receiver ready
	InFlight     int  // currently-processing delivery count
}

// SessionHealthDetail describes one session's health, including
// subscription readiness and lease ownership. Part of the read-side
// contract surfaced through RuntimeQuery.DeepHealth.
type SessionHealthDetail struct {
	SessionID           string
	Connected           bool
	HasLease            bool
	ConnectAfterLease   bool // source session defers Start until this instance wins the lease
	SubscriptionsWanted int
	SubscriptionsActive int
	ActiveTopics        []string // topics the broker has ACK'd subscriptions for
	Ready               bool
	ServiceLevel        ServiceLevel
}

// DeepHealth is a comprehensive health snapshot of a runtime instance.
// It is the read-side projection driving adapters use to render
// /deephealth probes, dashboards, or readiness gates.
type DeepHealth struct {
	Running         bool
	Healthy         bool
	InstanceID      string
	Role            string
	Routes          []RouteHealth
	Sessions        []SessionHealthDetail
	ReadyForTraffic bool         // All sessions connected + runtime healthy
	ServiceLevel    ServiceLevel // Minimum service level across all sessions
}

// ReadinessLevel describes the highest operational level the runtime
// has currently achieved. Levels are strictly ordered: each level
// implies all lower levels are satisfied.
//
// Operators choose the level appropriate for their probe:
//   - K8s liveness: LevelLive (always — the process answering is enough)
//   - K8s readiness: LevelConnected or LevelSubscribed (accepts intermittent broker hiccups)
//   - Pre-traffic gate: LevelFull (every route handler registered, ready to dispatch)
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
// reports whether the input is recognised. Used by driving adapters
// (HTTP handlers, CLI flags) that accept a readiness level argument.
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

// RuntimeQuery is the read-side driving port for the bridge runtime.
// Driving adapters (HTTP monitor / admin, CLI, future gRPC) depend on
// this interface, not on a concrete runtime type. Methods are
// side-effect free and safe for concurrent invocation.
//
// The interface is intentionally narrow: it exposes only what driving
// adapters need to render status, health, readiness probes, and route
// inventories. Mutating operations live on RuntimeCommand.
type RuntimeQuery interface { //nolint:interfacebloat // RuntimeQuery is a deliberately wide read-side facade that mirrors the bridge runtime's read surface (status, health, readiness, route inventory, DLQ read accessor). Splitting it into smaller interfaces would force every driving adapter (HTTP monitor, future CLI, future gRPC) to depend on multiple ports for a single status page, defeating the purpose of having a coherent driving-port contract.
	// InstanceID returns the bridge instance identifier.
	InstanceID() string
	// IsRunning reports whether the runtime is currently running and
	// every critical background component is healthy.
	IsRunning() bool
	// Healthy reports whether all background components are healthy
	// (independent of running state).
	Healthy() bool
	// Terminal reports whether the runtime has suffered an unrecoverable
	// failure and cancelled itself. A terminal runtime never recovers on
	// its own; the process must be restarted. Liveness probes fail closed
	// on this so the orchestrator restarts a dead-but-running instance.
	Terminal() bool
	// ComponentErrors returns a copy of the failed-component error map.
	// Empty map means all components are healthy.
	ComponentErrors() map[string]error
	// Role returns the operational role of this instance based on
	// lease ownership: "active", "standby", or "standalone".
	Role() string
	// Routes returns information about all registered routes.
	Routes() []RouteInfo
	// DeepHealth returns a comprehensive health snapshot including
	// session subscription readiness and lease status.
	DeepHealth(ctx context.Context) DeepHealth
	// ReadinessLevel returns the highest level the runtime has
	// currently achieved. Computed from a single DeepHealth snapshot
	// so the result is internally consistent.
	ReadinessLevel(ctx context.Context) ReadinessLevel
	// DLQReader returns the configured DLQ read port, or nil when no
	// DLQ is wired (standalone / non-DLQ deployments). The read port
	// exposes only Get/List; the destructive dead-letter operations
	// live on the write-side RuntimeCommand.DLQAdmin.
	DLQReader() DLQReader
}

// RuntimeCommand is the write-side driving port for the bridge runtime.
// Driving adapters that mutate runtime state (admin start/stop,
// programmatic Inject) depend on this interface.
//
// All methods accept a deadline-bearing context so the calling adapter
// can cap operation latency (e.g. HTTP admin's per-operation timeout).
type RuntimeCommand interface {
	// Start brings the runtime up: starts background components,
	// session managers, route runners. Returns an error if already
	// running or if any component fails to start.
	Start(ctx context.Context) error
	// Stop gracefully shuts the runtime down. Safe to call when not
	// running (no-op).
	Stop(ctx context.Context) error
	// Inject sends a synthetic envelope through the named route's
	// delivery pipeline. Returns shared.ErrNotFound when the route
	// does not exist.
	Inject(ctx context.Context, routeID string, env *messaging.Envelope) error
	// DLQAdmin returns the configured DLQ admin port, or nil when no
	// DLQ is wired. It carries the destructive dead-letter operations
	// (write, delete, delete-by-filter, purge) kept off the read port.
	DLQAdmin() DLQAdmin
}

// Runtime aggregates the read- and write-side runtime ports for
// driving adapters that need both. Most adapters should depend on
// RuntimeQuery or RuntimeCommand individually (interface segregation);
// the combined interface exists so a single composition-root provider
// can satisfy both halves at once.
type Runtime interface {
	RuntimeQuery
	RuntimeCommand
}
