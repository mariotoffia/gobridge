package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ctxRecordingSession records the context its Close was handed, so a test can
// tell an orderly disconnect (live context, real budget) from a token one (the
// already-expired build context, which every broker client refuses immediately).
type ctxRecordingSession struct {
	fakeSession
	closeErr      error
	closed        chan struct{}
	closeCtxErr   error
	closeDeadline time.Duration
	hasDeadline   bool
}

func newCtxRecordingSession() *ctxRecordingSession {
	return &ctxRecordingSession{closed: make(chan struct{}, 1)}
}

func (s *ctxRecordingSession) Close(ctx context.Context) error {
	s.closeCtxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		s.hasDeadline = true
		s.closeDeadline = time.Until(dl)
	}
	select {
	case s.closed <- struct{}{}:
	default:
	}
	return s.closeErr
}

// sessionOnlyFactory hands out one recording session and fails receiver
// construction, so complete() always fails AFTER the session exists.
type sessionOnlyFactory struct {
	fakeTransportFactory
	sess *ctxRecordingSession
}

func (f *sessionOnlyFactory) NewSession(context.Context, ports.SessionSpec) (ports.Session, error) {
	return f.sess, nil
}

func (f *sessionOnlyFactory) NewReceiver(context.Context, ports.ReceiverSpec, ports.Session) (ports.Receiver, error) {
	return nil, assertReceiverBuildFailure
}

var assertReceiverBuildFailure = context.DeadlineExceeded

// TestBuildFailureCleanup_ClosesSessionsUnderDetachedBudget: a build that fails
// because its deadline expired must still disconnect the sessions it opened
// under a LIVE, bounded context. Closing them with the expired build context
// makes every broker client refuse the disconnect, so the client id / durable
// session is still held at the broker when the immediately-following recovery
// build reconnects with the same identity — the collision the reload was trying
// to avoid. Receivers and senders already get this treatment; sessions are the
// piece that was still using the dead context.
func TestBuildFailureCleanup_ClosesSessionsUnderDetachedBudget(t *testing.T) {
	sess := newCtxRecordingSession()

	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "b1"},
		Sessions:  []ports.SessionDef{{ID: "s1", Transport: "sessonly"}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "sessonly", SessionID: "s1"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "sessonly", SessionID: "s1"}},
		Bindings:  []ports.BindingDef{{ID: "bd1", SenderID: "tx", SessionID: "s1", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			{
				ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold",
				Bindings: []string{"bd1"},
				Policy:   ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}

	b := NewBuilder(cfg).RegisterTransportFactory("sessonly", &sessionOnlyFactory{sess: sess})

	// The build context is already expired — the deadline-expired swap this
	// cleanup exists for.
	ctx, cancel := context.WithCancel(context.Background())
	prep, err := b.prepare(ctx)
	require.NoError(t, err)
	cancel()

	_, err = b.complete(ctx, prep)
	require.Error(t, err, "test setup: receiver construction must fail so cleanup runs")

	require.Len(t, sess.closed, 1, "the opened session must be closed on the failure path")
	assert.NoError(t, sess.closeCtxErr,
		"a session must be closed under a LIVE detached context, not the expired build context")
	assert.True(t, sess.hasDeadline, "the detached session close must stay bounded")
	assert.LessOrEqual(t, sess.closeDeadline, builderCloseBudget,
		"the detached session close must use the builder close budget")
}

// TestBuild_RuntimeInheritsConfiguredDrainBudget: bridge.drain_timeout is the
// ceiling the supervisor puts on Runtime.Stop, and it must be the runtime's own
// ceiling too. Without it the runtime fell back to a 5s internal budget, so a
// SIGTERM-driven Stop clamped its close phase to 5s while the supervisor was
// still holding a 30s drain open — the teardown raced its own supervisor and the
// configured budget governed nothing.
//
// The close context Stop hands each unmanaged session carries that budget as its
// deadline, which is what this measures.
func TestBuild_RuntimeInheritsConfiguredDrainBudget(t *testing.T) {
	const drain = 47 * time.Second
	sess := newCtxRecordingSession()

	cfg := &ports.BridgeConfig{
		Bridge:    ports.BridgeSettings{ID: "b1", DrainTimeout: drain.String()},
		Sessions:  []ports.SessionDef{{ID: "s1", Transport: "sessdrain"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "sessdrain", SessionID: "s1"}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "sessdrain"}},
		Bindings:  []ports.BindingDef{{ID: "bd1", SenderID: "tx", SessionID: "s1", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			{
				ID: "r1", ReceiverID: "rx", DeliveryMode: "direct_hold",
				Bindings: []string{"bd1"},
				Policy:   ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}

	b := NewBuilder(cfg).RegisterTransportFactory("sessdrain", &drainBudgetFactory{sess: sess})
	rt, err := b.Build(context.Background())
	require.NoError(t, err)
	require.NoError(t, rt.Start(context.Background()))
	require.NoError(t, rt.Stop(context.Background()))

	require.Len(t, sess.closed, 1, "Stop must close the unmanaged session")
	require.True(t, sess.hasDeadline, "the session close must be bounded")
	assert.Greater(t, sess.closeDeadline, drain/2,
		"the runtime must inherit bridge.drain_timeout as its stop budget, not the 5s fallback")
	assert.LessOrEqual(t, sess.closeDeadline, drain,
		"the runtime stop budget must not exceed bridge.drain_timeout")
}

type drainBudgetFactory struct {
	fakeTransportFactory
	sess *ctxRecordingSession
}

func (f *drainBudgetFactory) NewSession(context.Context, ports.SessionSpec) (ports.Session, error) {
	return f.sess, nil
}
