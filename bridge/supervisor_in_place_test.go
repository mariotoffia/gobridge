package bridge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// runInPlaceSupervisor runs a Supervisor over tf as the tracked transport and
// the memory store, starting on cfg. It returns the channel to send reloads on
// and the channel their swap events arrive on. Run stops when the test ends.
func runInPlaceSupervisor(t *testing.T, tf ports.TransportFactory, cfg *ports.BridgeConfig, opts ...SupervisorOption,
) (*Supervisor, chan<- *ports.BridgeConfig, <-chan SwapEvent) {
	t.Helper()
	onSwap, swaps := swapChan(4)
	opts = append([]SupervisorOption{WithOnSwap(onSwap), WithSupervisorBlueprintValidator(config.Validate)}, opts...)
	s := NewSupervisor(opts...)
	s.RegisterTransport("tracked", tf)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	changes := make(chan *ports.BridgeConfig, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := runSupervisorAsync(ctx, s, cfg, changes)
	t.Cleanup(func() { cancel(); <-errCh })
	wait.Until(t, 5*time.Second, "the initial runtime starts", func() bool { return s.Runtime() != nil })
	return s, changes, swaps
}

// reloadTo sends next as version and returns the swap event it produced.
func reloadTo(t *testing.T, changes chan<- *ports.BridgeConfig, swaps <-chan SwapEvent, next *ports.BridgeConfig, version int) SwapEvent {
	t.Helper()
	next.Version = version
	require.True(t, sendConfig(changes, next, time.Second), "the reload is accepted")
	return awaitSwap(t, swaps)
}

func TestSupervisorInPlace_OtherOwnersSessionsStayConnected(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.NoError(t, ev.Error)
	assert.Equal(t, SwapInPlace, ev.SwapMode)
	assert.Same(t, rt, s.Runtime(), "the running runtime is kept")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a's session is never closed")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"), "owner b's session is replaced")
	assert.Equal(t, 7, runtimeRoutePolicy(t, rt, "b").MaxInFlight, "the runtime runs owner b's new route")
	assert.Equal(t, 2, s.Config().Version)
}

// flowTransportFactory is a perSessionTransportFactory whose owner a receiver
// the test drives and whose owner a sender records what it is handed.
type flowTransportFactory struct {
	*perSessionTransportFactory
	rx *workCtxReceiver
	tx *fakeSender
}

func (f *flowTransportFactory) NewReceiver(ctx context.Context, spec ports.ReceiverSpec, sess ports.Session) (ports.Receiver, error) {
	if spec.ID == "a-rx" {
		return f.rx, nil
	}
	return f.perSessionTransportFactory.NewReceiver(ctx, spec, sess)
}

func (f *flowTransportFactory) NewSender(ctx context.Context, spec ports.SenderSpec, sess ports.Session) (ports.Sender, error) {
	if spec.ID == "a-tx" {
		return f.tx, nil
	}
	return f.perSessionTransportFactory.NewSender(ctx, spec, sess)
}

// Owner a's route delivers before, during and after a reload of owner b: it
// is held open while owner b's unit retires, and a message sent then arrives.
func TestSupervisorInPlace_MessagesFlowThroughUnchangedRouteDuringReload(t *testing.T) {
	tf := &flowTransportFactory{perSessionTransportFactory: newPerSessionTransportFactory(false), rx: newWorkCtxReceiver(), tx: &fakeSender{}}
	retiring, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseB := func() { releaseOnce.Do(func() { close(release) }) }
	tf.onClose = func(name string) {
		if name == "b-s#1" {
			close(retiring)
			<-release
		}
	}
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	t.Cleanup(releaseB) // runs before the Supervisor stops, so a failed test never leaves the retire held
	rt := s.Runtime()
	delivered := func(n int) func() bool { return func() bool { return len(tf.tx.snapshot()) == n } }

	require.NoError(t, tf.rx.emitWork(newBlockingDelivery("before")))
	wait.Until(t, 2*time.Second, "the message sent before the reload is delivered", delivered(1))

	next := changeRoute(applyTestConfig("a", "b"), "b")
	next.Version = 2
	require.True(t, sendConfig(changes, next, time.Second))
	wait.RequireClosed(t, retiring, 5*time.Second)
	require.NoError(t, tf.rx.emitWork(newBlockingDelivery("during")))
	wait.Until(t, 2*time.Second, "the message sent while owner b retires is delivered", delivered(2))
	releaseB()
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)
	require.Equal(t, SwapInPlace, ev.SwapMode)

	require.NoError(t, tf.rx.emitWork(newBlockingDelivery("after")))
	wait.Until(t, 2*time.Second, "the message sent after the reload is delivered", delivered(3))

	var ids []string
	for _, msg := range tf.tx.snapshot() {
		ids = append(ids, msg.Envelope.ID())
	}
	assert.Equal(t, []string{"before", "during", "after"}, ids)
	tf.rx.mu.Lock()
	runCtx := tf.rx.ctx
	tf.rx.mu.Unlock()
	assert.NoError(t, runCtx.Err(), "owner a's receiver ran through the reload without a restart")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
	assert.Same(t, rt, s.Runtime())
}

