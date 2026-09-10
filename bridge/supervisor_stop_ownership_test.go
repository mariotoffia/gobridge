package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// A failed Runtime.Stop is not a "the old runtime keeps serving" outcome.
// Runtime.Stop has no early error return: by the time it reports a failure it
// has already flipped running=false, cancelled the work context, and closed
// managers, sessions and stores — and the runtime is single-use, so it can never
// serve again. Retaining it as the current runtime left the process bridging
// nothing behind a green /live until some unrelated config arrived.
//
// Both swap paths must therefore end in a state the orchestrator can act on:
// the supervisor wedges, Terminal() trips, and the composition-root backstop
// restarts the process with freshly-built transports (ADR-0004).

func TestSupervisor_OldStopFails_WedgesInsteadOfRetainingDeadRuntime_PrepareCommit(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", closeFailExclusiveFactory())
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "stop old runtime",
		"a failed old-runtime stop must fail the reload instead of committing a replacement onto the same identity")

	assert.NotSame(t, oldRt, s.Runtime(),
		"a torn-down runtime must never be retained as the current one")
	assert.Nil(t, s.Runtime(), "the wedged supervisor holds no runtime")
	assert.True(t, s.Terminal(),
		"a wedged supervisor must report terminal so /live fails closed and the process is restarted")
}

func TestSupervisor_OldStopFails_WedgesInsteadOfRetainingDeadRuntime_Overlap(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
		WithSwapMode(SwapOverlap),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", closeFailExclusiveFactory())
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "stop old runtime")

	assert.NotSame(t, oldRt, s.Runtime(),
		"overlap must not retain a torn-down runtime as the current one")
	assert.Nil(t, s.Runtime(), "the wedged supervisor holds no runtime")
	assert.True(t, s.Terminal(),
		"a wedged supervisor must report terminal so /live fails closed and the process is restarted")
}

// slowStopSession blocks Close until its context expires, so the old runtime's
// Stop consumes its whole drain budget before returning.
type slowStopSession struct {
	fakeSession
}

func (s *slowStopSession) Close(ctx context.Context) error {
	<-ctx.Done()
	// A drain that ran out of budget is not a failed teardown: the session is
	// gone either way, so Stop still reports success and the swap proceeds.
	return nil
}

// deadlineRecordingFactory hands out slow-closing sessions and records the
// construction budget each NewSession was given.
type deadlineRecordingFactory struct {
	fakeTransportFactory
	mu        sync.Mutex
	remaining []time.Duration
}

func (f *deadlineRecordingFactory) NewSession(ctx context.Context, _ ports.SessionSpec) (ports.Session, error) {
	f.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		f.remaining = append(f.remaining, time.Until(dl))
	} else {
		f.remaining = append(f.remaining, -1)
	}
	f.mu.Unlock()
	return &slowStopSession{}, nil
}

func (f *deadlineRecordingFactory) lastBudget() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.remaining) == 0 {
		return 0
	}
	return f.remaining[len(f.remaining)-1]
}

// TestSupervisor_PrepareCommitSwap_ConstructionDeadlineStartsAfterOldStop: the
// prepare/commit phase deadline used to be armed BEFORE the old runtime's Stop,
// which is allowed to consume the entire drain budget. A slow-but-successful
// drain therefore handed complete() an already-spent (or expired) context, so
// session and receiver construction failed and the reload bounced the old
// runtime — deterministically, on every retry. The construction budget must
// start when construction does.
//
// drain_timeout and the swap deadline are equal here, so a deadline armed before
// the Stop leaves construction NOTHING; the assertion is on the budget the
// replacement session construction actually receives.
func TestSupervisor_PrepareCommitSwap_ConstructionDeadlineStartsAfterOldStop(t *testing.T) {
	const swapDeadline = time.Second

	onSwap, swaps := swapChan(1)
	tf := &deadlineRecordingFactory{}
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
		WithSwapDeadline(swapDeadline),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", tf)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	// supervisorTestConfigWithSession sets drain_timeout: 1s, which the slow
	// session Close above burns in full.
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()
	require.NotNil(t, s.Runtime())

	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error,
		"a slow but successful old-runtime drain must not consume the construction deadline")
	assert.NotNil(t, s.Runtime(), "the replacement runtime must be installed")
	assert.False(t, s.Terminal())

	assert.Greater(t, tf.lastBudget(), swapDeadline*3/4,
		"the replacement session must be constructed under a fresh swap deadline, "+
			"not the remainder left after the old runtime drained")
}

// terminalRouteFactory hands out a receiver that fails immediately and owns a
// Close(ctx), so the route runner closes it on exit and the next supervised run
// cannot re-enter it — the route escalates and trips the runtime terminal. The
// trip leaves running=true (only healthy flips), which is the state StartBridge
// used to walk straight past. Its sessions are tracked so the test can see
// whether the orphaned runtime was ever torn down.
type terminalRouteFactory struct {
	trackingTransportFactory
}

func (f *terminalRouteFactory) NewReceiver(context.Context, ports.ReceiverSpec, ports.Session) (ports.Receiver, error) {
	return &terminalReceiver{}, nil
}

type terminalReceiver struct{}

func (r *terminalReceiver) Run(context.Context, func(context.Context, ports.Delivery) error) error {
	return errors.New("source permanently gone")
}

func (r *terminalReceiver) Close(context.Context) error { return nil }

// TestSupervisor_StartBridge_StopsTerminalRuntimeBeforePublishingNewOne:
// StartBridge gated only on IsRunning() (running && healthy). A component-failure
// trip flips healthy while leaving running true and never closes the sessions, so
// /bridge/start built and published a fresh runtime while the tripped one still
// held its broker sessions, SQLite handles and leases — for the process lifetime,
// since Terminal() then read the NEW runtime and the liveness backstop was
// disarmed.
func TestSupervisor_StartBridge_StopsTerminalRuntimeBeforePublishingNewOne(t *testing.T) {
	tf := &terminalRouteFactory{}
	tf.failAt = -1
	s := NewSupervisor(WithSupervisorBlueprintValidator(config.Validate))
	s.RegisterTransport("fake", tf)
	s.RegisterTransport("exclusive", tf)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)
	// The route's single-use receiver fails, is closed, and cannot be re-entered:
	// the second supervised run escalates and trips the runtime terminal.
	require.Eventually(t, oldRt.Terminal, 10*time.Second, 10*time.Millisecond,
		"test setup: the failing single-use route must trip the runtime terminal")
	orphanedSessions := tf.sessionCloseCount()

	require.NoError(t, s.StartBridge(context.Background()))

	newRt := s.Runtime()
	require.NotNil(t, newRt)
	assert.NotSame(t, oldRt, newRt, "StartBridge must publish a fresh runtime")
	assert.Greater(t, tf.sessionCloseCount(), orphanedSessions,
		"the tripped runtime must be stopped — releasing its broker sessions and leases — "+
			"before a replacement is published")
}

func (f *trackingTransportFactory) sessionCloseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sessions {
		n += s.CloseCount()
	}
	return n
}
