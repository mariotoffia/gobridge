// Package clock abstracts time so production code can be tested
// deterministically. Production code MUST take a [Clock] dependency
// rather than calling time.Now / time.After / time.NewTimer /
// time.NewTicker / time.Sleep / time.Since / time.Until directly;
// these primitives are gated by golangci-lint's forbidigo rule.
//
// # Cancellable waits
//
// The [Clock] interface deliberately omits a Sleep method. Sleeping
// without a context is a bug: callers cannot be cancelled and tests
// hang. Use the cancellable idiom instead:
//
//	select {
//	case <-ctx.Done():
//		return ctx.Err()
//	case <-clk.After(d):
//		// proceed
//	}
//
// For repeated ticks use [Clock.NewTicker]; for one-shot timers use
// [Clock.NewTimer] (both expose a Stop method to release resources).
//
// # Usage
//
// Constructors should take a [Clock] (typically via a functional
// option) and default to [System] when none is provided. Options that
// accept a clock MUST guard against nil so a caller passing nil does
// not clobber the [System] default:
//
//	func WithClock(clk clock.Clock) Option {
//	    return func(c *Component) {
//	        if clk != nil {
//	            c.clk = clk
//	        }
//	    }
//	}
//
// Tests use [github.com/mariotoffia/gobridge/domain/clock/clocktest.Fake]
// to drive deterministic time advancement.
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