func TestSupervisorInPlace_ExplicitSwapModeKeepsFullReplacement(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"), WithSwapMode(SwapOverlap))
	rt := s.Runtime()

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.NoError(t, ev.Error)
	assert.Equal(t, SwapOverlap, ev.SwapMode)
	assert.NotSame(t, rt, s.Runtime(), "an explicit swap mode replaces the runtime")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a is rebuilt with everything else")
}

func TestSupervisorInPlace_BridgeSettingChangeUsesFullReplacement(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()
	next := applyTestConfig("a", "b")
	next.Bridge.DrainTimeout = "2s"

	ev := reloadTo(t, changes, swaps, next, 2)

	require.NoError(t, ev.Error)
	assert.Equal(t, SwapOverlap, ev.SwapMode)
	assert.NotSame(t, rt, s.Runtime(), "a bridge-wide change replaces the runtime")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"))
	assert.Equal(t, []int{1, 0}, tf.closeCounts("b-s"))
}

func TestSupervisorInPlace_FailedBuildKeepsRunningConfig(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()
	oldPolicy := runtimeRoutePolicy(t, rt, "b")
	tf.refuseSessions("b-s", 1)

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.ErrorIs(t, ev.Error, errSessionRefused)
	assert.Equal(t, SwapInPlace, ev.SwapMode)
	assert.Equal(t, 1, s.Config().Version, "the running configuration stays applied")
	assert.Same(t, rt, s.Runtime())
	assert.True(t, rt.IsRunning())
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a is untouched")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b is untouched")
	assert.Equal(t, oldPolicy, runtimeRoutePolicy(t, rt, "b"))
}

// A serialized reload retires owner b, then neither its successor nor its
// restore can be built: the runtime runs neither configuration, and the
// Supervisor replaces it with one built from the running configuration.
func TestSupervisorInPlace_TornRecoversOldConfig(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()
	oldPolicy := runtimeRoutePolicy(t, rt, "b")
	tf.refuseSessions("b-s", 2)

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.ErrorIs(t, ev.Error, errSessionRefused)
	assert.Equal(t, SwapInPlace, ev.SwapMode)
	recovered := s.Runtime()
	require.NotNil(t, recovered)
	assert.NotSame(t, rt, recovered, "the torn runtime is replaced")
	assert.False(t, rt.IsRunning(), "the torn runtime is stopped")
	assert.True(t, recovered.IsRunning())
	assert.False(t, s.Terminal(), "a successful recovery does not wedge")
	assert.Equal(t, 1, s.Config().Version)
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(recovered))
	assert.Equal(t, oldPolicy, runtimeRoutePolicy(t, recovered, "b"), "the recovered runtime runs the running configuration")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a is rebuilt with the recovered runtime")
}

// A torn runtime whose stop fails may still hold the identities a rebuild
// would claim, so the Supervisor wedges instead of recovering, as it does when
// an old runtime fails to stop during a full swap.
func TestSupervisorInPlace_TornRuntimeThatDoesNotStopWedges(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	tf.refuseSessions("b-s", 2)
	tf.refuseClose("a-s", 1, errCloseRefused)

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.ErrorIs(t, ev.Error, errSessionRefused)
	assert.True(t, s.Terminal())
	assert.Nil(t, s.Runtime())
	assert.Len(t, tf.closeCounts("a-s"), 1, "nothing is rebuilt over a session that did not close")
}

