package bootstrap

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// errRuntimeWedged is the component error reported by the terminalRuntime
// sentinel. It exists so authenticated monitor endpoints (deep health,
// component errors) surface a clear "process must be restarted" reason rather
// than an empty map while the App is wedged.
var errRuntimeWedged = errors.New("bootstrap: runtime wedged — prepare/commit swap and recovery both failed; process must be restarted")

// terminalRuntime is the sentinel ports.Runtime the App exposes through its
// RuntimeProvider while WEDGED (a prepare/commit swap AND its recovery both
// failed, leaving no active runtime). It is not a real runtime: it exists
// solely so the unauthenticated monitor /live probe — which returns 503 only
// when rt != nil && rt.Terminal() — fails closed for a wedged process, turning
// a dead-but-serving container into an orchestrator restart WITHOUT any change
// to httpapi. Every method reports the fail-closed (down/terminal) projection
// so /health and /ready also report unavailable rather than pretending a dead
// process is serving.
type terminalRuntime struct{}

var _ ports.Runtime = terminalRuntime{}

// --- RuntimeQuery (read side) ---

func (terminalRuntime) InstanceID() string { return "" }
func (terminalRuntime) IsRunning() bool    { return false }
func (terminalRuntime) Healthy() bool      { return false }

// Terminal is the whole point of the sentinel: it reports true so the /live
// probe fails closed for a wedged process.
func (terminalRuntime) Terminal() bool { return true }

func (terminalRuntime) ComponentErrors() map[string]error {
	return map[string]error{"bootstrap": errRuntimeWedged}
}

func (terminalRuntime) Role() string { return "" }

func (terminalRuntime) Routes() []ports.RouteInfo { return nil }

func (terminalRuntime) DeepHealth(context.Context) ports.DeepHealth {
	// Zero value: Running=false, Healthy=false, ReadyForTraffic=false — so
	// /deephealth reports 503 (not ready for traffic).
	return ports.DeepHealth{}
}

func (terminalRuntime) ReadinessLevel(context.Context) ports.ReadinessLevel {
	return ports.LevelDown
}

//nolint:ireturn // implements the ports.Runtime interface contract; a wedged runtime has no DLQ, so nil is the correct read port.
func (terminalRuntime) DLQReader() ports.DLQReader { return nil }

// --- RuntimeCommand (write side) ---

func (terminalRuntime) Start(context.Context) error { return errRuntimeWedged }
func (terminalRuntime) Stop(context.Context) error  { return nil }

func (terminalRuntime) Inject(context.Context, string, *messaging.Envelope) error {
	return errRuntimeWedged
}

//nolint:ireturn // implements the ports.Runtime interface contract; a wedged runtime has no DLQ, so nil is the correct admin port.
func (terminalRuntime) DLQAdmin() ports.DLQAdmin { return nil }
