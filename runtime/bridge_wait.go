package runtime

import (
	"context"
	"reflect"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// Event-driven waits over runtime state: route readiness and route quiescence.
// These are not health projections — they block a caller until the runtime
// reaches a state — so they live beside DeepHealth rather than inside it.
func (rt *Runtime) WaitRouteReady(ctx context.Context, routeID string) error {
	for {
		rt.mu.Lock()
		var runnerStarted, recvStarted <-chan struct{}
		var found bool
		for _, e := range rt.entries {
			if e.config.ID != routeID {
				continue
			}
			found = true
			if e.runner != nil {
				runnerStarted = e.runner.Started()
			}
			if rss, ok := e.receiver.(ports.ReceiverStartedSignaler); ok {
				recvStarted = rss.Started()
			}
			break
		}
		rt.mu.Unlock()

		if found {
			runnerReady := runnerStarted == nil || isClosed(runnerStarted)
			recvReady := recvStarted == nil || isClosed(recvStarted)
			if runnerReady && recvReady {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-runnerStarted:
		case <-recvStarted:
		case <-rt.clk.After(1 * time.Second):
		}
	}
}

// isClosed reports whether a close-once signal channel has been
// closed. Safe to call with a nil channel (returns false — callers
// treat nil as "no signal required" separately).
func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// QuiescenceOptions controls WaitQuiescent behaviour.
type QuiescenceOptions struct {
	Routes   []string      // only watch these routes (empty = all)
	MinQuiet time.Duration // zero defaults to 50ms; negative returns on the first all-zero snapshot
	Timeout  time.Duration // overall deadline (0 = rely on ctx)
}

// WaitQuiescent blocks until every watched route has zero InFlight
// deliveries for at least MinQuiet, or the context / Timeout expires.
// Event-driven: each watched RouteRunner's IdleChanged() channel fires
// on the InFlight → 0 transition. The quiet-window timer (clk.After)
// provides both the MinQuiet deadline and a sanity fallback.
func (rt *Runtime) WaitQuiescent(ctx context.Context, opts QuiescenceOptions) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	noQuietWindow := opts.MinQuiet < 0
	if opts.MinQuiet == 0 {
		opts.MinQuiet = 50 * time.Millisecond
	}

	quietSince := time.Time{}
	for {
		// Capture idle channels AND inFlight snapshot under rt.mu.
		// Channels must be captured before reading InFlight to avoid
		// a lost-wakeup race (see OutboxDrainer.WaitIdle).
		rt.mu.Lock()
		idleChs := make([]<-chan struct{}, 0, len(rt.entries))
		allZero := true
		for _, e := range rt.entries {
			if e.runner == nil {
				continue
			}
			if !routeWatched(opts.Routes, e.config.ID) {
				continue
			}
			idleChs = append(idleChs, e.runner.IdleChanged())
			if e.runner.InFlight() > 0 {
				allZero = false
			}
		}
		rt.mu.Unlock()

		if allZero {
			if noQuietWindow {
				return nil
			}
			if quietSince.IsZero() {
				quietSince = rt.clk.Now()
			}
			if rt.clk.Since(quietSince) >= opts.MinQuiet {
				return nil
			}
		} else {
			quietSince = time.Time{}
		}

		// Sanity/quiet-window timer: when we are currently quiet, sleep
		// the remaining window; otherwise fall back to MinQuiet so we
		// re-check on routes that never fire an idle transition.
		sanity := opts.MinQuiet
		if noQuietWindow {
			sanity = 50 * time.Millisecond
		}
		if !quietSince.IsZero() {
			sanity = opts.MinQuiet - rt.clk.Since(quietSince)
			if sanity <= 0 {
				continue
			}
		}

		cases := make([]reflect.SelectCase, 0, len(idleChs)+2)
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		})
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(rt.clk.After(sanity)),
		})
		for _, ch := range idleChs {
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(ch),
			})
		}
		chosen, _, _ := reflect.Select(cases)
		if chosen == 0 {
			return ctx.Err()
		}
	}
}

// routeWatched reports whether routeID is in the watched set. An
// empty watched slice means "all routes".
func routeWatched(watched []string, routeID string) bool {
	if len(watched) == 0 {
		return true
	}
	return slices.Contains(watched, routeID)
}

// minServiceLevel returns the lower of two service levels.
// Order: None < Degraded < Full. Empty string is treated as None.
