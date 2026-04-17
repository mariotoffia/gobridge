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
		ch:       make(chan time.Time, 1),
		deadline: f.now.Add(d),
		clock:    f,
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
		ch:       make(chan time.Time, 1),
		period:   d,
		nextTick: f.now.Add(d),
		clock:    f,
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

	alive := f.timers[:0]
	for _, t := range f.timers {
		if t.stopped {
			continue
		}
		if !t.deadline.After(target) {
			t.fireLocked(t.deadline)
			continue
		}
		alive = append(alive, t)
	}
	f.timers = alive

	activeTickers := f.tickers[:0]
	for _, tk := range f.tickers {
		if tk.stopped {
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
	clock    *Fake
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	wasActive := !t.stopped && !t.fired
	wasFired := t.fired
	t.stopped = false
	t.fired = false
	t.deadline = t.clock.now.Add(d)
	select {
	case <-t.ch:
	default:
	}
	// If the timer had already fired, it was removed from the Fake's
	// timer list by a previous Advance. Re-register it so that
	// subsequent Advance calls can fire it again — matching the
	// behavior of the real runtime where time.Timer.Reset after Stop
	// or after firing re-arms the timer.
	if wasFired {
		t.clock.timers = append(t.clock.timers, t)
	}
	return wasActive
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
	clock    *Fake
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
	select {
	case <-tk.ch:
	default:
	}
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
