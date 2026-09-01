package clocktest

import (
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// Fake is a manually-controlled clock for testing. Thread-safe.
// Time advances only when Advance is called; all timers and tickers
// fire synchronously during Advance.
//
// All state on the Fake — including every field of every fakeTimer
// and fakeTicker created from it — is guarded by f.mu. The per-element
// types do not have their own mutex; centralising the lock eliminates
// the lock-ordering hazards that arise when Advance walks the slices
// while Reset/Stop/fire mutate elements.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	tickers []*fakeTicker
}

// New returns a Fake clock starting at the current wall-clock time.
func New() *Fake { return NewAt(time.Now()) }

// NewAt returns a Fake clock starting at t.
func NewAt(t time.Time) *Fake {
	return &Fake{now: t}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration {
	return f.Now().Sub(t)
}

func (f *Fake) NewTimer(d time.Duration) clock.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()

	ft := &fakeTimer{
		ch:         make(chan time.Time, 1),
		deadline:   f.now.Add(d),
		clock:      f,
		registered: true,
	}
	f.timers = append(f.timers, ft)
	return ft
}

func (f *Fake) NewTicker(d time.Duration) clock.Ticker {
	if d <= 0 {
		panic("clocktest: non-positive interval for NewTicker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	ft := &fakeTicker{
		ch:         make(chan time.Time, 1),
		period:     d,
		nextTick:   f.now.Add(d),
		clock:      f,
		registered: true,
	}
	f.tickers = append(f.tickers, ft)
	return ft
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.NewTimer(d).C()
}

// TickerCount returns the number of active (non-stopped) tickers
// currently registered with the fake clock. Tests can use this to
// synchronise with background goroutines that create tickers on
// startup: spin on TickerCount() until it reaches the expected value
// before calling Advance, so no ticks are lost to a race between
// goroutine scheduling and time advancement.
func (f *Fake) TickerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tk := range f.tickers {
		if !tk.stopped {
			n++
		}
	}
	return n
}

// TickerPeriods returns the current period of every active ticker, in
// registration order.
//
// It is the Reset-aware companion to TickerCount. A goroutine that
// switches cadence mid-loop calls Ticker.Reset, which changes no count
// and consumes no channel — it leaves the test with nothing to
// synchronise on. Advancing by the NEW period before that Reset lands
// re-arms nextTick past the instant the test aimed at, the tick never
// fires, and the test hangs until its wait deadline. Spin here until
// the expected period appears, then Advance.
func (f *Fake) TickerPeriods() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, 0, len(f.tickers))
	for _, tk := range f.tickers {
		if !tk.stopped {
			out = append(out, tk.period)
		}
	}
	return out
}

// TimerCount returns the number of active (non-stopped, non-fired)
// timers currently registered with the fake clock. Useful for the
// same synchronisation pattern as TickerCount.
func (f *Fake) TimerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// Advance moves the fake clock forward by d, firing any timers whose
// deadline has been reached and tickers for each elapsed period.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	target := f.now.Add(d)

	// A timer that is stopped or has fired is retired from the list — it has
	// no further deadline to cross. Mark it de-registered so its own Reset
	// knows to put it back; a retired element that Reset cannot revive is a
	// clock that has silently stopped obeying the test.
	alive := f.timers[:0]
	for _, t := range f.timers {
		if t.stopped {
			t.registered = false
			continue
		}
		if !t.deadline.After(target) {
			t.fireLocked(t.deadline)
			t.registered = false
			continue
		}
		alive = append(alive, t)
	}
	f.timers = alive

	activeTickers := f.tickers[:0]
	for _, tk := range f.tickers {
		if tk.stopped {
			tk.registered = false
			continue
		}
		for !tk.nextTick.After(target) {
			tk.fireLocked(tk.nextTick)
			tk.nextTick = tk.nextTick.Add(tk.period)
		}
		activeTickers = append(activeTickers, tk)
	}
	f.tickers = activeTickers

	f.now = target
}

// ---------- fakeTimer ----------

