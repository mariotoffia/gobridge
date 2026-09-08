package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestWatchStreams_ReconcilesEmptyBaseline verifies a missing or version-zero
// initial Load is a real baseline: Load(0) -> peer CAS(1) -> LATEST -> deliver(1).
func TestWatchStreams_ReconcilesEmptyBaseline(t *testing.T) {
	for _, missing := range []bool{true, false} {
		name := "version zero"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			l, peer, streams, _ := newCursorTestLoader(t, !missing, 0)
			cfg, err := l.Load(t.Context())
			if missing {
				if !errors.Is(err, shared.ErrNotFound) {
					t.Fatalf("Load missing: %v", err)
				}
			} else if err != nil || cfg.Version != 0 {
				t.Fatalf("Load version zero: cfg=%v err=%v", cfg, err)
			}
			ch := startCursorWatch(t, l)
			wait.RequireReceive(t, streams.acquiring, time.Second)
			if err := peer.SaveIfVersion(t.Context(), &ports.BridgeConfig{}, 0); err != nil {
				t.Fatal(err)
			}
			finishCursorReconcile(t, streams)
			assertCursorVersion(t, ch, 1)
		})
	}
}

// TestWatchStreams_StoreObservationsDoNotAdvanceDelivery verifies reads and
// successful writes on the shared admin store cannot acknowledge watcher work.
// Initial Load(1) -> iterator gap -> Load/Save observes 2 -> LATEST -> deliver(2).
// A Save succeeds here with NO ConfigApplier, as when in-band apply fails.
func TestWatchStreams_StoreObservationsDoNotAdvanceDelivery(t *testing.T) {
	for _, operation := range []string{"load", "save", "save if version"} {
		for _, beforeWatch := range []bool{true, false} {
			stage := "iterator gap"
			if beforeWatch {
				stage = "before watch"
			}
			t.Run(operation+"/"+stage, func(t *testing.T) {
				l, peer, streams, fc := newCursorTestLoader(t, true, 1)
				if _, err := l.Load(t.Context()); err != nil {
					t.Fatal(err)
				}
				var ch <-chan *ports.BridgeConfig
				if !beforeWatch {
					ch = startCursorWatch(t, l)
					wait.RequireReceive(t, streams.acquiring, time.Second)
					finishCursorReconcile(t, streams)
					assertCursorSilent(t, ch)
					nextCursorGap(t, streams, fc, l.streamPollInterval)
				}
				observeCursorUpdate(t, operation, l, peer)
				if beforeWatch {
					ch = startCursorWatch(t, l)
					wait.RequireReceive(t, streams.acquiring, time.Second)
				}
				finishCursorReconcile(t, streams)
				assertCursorVersion(t, ch, 2)
				// Delivery, not the store observation, advances the cursor. A later
				// unchanged reacquisition must not emit the same config again.
				nextCursorGap(t, streams, fc, l.streamPollInterval)
				finishCursorReconcile(t, streams)
				assertCursorSilent(t, ch)
			})
		}
	}
}

// TestWatchPoll_StoreObservationsDoNotAdvanceDelivery verifies the poll cursor
// is seeded from the initial load, not a later admin read or successful write.
func TestWatchPoll_StoreObservationsDoNotAdvanceDelivery(t *testing.T) {
	for _, operation := range []string{"load", "save", "save if version"} {
		t.Run(operation, func(t *testing.T) {
			l, peer, _, fc := newCursorTestLoader(t, true, 1)
			l.mode = ModePoll
			if _, err := l.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			observeCursorUpdate(t, operation, l, peer)
			ch := startCursorWatch(t, l)
			fc.Advance(l.pollInterval)
			cfg := wait.RequireReceive(t, ch, time.Second)
			if cfg.Version != 2 {
				t.Fatalf("delivered version=%d, want 2", cfg.Version)
			}
		})
	}
}

