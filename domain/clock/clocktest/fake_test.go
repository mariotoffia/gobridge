package clocktest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

var _ clock.Clock = (*clocktest.Fake)(nil)

func TestNowAdvances(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(start)

	if got := clk.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}

	clk.Advance(5 * time.Second)

	want := start.Add(5 * time.Second)
	if got := clk.Now(); !got.Equal(want) {
		t.Fatalf("after Advance(5s): Now() = %v, want %v", got, want)
	}
}

func TestSince(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(start)
	clk.Advance(3 * time.Second)

	if got := clk.Since(start); got != 3*time.Second {
		t.Fatalf("Since() = %v, want 3s", got)
	}
}

func TestTimerFires(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tmr := clk.NewTimer(10 * time.Second)

	// Not yet fired.
	select {
	case <-tmr.C():
		t.Fatal("timer fired too early")
	default:
	}

	clk.Advance(10 * time.Second)

	select {
	case <-tmr.C():
	default:
		t.Fatal("timer did not fire after Advance")
	}
}

func TestTimerDoesNotFireEarly(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tmr := clk.NewTimer(10 * time.Second)

	clk.Advance(9 * time.Second)

	select {
	case <-tmr.C():
		t.Fatal("timer fired before deadline")
	default:
	}

	clk.Advance(1 * time.Second)

	select {
	case <-tmr.C():
	default:
		t.Fatal("timer should have fired at exactly 10s")
	}
}

func TestTimerStop(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tmr := clk.NewTimer(5 * time.Second)

	if stopped := tmr.Stop(); !stopped {
		t.Fatal("Stop on active timer should return true")
	}

	clk.Advance(10 * time.Second)

	select {
	case <-tmr.C():
		t.Fatal("stopped timer should not fire")
	default:
	}
}

func TestTimerReset(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tmr := clk.NewTimer(5 * time.Second)

	clk.Advance(3 * time.Second)
	tmr.Reset(10 * time.Second)

	clk.Advance(9 * time.Second)

	select {
	case <-tmr.C():
		t.Fatal("reset timer fired before new deadline")
	default:
	}

	clk.Advance(1 * time.Second)

	select {
	case <-tmr.C():
	default:
		t.Fatal("reset timer should have fired")
	}
}

// TestTimerResetAfterFire verifies that a timer which has already fired
// (and therefore been removed from the Fake's internal pending-timer
// list) is re-registered by Reset and can fire again. This matches the
// behavior of the real runtime, where time.Timer.Reset after firing
// re-arms the timer, and it is the pattern used by long-running drain
// and renew loops (e.g. OutboxDrainer.Run).
func TestTimerResetAfterFire(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tmr := clk.NewTimer(5 * time.Second)

	clk.Advance(5 * time.Second)

	select {
	case <-tmr.C():
	default:
		t.Fatal("timer did not fire on initial deadline")
	}

	tmr.Reset(5 * time.Second)

	clk.Advance(4 * time.Second)
	select {
	case <-tmr.C():
		t.Fatal("timer fired before second deadline")
	default:
	}

	clk.Advance(1 * time.Second)
	select {
	case <-tmr.C():
	default:
		t.Fatal("timer did not fire on second deadline after Reset")
	}
}

func TestAfter(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	ch := clk.After(5 * time.Second)

	clk.Advance(5 * time.Second)

	select {
	case <-ch:
	default:
		t.Fatal("After channel did not fire")
	}
}

func TestTickerFires(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tkr := clk.NewTicker(3 * time.Second)

	clk.Advance(3 * time.Second)
	select {
	case <-tkr.C():
	default:
		t.Fatal("ticker did not fire after first period")
	}

	clk.Advance(3 * time.Second)
	select {
	case <-tkr.C():
	default:
		t.Fatal("ticker did not fire after second period")
	}
}

