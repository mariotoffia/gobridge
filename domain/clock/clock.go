package clock

import "time"

// Clock abstracts time operations so production code can be tested
// deterministically. Every wait MUST be expressed as
// `select { case <-ctx.Done(): case <-clk.After(d): }` — there is no
// Sleep method by design to enforce cancellability at the type level.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	After(d time.Duration) <-chan time.Time
}

type Timer interface {
	C() <-chan time.Time
	Reset(d time.Duration) bool
	Stop() bool
}

type Ticker interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

// System is the real clock backed by the time package.
var System Clock = realClock{}