func TestSupervisorInPlace_WedgedWhenRetireFails(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()
	tf.refuseClose("b-s", 1, errCloseRefused)

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.ErrorIs(t, ev.Error, errCloseRefused)
	assert.Equal(t, SwapInPlace, ev.SwapMode)
	assert.True(t, s.Terminal(), "a unit that did not stop cleanly wedges the Supervisor")
	assert.Nil(t, s.Runtime())
	assert.False(t, rt.IsRunning(), "the runtime is stopped")
	degraded, _ := s.Degraded()
	assert.True(t, degraded)
}

// A forced reload that orphans an outbox partition in place keeps the running
// outbox store, so the post-swap re-check reads that store: a record the
// retired unit's drainer left pending is reported as stranded.
func TestSupervisorInPlace_OrphanedPartitionStrandIsReadFromTheRunningStore(t *testing.T) {
	rec := &ports.RecordingExporter{}
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(WithSupervisorBlueprintValidator(config.Validate), WithOnSwap(onSwap),
		WithSupervisorMetrics(rec), WithAllowDestructiveReload(true))
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", &exclusiveTransportFactory{})
	stores := &lateIngressStoreFactory{pendingByCall: []int{1}}
	s.RegisterStoreFactory("memory", stores)
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()
	rt := s.Runtime()

	// Route r1 drops its inline session, so partition SESSION#s1 loses its drainer.
	next := supervisorTestConfigWithSession("r1", "s1")
	next.Routes[0].DeliveryMode = "direct_hold"
	next.Routes[0].Session = nil
	require.True(t, sendConfig(ch, next, time.Second))
	ev := awaitSwap(t, swaps)

	require.NoError(t, ev.Error)
	require.Equal(t, SwapInPlace, ev.SwapMode)
	assert.Same(t, rt, s.Runtime())
	stores.mu.Lock()
	opened := stores.calls
	stores.mu.Unlock()
	assert.Equal(t, 1, opened, "the reload opens no outbox store of its own")
	strands := rec.FindEntries(shared.MetricOutboxStranded)
	require.Len(t, strands, 1, "the pending record in the running store is reported")
	assert.Equal(t, int64(1), strands[0].IValue)
	assert.Contains(t, strands[0].Tags,
		shared.Tag{Key: shared.TagKeyPartition, Value: persistence.OutboxPartitionKey("s1", "")})
}

// A serialized reload in place retires the unit before it commits the
// successor, and the retire may burn the whole drain timeout. The successor's
// sessions must still be built under a fresh swap deadline, as a prepare-commit
// swap's are after the old runtime stops.
func TestSupervisorInPlace_CommitDeadlineStartsAfterSlowRetire(t *testing.T) {
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

	// The retired session's slow Close runs until the close budget the retire
	// draws from drain_timeout (1s) ends.
	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()
	rt := s.Runtime()
	require.NotNil(t, rt)

	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "a slow but successful retire must not consume the construction deadline")
	require.Equal(t, SwapInPlace, ev.SwapMode)
	assert.Same(t, rt, s.Runtime())

	assert.Greater(t, tf.lastBudget(), swapDeadline*3/4,
		"the successor's session must be built under a fresh swap deadline, "+
			"not the remainder left after the retired unit drained")
}

// credPerSessionTransportFactory is a perSessionTransportFactory whose
// sessions take credential rotations, so a credential refresher watches each.
type credPerSessionTransportFactory struct {
	*perSessionTransportFactory
}

func (f credPerSessionTransportFactory) NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
	sess, err := f.perSessionTransportFactory.NewSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	return credAwareRecordedSession{sess.(*recordedSession)}, nil
}

type credAwareRecordedSession struct{ *recordedSession }

func (credAwareRecordedSession) ApplyCredentials(context.Context, *connectivity.CredentialSet) error {
	return nil
}

// liveWatchPushStore counts the credential pollers watching it: the Watch
// calls whose context has not ended.
type liveWatchPushStore struct{ live atomic.Int32 }

func (p *liveWatchPushStore) Watch(ctx context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	p.live.Add(1)
	ch := make(chan *connectivity.CredentialSet)
	go func() {
		<-ctx.Done()
		p.live.Add(-1)
		close(ch)
	}()
	return ch, nil
}

