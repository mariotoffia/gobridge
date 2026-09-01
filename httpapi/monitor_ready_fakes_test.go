package httpapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// readyReceiver is a route source that comes up and then simply stays alive
// until the runtime stops it. running closes once Run is entered, so a test can
// wait for the route to be up instead of guessing.
type readyReceiver struct {
	running chan struct{}
}

func newReadyReceiver() *readyReceiver {
	return &readyReceiver{running: make(chan struct{})}
}

func (r *readyReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	close(r.running)
	<-ctx.Done()
	return ctx.Err()
}

// discardSender accepts every delivery. The readiness probes under test are
// about pipeline liveness, not about what a target did with a message.
type discardSender struct{}

func (discardSender) Send(context.Context, ports.OutboundMessage) error { return nil }

// newBridgingRuntime returns a STARTED runtime that actually carries a route,
// so the readiness probes it answers are about a bridge that can move messages.
// A runtime with no routes and no sessions is the start-empty state and is
// deliberately capped below LevelFull, so it cannot stand in for a configured
// instance in a readiness test.
func newBridgingRuntime(t *testing.T, instanceID string) *runtime.Runtime {
	t.Helper()
	rt := runtime.New(runtime.WithInstanceID(instanceID))
	recv := newReadyReceiver()
	require.NoError(t, rt.AddRoute(runtime.RouteConfig{
		ID: "route-ready",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, recv, discardSender{}, nil, nil))

	require.NoError(t, rt.Start(context.Background()))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireClosed(t, recv.running, 2*time.Second)
	wait.Until(t, 2*time.Second, "route reaches full readiness", func() bool {
		return rt.ReadinessLevel(context.Background()) == ports.LevelFull
	})
	return rt
}