// fakeTimer's fields are guarded by clock.mu (the parent Fake's mutex).
// All exported methods acquire clock.mu; internal *Locked variants
// assume the caller already holds it (used from Advance).
type fakeTimer struct {
	ch       chan time.Time
	deadline time.Time
	stopped  bool
	fired    bool
	// registered reports whether this timer is still in the Fake's list.
	// Advance retires a stopped or fired timer from that list; Reset uses
	// this to put it back rather than leaving a re-armed timer that no
	// Advance will ever look at again.
	registered bool
	clock      *Fake
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	wasActive := !t.stopped && !t.fired
	t.stopped = false
	t.fired = false
	t.deadline = t.clock.now.Add(d)
	// A one-shot timer's pending value belongs to the deadline the caller is
	// replacing, so dropping it is what Reset means (and matches time.Timer
	// since Go 1.23). Unlike a ticker there is nothing to lose: the new
	// deadline will fire on a later Advance.
	select {
	case <-t.ch:
	default:
	}
	// Advance retires a timer once it is stopped or has fired, so a Reset that
	// re-arms one has to put it back in the list. Without this the timer holds
	// a live deadline that no Advance will ever cross, and the loop waiting on
	// it never wakes.
	t.registerLocked()
	return wasActive
}

// registerLocked puts a retired timer back in the Fake's list.
// Caller MUST hold t.clock.mu.
func (t *fakeTimer) registerLocked() {
	if t.registered {
		return
	}
	t.registered = true
	t.clock.timers = append(t.clock.timers, t)
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := !t.stopped && !t.fired
	t.stopped = true
	return wasActive
}

// fireLocked sends the deadline timestamp on the timer's channel.
// Caller MUST hold t.clock.mu. Safe to call multiple times — only the
// first call has an effect; subsequent calls are no-ops.
func (t *fakeTimer) fireLocked(now time.Time) {
	if t.stopped || t.fired {
		return
	}
	t.fired = true
	select {
	case t.ch <- now:
	default:
	}
}

// ---------- fakeTicker ----------

// fakeTicker's fields are guarded by clock.mu (the parent Fake's mutex).
// All exported methods acquire clock.mu; internal *Locked variants
// assume the caller already holds it (used from Advance).
type fakeTicker struct {
	ch       chan time.Time
	period   time.Duration
	nextTick time.Time
	stopped  bool
	// registered mirrors fakeTimer.registered: Advance retires a stopped
	// ticker from the Fake's list, and Reset puts it back.
	registered bool
	clock      *Fake
}

func (tk *fakeTicker) C() <-chan time.Time { return tk.ch }

func (tk *fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clocktest: non-positive interval for Ticker.Reset")
	}
	tk.clock.mu.Lock()
	defer tk.clock.mu.Unlock()
	tk.period = d
	tk.stopped = false
	// Schedule the next tick one full period from the current fake
	// time. Without this, nextTick would keep its old value and the
	// next Advance would immediately fire on the old cadence — the
	// opposite of what Reset should do.
	tk.nextTick = tk.clock.now.Add(d)
	// A tick already sitting in the channel is DELIBERATELY kept, which is
	// where this fake parts company with time.Ticker.Reset.
	//
	// A loop that re-paces itself calls Reset at the end of the handler it is
	// running, while the test that drives it advances the clock from another
	// goroutine. The advance can therefore land mid-handler: the tick is
	// fired and buffered, and this Reset arrives after it. Real time.Ticker
	// can afford to discard that tick because wall time keeps flowing and the
	// next one is at most one period away. Here nothing else moves time — a
	// discarded tick is never re-delivered, so the loop and the test both wait
	// forever, and which of the two happens depends on a goroutine race. That
	// is exactly the non-determinism a fake clock exists to remove: a tick the
	// test has already advanced past is an event that HAPPENED, and Reset
	// changes only the cadence of the ones still to come.
	tk.registerLocked()
}

// registerLocked puts a retired ticker back in the Fake's list.
// Caller MUST hold tk.clock.mu.
func (tk *fakeTicker) registerLocked() {
	if tk.registered {
		return
	}
	tk.registered = true
	tk.clock.tickers = append(tk.clock.tickers, tk)
}

func (tk *fakeTicker) Stop() {
	tk.clock.mu.Lock()
	defer tk.clock.mu.Unlock()
	tk.stopped = true
}

// fireLocked sends the tick timestamp on the ticker's channel.
// Caller MUST hold tk.clock.mu.
func (tk *fakeTicker) fireLocked(now time.Time) {
	if tk.stopped {
		return
	}
	select {
	case tk.ch <- now:
	default:
	}
}