var _ ports.PushCredentialStore = (*liveWatchPushStore)(nil)

// Every reload grafts a part with a credential refresher of its own, and the
// retire of its predecessor must release that predecessor's refresher. Were
// the refreshers left behind, each reload would add a poller that never ends.
func TestSupervisorInPlace_RepeatedReloadsDoNotAccumulateHooks(t *testing.T) {
	credCfg := func(maxInFlight int) *ports.BridgeConfig {
		cfg := applyTestConfig("a", "b")
		for i := range cfg.Sessions {
			cfg.Sessions[i].Config = &testCredConfig{URI: "cred://" + cfg.Sessions[i].ID}
		}
		cfg.Routes[1].Policy.MaxInFlight = maxInFlight
		return cfg
	}
	pull := &fakeCredentialStore{creds: map[string]*connectivity.CredentialSet{
		"cred://a-s": connectivity.NewCredentialSet(pwCred("a", "p"), nil),
		"cred://b-s": connectivity.NewCredentialSet(pwCred("b", "p"), nil),
	}}
	push := &liveWatchPushStore{}
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, credPerSessionTransportFactory{tf}, credCfg(0),
		WithSupervisorCredentialStore(pull), WithSupervisorPushCredentialStore(push))
	rt := s.Runtime()
	const steady = 2 // one poller per credentials URI
	wait.Until(t, 2*time.Second, "the initial runtime watches both credentials", func() bool { return push.live.Load() == steady })

	for i := range 20 {
		ev := reloadTo(t, changes, swaps, credCfg(1+i%2), i+2)
		require.NoError(t, ev.Error)
		require.Equal(t, SwapInPlace, ev.SwapMode)
		wait.Until(t, 2*time.Second, "the retired unit's poller stops", func() bool { return push.live.Load() == steady })
	}
	assert.Same(t, rt, s.Runtime())
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "owner a keeps its session through every reload")
	assert.Len(t, tf.closeCounts("b-s"), 21, "every reload replaces owner b's session")
}

// An in-place reload keeps the runtime, so the watch started for the running
// configuration watches the very runtime the next configuration's watch does.
// Only the newest may judge it: the older watch can no longer mark, and the
// runtime is marked no earlier than the newest watch's own budget allows. The
// sessions of the fake transport never report connected, so the runtime never
// converges.
func TestSupervisorInPlace_ConvergenceWatchOfOlderGenerationCannotMark(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	tf := newPerSessionTransportFactory(false)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"), WithSupervisorClock(clk))
	rt := s.Runtime()

	// The first reload starts the older watch.
	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)
	require.NoError(t, ev.Error)
	require.Equal(t, SwapInPlace, ev.SwapMode)
	wait.Until(t, 2*time.Second, "the older watch arms its poll timer", func() bool { return clk.TimerCount() == 1 })
	s.mu.RLock()
	olderGen := s.convergenceGen
	s.mu.RUnlock()
	// Half the older watch's budget passes first, so its deadline falls well
	// inside the newer watch's budget.
	clk.Advance(convergenceBudgetFloor / 2)

	reloadAt := clk.Now()
	ev = reloadTo(t, changes, swaps, applyTestConfig("a", "b"), 3)
	require.NoError(t, ev.Error)
	require.Equal(t, SwapInPlace, ev.SwapMode)
	require.Same(t, rt, s.Runtime())

	_, marked := s.markConvergenceDegraded(rt, olderGen, ports.LevelLive, convergenceBudgetFloor)
	assert.False(t, marked, "the watch of the configuration the runtime ran before cannot mark it")
	assert.False(t, s.watcherCurrent(rt, olderGen), "the older watch stops at its next poll")

	require.Eventually(t, func() bool {
		clk.Advance(convergencePollInterval)
		degraded, _ := s.Degraded()
		return degraded
	}, 5*time.Second, 5*time.Millisecond, "the newest watch marks the runtime when its budget expires")
	assert.GreaterOrEqual(t, clk.Now().Sub(reloadAt), convergenceBudgetFloor,
		"no watch marks the runtime before the newest watch's budget expires")
	_, reason := s.Degraded()
	assert.Contains(t, reason, "config version 3")
}
