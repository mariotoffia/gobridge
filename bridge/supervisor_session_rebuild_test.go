package bridge

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// errTransportGone is a session failure that only a fresh session clears.
var errTransportGone = fmt.Errorf("fake: %w", shared.ErrTransportClosedPermanently)

// rebuildWait bounds the wait for a session rebuild. The Supervisor does not
// hand its clock to the runtimes it builds, so the runtime paces the report
// with its rebuild backoff (at most 1s for a first failure) on the real clock.
const rebuildWait = 5 * time.Second

// failStartOf returns an onStart hook under which only the session named name
// fails to start, with errTransportGone.
func failStartOf(name string) func(string) error {
	return func(started string) error {
		if started == name {
			return errTransportGone
		}
		return nil
	}
}

// Owner a's session fails in a way only a fresh session clears: the Supervisor
// rebuilds owner a's unit inside the running runtime, while owner b keeps its
// session throughout.
func TestSupervisorRebuildsOnlyTheFailedSessionsUnit(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	tf.onStart = failStartOf("a-s#1")
	rec := &ports.RecordingExporter{}
	s, _, _ := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"), WithSupervisorMetrics(rec))
	rt := s.Runtime()

	wait.Until(t, rebuildWait, "owner a's unit runs on a fresh session", func() bool {
		return tf.eventIndex("start:a-s#2") != -1 && !rt.SessionUnrecoverable("a-s")
	})

	assert.Same(t, rt, s.Runtime(), "the running runtime is kept")
	assert.True(t, rt.IsRunning())
	assert.False(t, s.Terminal())
	assert.Less(t, tf.eventIndex("close:a-s#1"), tf.eventIndex("new:a-s#2"), "the failed session closes before its copy is built")
	assert.Equal(t, []int{1, 0}, tf.closeCounts("a-s"), "owner a's failed session is closed once and replaced")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"), "owner b keeps its one session")
	assert.Equal(t, []string{"a", "b"}, runtimeRouteIDs(rt))
	rebuilds := rec.FindEntries(shared.MetricSessionRebuilds)
	require.Len(t, rebuilds, 1)
	assert.Contains(t, rebuilds[0].Tags, shared.Tag{Key: shared.TagKeySessionID, Value: "a-s"})
}

// A failed session that does not close may still hold its broker connection,
// so the Supervisor builds no copy beside it: it stops the runtime and wedges.
func TestSupervisorSessionRebuildWedgesWhenTheUnitDoesNotStop(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	tf.onStart = failStartOf("a-s#1")
	tf.refuseClose("a-s", 1, errCloseRefused)
	s, _, _ := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()

	wait.Until(t, rebuildWait, "the Supervisor wedges", s.Terminal)

	assert.Nil(t, s.Runtime())
	assert.False(t, rt.IsRunning(), "the runtime is stopped")
	assert.Equal(t, -1, tf.eventIndex("new:a-s#2"), "no copy is built beside a session that did not close")
	degraded, reason := s.Degraded()
	assert.True(t, degraded)
	assert.Contains(t, reason, "a session rebuild could not clear the failed session or stop its unit cleanly")
}

// A report that arrives when the runtime no longer records the fault, because
// a reload or an earlier rebuild already replaced the session, rebuilds
// nothing.
func TestSupervisorSessionRebuildIgnoresAStaleReport(t *testing.T) {
	tf := newPerSessionTransportFactory(false)
	s, _, _ := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"))
	rt := s.Runtime()

	s.rebuildSession("a-s")

	assert.Same(t, rt, s.Runtime())
	assert.True(t, rt.IsRunning())
	assert.False(t, s.Terminal())
	assert.Equal(t, []int{0}, tf.closeCounts("a-s"), "the healthy session is not retired")
	assert.Equal(t, []int{0}, tf.closeCounts("b-s"))
}

// A rebuild refused before it retires anything leaves the failed session
// unserved. The Supervisor does not keep a runtime in that state: it stops it
// and wedges, so the process restarts.
func TestSupervisorSessionRebuildWedgesWhenTheFaultSurvives(t *testing.T) {
	errInvalid := errors.New("configuration refused")
	var refuse atomic.Bool
	validate := func(cfg *ports.BridgeConfig) error {
		if refuse.Load() {
			return errInvalid
		}
		return config.Validate(cfg)
	}
	tf := newPerSessionTransportFactory(false)
	tf.onStart = func(name string) error {
		if name == "a-s#1" {
			refuse.Store(true) // the rebuild's preflight now refuses the running configuration
			return errTransportGone
		}
		return nil
	}
	s, _, _ := runInPlaceSupervisor(t, tf, applyTestConfig("a", "b"), WithSupervisorBlueprintValidator(validate))
	rt := s.Runtime()

	wait.Until(t, rebuildWait, "the Supervisor wedges", s.Terminal)

	assert.Nil(t, s.Runtime())
	assert.False(t, rt.IsRunning(), "the runtime is stopped")
	assert.Equal(t, -1, tf.eventIndex("new:a-s#2"), "no copy of the failed session is built")
	assert.Len(t, tf.closeCounts("b-s"), 1, "owner b is not rebuilt either")
	degraded, reason := s.Degraded()
	assert.True(t, degraded)
	assert.Contains(t, reason, "a session rebuild could not clear the failed session")
}