// TestWatchStreams_InitialBaselineSemantics verifies never-loaded watchers do
// not synthesize an initial config, while a standalone Save can seed Watch.
func TestWatchStreams_InitialBaselineSemantics(t *testing.T) {
	for _, baseline := range []string{"never loaded", "load", "save", "save if version"} {
		t.Run(baseline, func(t *testing.T) {
			l, _, streams, _ := newCursorTestLoader(t, true, 1)
			cfg := &ports.BridgeConfig{}
			var err error
			switch baseline {
			case "load":
				_, err = l.Load(t.Context())
			case "save":
				err = l.Save(t.Context(), cfg)
			case "save if version":
				err = l.SaveIfVersion(t.Context(), cfg, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			ch := startCursorWatch(t, l)
			wait.RequireReceive(t, streams.acquiring, time.Second)
			finishCursorReconcile(t, streams)
			assertCursorSilent(t, ch)
		})
	}
}

func newCursorTestLoader(t *testing.T, exists bool, version int64) (*Loader, *Loader, *cursorStreams, *clocktest.Fake) {
	t.Helper()
	ddb := &cursorDDB{casFakeDDB: casFakeDDB{
		hasRow: exists, storedVersion: version, storedData: `{"bridge":{"id":"cursor"}}`, getReturnsVersion: -1,
	}}
	streams := &cursorStreams{
		fakeStreams: fakeStreams{closeAfterDrain: true},
		acquiring:   make(chan struct{}, 1), resume: make(chan struct{}, 1), reconciled: make(chan struct{}, 1),
	}
	fc := clocktest.NewAt(time.Unix(0, 0))
	l := NewLoader(nil, WithRegistry(ports.NewRegistry()), WithClock(fc), WithWatchMode(ModeStreams))
	l.session.ddb, l.session.streams = ddb, streams
	l.randFloat = func() float64 { return 0 }
	peer := NewLoader(nil, WithRegistry(ports.NewRegistry()))
	peer.session.ddb = ddb
	return l, peer, streams, fc
}

// TestWatchStreams_FirstLoadAfterWatchEstablishesBaseline verifies standalone
// callers can start Watch before their initial Load without losing later gaps.
func TestWatchStreams_FirstLoadAfterWatchEstablishesBaseline(t *testing.T) {
	l, peer, streams, _ := newCursorTestLoader(t, true, 1)
	ch := startCursorWatch(t, l)
	wait.RequireReceive(t, streams.acquiring, time.Second)
	if _, err := l.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := peer.SaveIfVersion(t.Context(), &ports.BridgeConfig{}, 1); err != nil {
		t.Fatal(err)
	}
	finishCursorReconcile(t, streams)
	assertCursorVersion(t, ch, 2)
}

// TestWatchStreams_PollFallbackKeepsDeliveryCursor verifies an admin observation
// during failed shard acquisition cannot become the fallback poll baseline.
func TestWatchStreams_PollFallbackKeepsDeliveryCursor(t *testing.T) {
	for _, operation := range []string{"load", "save", "save if version"} {
		t.Run(operation, func(t *testing.T) {
			l, peer, streams, fc := newCursorTestLoader(t, true, 1)
			streams.describeErr = errors.New("stream unavailable")
			if _, err := l.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			ch := startCursorWatch(t, l)
			wait.Until(t, time.Second, "first acquisition failure", func() bool {
				return streams.describeCalls.Load() == 1 && fc.TimerCount() == 1
			})
			observeCursorUpdate(t, operation, l, peer)
			for attempt := 1; attempt < streamAcquireFallbackAfter; attempt++ {
				wait.Until(t, time.Second, "acquisition backoff armed", func() bool {
					return streams.describeCalls.Load() >= int32(attempt) && fc.TimerCount() == 1
				})
				fc.Advance(maxStreamBackoff)
			}
			wait.Until(t, time.Second, "poll fallback started", func() bool { return fc.TickerCount() == 1 })
			fc.Advance(l.pollInterval)
			cfg := wait.RequireReceive(t, ch, time.Second)
			if cfg.Version != 2 {
				t.Fatalf("delivered version=%d, want 2", cfg.Version)
			}
		})
	}
}

func startCursorWatch(t *testing.T, l *Loader) <-chan *ports.BridgeConfig {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ch, err := l.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		wait.Until(t, time.Second, "config watcher stopped", func() bool {
			select {
			case _, ok := <-ch:
				return !ok
			default:
				return false
			}
		})
	})
	return ch
}

func observeCursorUpdate(t *testing.T, operation string, l, peer *Loader) {
	t.Helper()
	cfg := &ports.BridgeConfig{}
	var err error
	switch operation {
	case "load":
		if err = peer.SaveIfVersion(t.Context(), cfg, 1); err != nil {
			t.Fatal(err)
		}
		cfg, err = l.Load(t.Context())
	case "save":
		err = l.Save(t.Context(), cfg)
	case "save if version":
		err = l.SaveIfVersion(t.Context(), cfg, 1)
	}
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 2 {
		t.Fatalf("observed version=%d, want 2", cfg.Version)
	}
}

func finishCursorReconcile(t *testing.T, streams *cursorStreams) {
	t.Helper()
	streams.resume <- struct{}{}
	wait.RequireReceive(t, streams.reconciled, time.Second)
}

func nextCursorGap(t *testing.T, streams *cursorStreams, fc *clocktest.Fake, interval time.Duration) {
	t.Helper()
	wait.Until(t, time.Second, "stream wait armed", func() bool { return fc.TimerCount() == 1 })
	fc.Advance(interval)
	wait.RequireReceive(t, streams.acquiring, time.Second)
}

func assertCursorVersion(t *testing.T, ch <-chan *ports.BridgeConfig, want int) {
	t.Helper()
	select {
	case cfg := <-ch:
		if cfg == nil || cfg.Version != want {
			t.Fatalf("delivered config=%v, want version %d", cfg, want)
		}
	default:
		t.Fatalf("reconciliation completed without delivering version %d", want)
	}
}

func assertCursorSilent(t *testing.T, ch <-chan *ports.BridgeConfig) {
	t.Helper()
	select {
	case cfg := <-ch:
		t.Fatalf("unexpected config after reconciliation: %v", cfg)
	default:
	}
}