func TestTickerMultiplePeriods(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tkr := clk.NewTicker(2 * time.Second)

	// Advance by 5s: ticker at 2s and 4s should fire. Because channel
	// is buffered with size 1, only the last tick is guaranteed readable.
	clk.Advance(5 * time.Second)

	select {
	case <-tkr.C():
	default:
		t.Fatal("ticker should have fired at least once")
	}

	tkr.Stop()
}

func TestTickerStop(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tkr := clk.NewTicker(2 * time.Second)

	tkr.Stop()
	clk.Advance(10 * time.Second)

	select {
	case <-tkr.C():
		t.Fatal("stopped ticker should not fire")
	default:
	}
}

// TestTickerResets_CountsEveryReArm pins the signal a test needs when the
// goroutine under test changes its own cadence: the counter must move on each
// Reset, across every ticker on the clock, and must not move for a plain tick.
// Without it a test can only observe the side effect that precedes the Reset,
// advance into the gap, and lose the next tick.
func TestTickerResets_CountsEveryReArm(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	first := clk.NewTicker(2 * time.Second)
	second := clk.NewTicker(2 * time.Second)

	if got := clk.TickerResets(); got != 0 {
		t.Fatalf("TickerResets() on a fresh clock = %d, want 0", got)
	}

	clk.Advance(2 * time.Second)
	if got := clk.TickerResets(); got != 0 {
		t.Fatalf("TickerResets() after a tick = %d, want 0 (a fire is not a re-arm)", got)
	}

	first.Reset(5 * time.Second)
	if got := clk.TickerResets(); got != 1 {
		t.Fatalf("TickerResets() after one Reset = %d, want 1", got)
	}

	second.Reset(3 * time.Second)
	first.Reset(4 * time.Second)
	if got := clk.TickerResets(); got != 3 {
		t.Fatalf("TickerResets() after three Resets = %d, want 3", got)
	}
}

// TestTickerResets_ObservedReArmMakesAdvanceSafe reproduces the ordering that
// strands a tick. A ticker re-armed to a shorter period schedules from the
// clock's current time, so advancing by the new period only fires once the
// Reset has landed — which is exactly what TickerResets() lets a test await.
func TestTickerResets_ObservedReArmMakesAdvanceSafe(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	tkr := clk.NewTicker(10 * time.Second)

	clk.Advance(10 * time.Second)
	<-tkr.C()

	// Advancing by the shorter period BEFORE the re-arm steps over the stale
	// 20s deadline without firing.
	clk.Advance(5 * time.Second)
	select {
	case <-tkr.C():
		t.Fatal("ticker fired on the stale cadence")
	default:
	}

	tkr.Reset(5 * time.Second)
	if got := clk.TickerResets(); got != 1 {
		t.Fatalf("TickerResets() = %d, want 1", got)
	}
	clk.Advance(5 * time.Second)
	select {
	case <-tkr.C():
	default:
		t.Fatal("ticker did not fire on the re-armed cadence")
	}
}

// TestNowCalls_CountsEveryRead pins the barrier a test needs when the
// goroutine under test reads the clock after the side effect the test can
// observe. Every read counts, including the one Since makes, and advancing
// time is not itself a read.
func TestNowCalls_CountsEveryRead(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	if got := clk.NowCalls(); got != 0 {
		t.Fatalf("NowCalls() on a fresh clock = %d, want 0", got)
	}

	clk.Advance(time.Second)
	if got := clk.NowCalls(); got != 0 {
		t.Fatalf("NowCalls() after Advance = %d, want 0 (advancing is not a read)", got)
	}

	start := clk.Now()
	if got := clk.NowCalls(); got != 1 {
		t.Fatalf("NowCalls() after one Now = %d, want 1", got)
	}

	// Since reads the clock to compute the delta, so it counts too.
	clk.Advance(2 * time.Second)
	if elapsed := clk.Since(start); elapsed != 2*time.Second {
		t.Fatalf("Since(start) = %v, want 2s", elapsed)
	}
	if got := clk.NowCalls(); got != 2 {
		t.Fatalf("NowCalls() after Since = %d, want 2", got)
	}
}

