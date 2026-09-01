package clocktest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// A tick loop that re-paces itself calls Ticker.Reset at the END of the
// handler it is running. A test driving that loop advances the fake clock from
// its own goroutine, so the advance can land WHILE the handler is still
// running: the tick is fired and buffered, and the handler's Reset arrives
// after it. These tests pin that the fired tick survives, because under a fake
// clock nothing else moves time — a swallowed tick is not "one period late",
// it never arrives at all, and the loop and the test both wait forever.

// TestTickerReset_KeepsATickAlreadyFired proves Reset does not discard a tick
// Advance has already delivered into the channel.
func TestTickerReset_KeepsATickAlreadyFired(t *testing.T) {
	fake := clocktest.New()
	tk := fake.NewTicker(5 * time.Second)
	t.Cleanup(tk.Stop)

	fake.Advance(6 * time.Second) // the tick at +5s fires and is buffered
	tk.Reset(5 * time.Second)     // the handler re-paces itself afterwards

	select {
	case <-tk.C():
	default:
		t.Fatal("Reset discarded a tick the test had already advanced past; " +
			"under a fake clock that tick can never be re-delivered")
	}
}

// TestTickerReset_ReArmsFromTheCurrentFakeTime proves the re-pacing itself is
// unchanged: after Reset the next tick is one full NEW period away from the
// current fake time, not a leftover of the old cadence.
func TestTickerReset_ReArmsFromTheCurrentFakeTime(t *testing.T) {
	fake := clocktest.New()
	tk := fake.NewTicker(5 * time.Second)
	t.Cleanup(tk.Stop)

	fake.Advance(6 * time.Second)
	require.Len(t, tk.C(), 1, "precondition: the +5s tick is pending")
	<-tk.C() // the handler consumes it

	tk.Reset(20 * time.Second) // now = +6s, so the next tick is due at +26s

	fake.Advance(19 * time.Second) // +25s: still short of the new cadence
	assert.Empty(t, tk.C(), "Reset must re-arm from the current fake time, not the old cadence")

	fake.Advance(1 * time.Second) // +26s
	assert.Len(t, tk.C(), 1, "the next tick is one full new period after Reset")
}

// TestTickerReset_OnAStoppedTickerResumesIt pins the existing revival contract
// so the delivery fix cannot quietly change it.
func TestTickerReset_OnAStoppedTickerResumesIt(t *testing.T) {
	fake := clocktest.New()
	tk := fake.NewTicker(5 * time.Second)
	t.Cleanup(tk.Stop)

	tk.Stop()
	fake.Advance(10 * time.Second)
	require.Empty(t, tk.C(), "precondition: a stopped ticker does not fire")

	tk.Reset(5 * time.Second)
	fake.Advance(5 * time.Second)

	assert.Len(t, tk.C(), 1, "Reset revives a stopped ticker")
}

// TestTimerReset_AfterStopAndAdvanceRearms proves the same revival contract for
// timers. Stop marks a timer inactive and the next Advance de-registers it;
// Reset must bring it back, or a loop that stops and re-arms its timer goes
// permanently deaf and the test that drives it hangs on a clock that will never
// fire again.
func TestTimerReset_AfterStopAndAdvanceRearms(t *testing.T) {
	fake := clocktest.New()
	tm := fake.NewTimer(5 * time.Second)
	t.Cleanup(func() { tm.Stop() })

	tm.Stop()
	fake.Advance(10 * time.Second) // de-registers the stopped timer
	require.Empty(t, tm.C(), "precondition: a stopped timer does not fire")

	tm.Reset(5 * time.Second)
	fake.Advance(5 * time.Second)

	assert.Len(t, tm.C(), 1, "Reset must re-arm a timer that Stop and Advance retired")
}

// TestTimerReset_AfterFiringRearms pins the already-working half of the same
// contract, so the rework cannot regress it.
func TestTimerReset_AfterFiringRearms(t *testing.T) {
	fake := clocktest.New()
	tm := fake.NewTimer(5 * time.Second)
	t.Cleanup(func() { tm.Stop() })

	fake.Advance(5 * time.Second)
	require.Len(t, tm.C(), 1, "precondition: the timer fired")
	<-tm.C()

	tm.Reset(5 * time.Second)
	fake.Advance(5 * time.Second)

	assert.Len(t, tm.C(), 1, "Reset must re-arm a timer that already fired")
}
