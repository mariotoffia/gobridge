package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

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

// requireClosedBeforeBuilt fails unless tf closed the session named closed
// before it built the one named built.
func requireClosedBeforeBuilt(t *testing.T, tf *perSessionTransportFactory, closed, built, msg string) {
	t.Helper()
	closedAt, builtAt := tf.eventIndex("close:"+closed), tf.eventIndex("new:"+built)
	require.GreaterOrEqual(t, closedAt, 0, "%s is closed", closed)
	require.Greater(t, builtAt, closedAt, msg)
}

// SwapInPlace passed to WithSwapMode acts as SwapAuto. Taken as a full swap
// mode it would fall back to an overlap swap, which builds the new runtime's
// sessions while the old runtime still holds the same exclusive broker
// identities.
func TestSupervisorInPlace_ExplicitSwapInPlaceActsAsAuto(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	s, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"), WithSwapMode(SwapInPlace))
	rt := s.Runtime()

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.NoError(t, ev.Error)
	assert.Equal(t, SwapInPlace, ev.SwapMode)
	assert.Same(t, rt, s.Runtime(), "a change confined to reload units reloads in place")
	requireClosedBeforeBuilt(t, tf, "b-s#1", "b-s#2", "owner b's exclusive session closes before its successor is built")

	next := applyTestConfig("a", "b")
	next.Bridge.DrainTimeout = "2s"
	ev = reloadTo(t, changes, swaps, next, 3)

	require.NoError(t, ev.Error)
	assert.Equal(t, SwapPrepareCommit, ev.SwapMode, "a full replacement still serializes around exclusive identities")
	assert.NotSame(t, rt, s.Runtime())
	requireClosedBeforeBuilt(t, tf, "a-s#1", "a-s#2", "the old runtime lets go of owner a's identity before the new one claims it")
}

// The Supervisor bounds an in-place reload's retires by the drain timeout its
// own stops use, so a WithDefaultDrainTimeout standing in for an unset
// drain_timeout reaches them too.
func TestSupervisorInPlace_TeardownUsesTheSupervisorsDrainTimeout(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s := NewSupervisor(WithDefaultDrainTimeout(time.Hour))
	s.RegisterTransport("tracked", tf)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	noDrainTimeout := func(cfg *ports.BridgeConfig) *ports.BridgeConfig {
		cfg.Bridge.DrainTimeout = ""
		return cfg
	}
	running := noDrainTimeout(applyTestConfig("a", "b"))
	rt := startApplyTestRuntime(t, s.newBuilder, running)
	plan := planApplyTest(t, tf, running, noDrainTimeout(changeRoute(applyTestConfig("a", "b"), "b")))

	_, err := s.applyInPlace(context.Background(), rt, running, plan)

	require.NoError(t, err)
	assert.Equal(t, time.Hour, plan.DrainTimeout)
}

// deadlineRecordingSessions records, per session id, the construction deadline
// of each session it builds; the zero time stands for none.
type deadlineRecordingSessions struct {
	*perSessionTransportFactory
	mu        sync.Mutex
	deadlines map[string][]time.Time
}

func (f *deadlineRecordingSessions) NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
	deadline, _ := ctx.Deadline()
	f.mu.Lock()
	f.deadlines[spec.ID] = append(f.deadlines[spec.ID], deadline)
	f.mu.Unlock()
	return f.perSessionTransportFactory.NewSession(ctx, spec)
}

// A serialized reload in place retires the unit before it commits the
// successor, and the retire may take the whole drain timeout. The successor's
// session is built under a swap deadline armed once the retired session has
// closed, with the whole deadline to spend, as a prepare-commit swap's sessions
// are after the old runtime stops. The instants are compared, not timed, so no
// delay in running the test can blur the check.
func TestSupervisorInPlace_CommitDeadlineIsArmedAfterTheRetire(t *testing.T) {
	const swapDeadline = time.Hour
	tf := &deadlineRecordingSessions{
		perSessionTransportFactory: newPerSessionTransportFactory(true),
		deadlines:                  make(map[string][]time.Time),
	}
	var mu sync.Mutex
	var retiredAt time.Time
	tf.onClose = func(name string) {
		if name == "b-s#1" {
			mu.Lock()
			retiredAt = time.Now()
			mu.Unlock()
		}
	}
	_, changes, swaps := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"), WithSwapDeadline(swapDeadline))

	ev := reloadTo(t, changes, swaps, changeRoute(applyTestConfig("a", "b"), "b"), 2)

	require.NoError(t, ev.Error)
	require.Equal(t, SwapInPlace, ev.SwapMode)
	tf.mu.Lock()
	built := tf.deadlines["b-s"]
	tf.mu.Unlock()
	require.Len(t, built, 2, "owner b's session and its successor")
	require.False(t, built[1].IsZero(), "the successor's session is built under a swap deadline")
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, built[1].Sub(retiredAt), swapDeadline,
		"the successor's swap deadline is armed after the retire, not charged for it")
}

func TestSwapMode_String(t *testing.T) {
	for mode, want := range map[SwapMode]string{
		SwapOverlap:       "overlap",
		SwapPrepareCommit: "prepare_commit",
		SwapAuto:          "auto",
		SwapInPlace:       "in_place",
		SwapMode(99):      "SwapMode(99)",
	} {
		assert.Equal(t, want, mode.String())
		assert.Equal(t, want, mode.LogValue().String(), "logged by name by every slog handler")
	}
}