func TestSystemClockNotNil(t *testing.T) {
	if clock.System == nil {
		t.Fatal("clock.System must not be nil")
	}
	now := clock.System.Now()
	if now.IsZero() {
		t.Fatal("clock.System.Now() returned zero time")
	}
}

// TestNewTicker_PanicsOnZero regresses a bug where the fake clock's
// NewTicker(0) would not panic (unlike time.NewTicker(0)) and instead
// caused Advance to enter an infinite loop trying to fire a zero-period
// ticker. Match the stdlib behavior of panicking.
func TestNewTicker_PanicsOnZero(t *testing.T) {
	for _, d := range []time.Duration{0, -1 * time.Millisecond, -1 * time.Hour} {
		t.Run(d.String(), func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewTicker(%v): expected panic, got none", d)
				}
			}()
			clk := clocktest.New()
			_ = clk.NewTicker(d)
		})
	}
}

// TestTickerReset_PanicsOnZero ensures Reset(0) on a ticker behaves like
// time.Ticker.Reset(0): panic. Otherwise Advance would loop forever.
func TestTickerReset_PanicsOnZero(t *testing.T) {
	for _, d := range []time.Duration{0, -1 * time.Millisecond} {
		t.Run(d.String(), func(t *testing.T) {
			clk := clocktest.New()
			tk := clk.NewTicker(1 * time.Millisecond)
			defer tk.Stop()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Reset(%v): expected panic, got none", d)
				}
			}()
			tk.Reset(d)
		})
	}
}

// TestRace_TickerResetVsAdvance regresses a real bug where fakeTicker.Reset
// wrote to ticker fields under a per-element lock while Fake.Advance read
// those same fields under only the Fake's lock — producing a data race
// flagged by -race in any test that called Reset on a ticker created from
// the fake clock. Run under -race; without the lock consolidation in
// fake.go this test fails on every run.
func TestRace_TickerResetVsAdvance(t *testing.T) {
	clk := clocktest.New()
	tk := clk.NewTicker(10 * time.Millisecond)
	defer tk.Stop()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tk.Reset(20 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clk.Advance(5 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-tk.C():
			}
		}
	}()

	time.Sleep(200 * time.Millisecond) // OTHER: real-time component of clocktest.Fake tests
	close(stop)
	wg.Wait()
}

// TestRace_TimerResetVsAdvance is the timer-side analogue of the ticker
// race: Reset re-arming a timer that has already fired must not race
// with concurrent Advance calls.
func TestRace_TimerResetVsAdvance(t *testing.T) {
	clk := clocktest.New()
	tm := clk.NewTimer(10 * time.Millisecond)
	defer tm.Stop()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tm.Reset(15 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clk.Advance(5 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-tm.C():
			}
		}
	}()

	time.Sleep(200 * time.Millisecond) // OTHER: real-time component of clocktest.Fake tests
	close(stop)
	wg.Wait()
}

// TestRace_StopVsAdvance regresses a related hazard: Stop on a timer
// or ticker concurrent with Advance must be race-free.
func TestRace_StopVsAdvance(t *testing.T) {
	clk := clocktest.New()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		tk := clk.NewTicker(time.Duration(5+i) * time.Millisecond)
		tm := clk.NewTimer(time.Duration(7+i) * time.Millisecond)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tk.Reset(time.Duration(3+i) * time.Millisecond)
				tk.Stop()
				tk.Reset(time.Duration(4+i) * time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tm.Reset(time.Duration(6+i) * time.Millisecond)
				tm.Stop()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clk.Advance(2 * time.Millisecond)
		}
	}()

	time.Sleep(200 * time.Millisecond) // OTHER: real-time component of clocktest.Fake tests
	close(stop)
	wg.Wait()
}
