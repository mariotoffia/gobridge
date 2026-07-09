package bridge

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// ReconfigStrategy filters a raw config change channel, controlling
// when the Supervisor triggers a rebuild. Implementations receive the
// upstream channel and return a new channel that emits configs at the
// desired cadence. The returned channel must close when ctx is
// cancelled or the input channel closes.
type ReconfigStrategy interface {
	Filter(ctx context.Context, in <-chan *ports.BridgeConfig) <-chan *ports.BridgeConfig
}

// DirectStrategy passes every config change through immediately.
type DirectStrategy struct{}

// NewDirectStrategy returns a strategy that applies every config
// change as soon as it arrives.
func NewDirectStrategy() *DirectStrategy { return &DirectStrategy{} }

// Filter forwards each incoming config without delay.
func (s *DirectStrategy) Filter(ctx context.Context, in <-chan *ports.BridgeConfig) <-chan *ports.BridgeConfig {
	out := make(chan *ports.BridgeConfig, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case cfg, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- cfg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// DebouncedStrategy waits for a quiet period with no new changes
// before emitting the latest config. Each incoming change resets
// the timer.
type DebouncedStrategy struct {
	quietPeriod time.Duration
	clk         clock.Clock
}

// NewDebouncedStrategy creates a debounce strategy. If quietPeriod
// is zero or negative it behaves like DirectStrategy.
// An optional Clock can be provided for testing; nil defaults to
// clock.System.
func NewDebouncedStrategy(quietPeriod time.Duration, clk clock.Clock) *DebouncedStrategy {
	if clk == nil {
		clk = clock.System
	}
	return &DebouncedStrategy{quietPeriod: quietPeriod, clk: clk}
}

// Filter debounces incoming configs, emitting only after quietPeriod
// of silence. Only the latest config is kept; intermediate values
// are discarded.
func (s *DebouncedStrategy) Filter(ctx context.Context, in <-chan *ports.BridgeConfig) <-chan *ports.BridgeConfig {
	if s.quietPeriod <= 0 {
		return NewDirectStrategy().Filter(ctx, in)
	}

	out := make(chan *ports.BridgeConfig, 1)
	go func() {
		defer close(out)

		var pending *ports.BridgeConfig
		// Go 1.23+ timer semantics (go.mod declares go 1.25): after Stop()/Reset()
		// returns, the channel yields no stale value, so the historical
		// `if !timer.Stop() { <-timer.C() }` drain is unnecessary AND unsafe —
		// it can block forever when the fire raced the Stop (reconfig_strategy.go:92,
		// Chunk 3). Create the timer stopped; Reset re-arms it before each use.
		timer := s.clk.NewTimer(s.quietPeriod)
		timer.Stop()

		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case cfg, ok := <-in:
				if !ok {
					// Input closed: flush the final debounced config before
					// exiting instead of dropping it with a best-effort
					// non-blocking send (Finding 11). A change that arrived
					// just before close would otherwise be silently lost.
					if pending != nil {
						timer.Stop()
						select {
						case out <- pending:
						case <-ctx.Done():
						}
					}
					return
				}
				pending = cfg
				timer.Reset(s.quietPeriod)
			case <-timer.C():
				if pending != nil {
					select {
					case out <- pending:
					case <-ctx.Done():
						return
					}
					pending = nil
				}
			}
		}
	}()
	return out
}

// WindowedStrategy combines debounce with a maximum delay cap.
// It waits for a quiet period like DebouncedStrategy, but if
// changes keep arriving it forces an emit after maxDelay
// regardless.
type WindowedStrategy struct {
	quietPeriod time.Duration
	maxDelay    time.Duration
	clk         clock.Clock
}

// NewWindowedStrategy creates a windowed strategy. If quietPeriod is
// zero it behaves like DirectStrategy. maxDelay must be positive;
// if less than quietPeriod it takes precedence.
// An optional Clock can be provided for testing; nil defaults to
// clock.System.
func NewWindowedStrategy(quietPeriod, maxDelay time.Duration, clk clock.Clock) *WindowedStrategy {
	if clk == nil {
		clk = clock.System
	}
	return &WindowedStrategy{quietPeriod: quietPeriod, maxDelay: maxDelay, clk: clk}
}

// Filter debounces incoming configs with a hard deadline. If the
// quiet window never opens, the latest config is emitted after
// maxDelay from the first change in the current batch.
func (s *WindowedStrategy) Filter(ctx context.Context, in <-chan *ports.BridgeConfig) <-chan *ports.BridgeConfig {
	if s.quietPeriod <= 0 {
		return NewDirectStrategy().Filter(ctx, in)
	}

	out := make(chan *ports.BridgeConfig, 1)
	go func() {
		defer close(out)

		var pending *ports.BridgeConfig
		// Go 1.23+ timer semantics: no manual channel drain after Stop()/Reset()
		// (reconfig_strategy.go:92, Chunk 3). Both timers are created stopped and
		// re-armed via Reset when a batch opens.
		quietTimer := s.clk.NewTimer(s.quietPeriod)
		quietTimer.Stop()

		maxTimer := s.clk.NewTimer(s.quietPeriod)
		maxTimer.Stop()

		emit := func() {
			if pending == nil {
				return
			}
			select {
			case out <- pending:
			case <-ctx.Done():
			}
			pending = nil
			quietTimer.Stop()
			maxTimer.Stop()
		}

		for {
			select {
			case <-ctx.Done():
				quietTimer.Stop()
				maxTimer.Stop()
				return
			case cfg, ok := <-in:
				if !ok {
					// Input closed: flush the final pending config with a
					// blocking send (ctx-guarded) so a change batched just
					// before close is not dropped (Finding 11).
					if pending != nil {
						quietTimer.Stop()
						maxTimer.Stop()
						select {
						case out <- pending:
						case <-ctx.Done():
						}
					}
					return
				}
				firstInBatch := pending == nil
				pending = cfg
				quietTimer.Reset(s.quietPeriod)

				if firstInBatch && s.maxDelay > 0 {
					maxTimer.Reset(s.maxDelay)
				}
			case <-quietTimer.C():
				emit()
			case <-maxTimer.C():
				emit()
			}
		}
	}()
	return out
}
