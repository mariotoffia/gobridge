package bridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A part built for a serialized reload claims the exclusive identity its added
// unit brings, possibly as soon as it is built. One that does not stop may
// still hold it, so the reload wedges rather than rebuild a retired unit that
// would claim it a second time.
func TestApply_SerializedBuildFailureWithAPartThatDoesNotStopIsWedged(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	// Owner c is added before owner b's successor, so one part is built before
	// the failing one.
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "c", "b"), "b"))
	require.True(t, plan.Serialized())
	tf.refuseSessions("b-s", 1)
	tf.refuseClose("c-s", 1, errCloseRefused)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errSessionRefused)
	require.ErrorIs(t, err, errCloseRefused)
	assert.Equal(t, InPlaceWedged, outcome)
	assert.Equal(t, []int{1}, tf.closeCounts("c-s"), "the part built before the failure is stopped")
	assert.Equal(t, -1, tf.eventIndex("new:b-s#2"), "the retired unit is not rebuilt")
	assert.Equal(t, []string{"a"}, runtimeRouteIDs(rt))
}

// One added unit listed twice, a plan no split produces, makes the second
// graft collide with the first, and the refused part does not stop.
func TestApply_SerializedGraftRefusalWithAPartThatDoesNotStopIsWedged(t *testing.T) {
	tf := newPerSessionTransportFactory(true)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
	require.True(t, plan.Serialized())
	plan.add = append(plan.add, plan.add[0])
	// #1 the running unit, retired; #2 the grafted successor; #3 the refused part.
	tf.refuseClose("b-s", 3, errCloseRefused)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorContains(t, err, "already registered")
	require.ErrorIs(t, err, errCloseRefused)
	assert.Equal(t, InPlaceWedged, outcome)
	counts := tf.closeCounts("b-s")
	require.Len(t, counts, 3, "the retired unit is not rebuilt")
	assert.Equal(t, 1, counts[2], "the refused part is stopped")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
}

// A restored unit whose graft is refused leaves the runtime running neither
// configuration. The part built to restore it claims the exclusive identity the
// retired unit held, so in a serialized reload a part that does not stop wedges
// the reload: the caller's torn handling would rebuild the running
// configuration and claim that identity beside it. A part that stops leaves no
// ownership in doubt, and the reload is torn.
//
// One retired unit listed twice, a plan no split produces, makes the second
// restore collide with the first; its second retire finds nothing to retire.
func TestApply_SerializedRestoreGraftRefusal(t *testing.T) {
	cases := map[string]struct {
		closeErr error
		want     InPlaceOutcome
	}{
		"a restored part that does not stop is wedged": {closeErr: errCloseRefused, want: InPlaceWedged},
		"a restored part that stops is torn":           {want: InPlaceTorn},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tf := newPerSessionTransportFactory(true)
			newBuilder := applyTestBuilder(tf)
			running := applyTestConfig("a", "b")
			rt := startApplyTestRuntime(t, newBuilder, running)
			plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "b"), "b"))
			require.True(t, plan.Serialized())
			plan.retire = append(plan.retire, plan.retire[0])
			tf.refuseSessions("b-s", 1) // the added unit's part fails to build, so restore runs
			// #1 the running unit, retired; #2 the first restore, grafted; #3 the
			// second restore, refused.
			if tc.closeErr != nil {
				tf.refuseClose("b-s", 3, tc.closeErr)
			}

			outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

			require.ErrorIs(t, err, errSessionRefused)
			require.ErrorContains(t, err, "already registered")
			if tc.closeErr != nil {
				require.ErrorIs(t, err, tc.closeErr)
			}
			assert.Equal(t, tc.want, outcome)
			assert.Equal(t, []int{1, 0, 1}, tf.closeCounts("b-s"), "the refused restored part is stopped")
		})
	}
}

// A build-first reload's added units claim no exclusive identity, so a part
// that does not stop after a failed build holds nothing the running units
// claim: the reload changes nothing and reports the stop failure.
func TestApply_BuildFirstFailureWithAPartThatDoesNotStopIsUnchanged(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	newBuilder := applyTestBuilder(tf)
	running := applyTestConfig("a", "b")
	rt := startApplyTestRuntime(t, newBuilder, running)
	oldPolicy := runtimeRoutePolicy(t, rt, "b")
	plan := planApplyTest(t, tf, running, changeRoute(applyTestConfig("a", "c", "b"), "b"))
	require.False(t, plan.Serialized())
	tf.refuseSessions("b-s", 1)
	tf.refuseClose("c-s", 1, errCloseRefused)

	outcome, err := plan.Apply(context.Background(), rt, newBuilder, nil)

	require.ErrorIs(t, err, errSessionRefused)
	require.ErrorIs(t, err, errCloseRefused)
	assert.Equal(t, InPlaceUnchanged, outcome)
	assert.Equal(t, []int{1}, tf.closeCounts("c-s"), "the part built before the failure is stopped")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "the old owner b is untouched")
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"))
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
	assert.Equal(t, oldPolicy, runtimeRoutePolicy(t, rt, "b"), "owner b still runs its old route")
}
