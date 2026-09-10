package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// expireProbeStore records the fencing token and the context shape each guarded
// housekeeping call receives. It backs two contracts:
//
//   - the bulk expiry sweep is lease-fenced end to end (the drainer's live token
//     reaches the store, so the store can reject a preempted owner);
//   - Expire and CountPending are bounded like Claim, so a black-holed store
//     cannot park the only drainer for this partition in housekeeping work.
type expireProbeStore struct {
	expireToken       atomic.Value // persistence.LeaseToken
	expireCalls       atomic.Int32
	expireHadDeadline atomic.Bool
	countCalls        atomic.Int32
	countHadDeadline  atomic.Bool
}

func (s *expireProbeStore) Persist(context.Context, []*persistence.OutboxRecord) error { return nil }

func (s *expireProbeStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *expireProbeStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (s *expireProbeStore) Expire(ctx context.Context, _ time.Time, _ string, token persistence.LeaseToken) (int, error) {
	s.expireToken.Store(token)
	if _, ok := ctx.Deadline(); ok {
		s.expireHadDeadline.Store(true)
	}
	s.expireCalls.Add(1)
	return 0, nil
}

func (s *expireProbeStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *expireProbeStore) CountPending(ctx context.Context, _ string) (int, error) {
	if _, ok := ctx.Deadline(); ok {
		s.countHadDeadline.Store(true)
	}
	s.countCalls.Add(1)
	return 0, nil
}

var (
	_ ports.OutboxStore         = (*expireProbeStore)(nil)
	_ ports.OutboxDepthReporter = (*expireProbeStore)(nil)
)

// newHousekeepingDrainer builds a drop-policy drainer whose expiry sweep is due:
// New seeds lastExpire to the construction instant so the first sweep waits a
// full expireInterval, so the caller advances the fake clock past it exactly as
// the sweep-policy tests do.
func newHousekeepingDrainer(store ports.OutboxStore, clk *clocktest.Fake) *Drainer {
	d := New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-housekeeping",
		PartitionKey: "SESSION#sess-housekeeping",
		Policy: routing.RoutePolicy{
			OnExpired:         routing.ExpiredDrop,
			MaxReplayAttempts: 5,
			SendTimeout:       2 * time.Second,
		},
		Clock:   clk,
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
	clk.Advance(2 * time.Minute)
	return d
}

// TestDrainer_ExpireCarriesLeaseToken pins that the drainer hands the token its
// OWN TokenFn returned to the bulk expiry sweep. Expire terminally destroys
// pending records a successor could still deliver, so the store must be able to
// refuse a preempted owner — which it can only do if the live token reaches it.
//
// It drives Run rather than calling maybeExpire directly: passing a token into
// maybeExpire and asserting the store saw that same value would only prove a
// function forwards its own parameter, and would stay green if Run were changed
// to sweep with a zero-value token.
//
// Mutation check: pass persistence.LeaseToken{} at Run's maybeExpire call site
// and this fails.
func TestDrainer_ExpireCarriesLeaseToken(t *testing.T) {
	store := &expireProbeStore{}
	clk := clocktest.NewAt(time.Unix(0, 0))
	liveToken := persistence.LeaseToken{Version: 42, Owner: "owner-live"}
	d := New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-livetoken",
		PartitionKey: "SESSION#sess-livetoken",
		Policy: routing.RoutePolicy{
			OnExpired:         routing.ExpiredDrop,
			MaxReplayAttempts: 5,
			SendTimeout:       2 * time.Second,
		},
		Clock: clk,
		// Only Run consults this; a drainer swept with any other token is a bug.
		TokenFn: func() (persistence.LeaseToken, bool) { return liveToken, true },
		// Never ready: Run reaches the sweep and stops before the claim path, so
		// the sweep is the only store call this test can observe.
		ReadyFn: func(context.Context) bool { return false },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()

	// Run's poll timer comes from the injected clock, so wait for the loop to
	// register it before advancing — otherwise the advance lands before the
	// timer exists and the loop never fires. The advance then clears both the
	// poll interval and the expireInterval lastExpire was seeded with.
	wait.Until(t, 5*time.Second, "Run registered its poll timer", func() bool {
		return clk.TimerCount() > 0
	})
	clk.Advance(2 * time.Minute)
	wait.Until(t, 5*time.Second, "Run performed its expiry sweep", func() bool {
		return store.expireCalls.Load() > 0
	})
	cancel()
	<-done

	got, _ := store.expireToken.Load().(persistence.LeaseToken)
	if got != liveToken {
		t.Fatalf("Expire token = %+v, want the token Run's TokenFn returned %+v", got, liveToken)
	}
}

// TestDrainer_ExpireReceivesBoundedContext pins that the expiry sweep runs under
// an operation deadline. The sweep holds the single drain goroutine for this
// partition, so an unbounded call against a black-holed store stops every drain
// cycle — including the send path — for as long as the store stays silent.
//
// Mutation check: pass the loop context straight to Expire and this fails — the
// store receives a deadline-less context.
func TestDrainer_ExpireReceivesBoundedContext(t *testing.T) {
	store := &expireProbeStore{}
	d := newHousekeepingDrainer(store, clocktest.NewAt(time.Unix(0, 0)))

	if err := d.maybeExpire(context.Background(), deferredTestToken()); err != nil {
		t.Fatalf("maybeExpire: %v", err)
	}

	if store.expireCalls.Load() == 0 {
		t.Fatal("Expire was never called; the test did not exercise the bounded-context path")
	}
	if !store.expireHadDeadline.Load() {
		t.Fatal("Expire received a deadline-less context; the sweep is unbounded")
	}
}

// TestDrainer_CountPendingReceivesBoundedContext pins the same bound on the
// depth query. It runs on every drain cycle purely to feed a gauge, so a store
// that never answers must not be able to hold the drainer hostage for a metric.
//
// Mutation check: pass the loop context straight to CountPending and this fails.
func TestDrainer_CountPendingReceivesBoundedContext(t *testing.T) {
	store := &expireProbeStore{}
	d := newHousekeepingDrainer(store, clocktest.NewAt(time.Unix(0, 0)))

	d.emitDepth(context.Background(), 0)

	if store.countCalls.Load() == 0 {
		t.Fatal("CountPending was never called; the test did not exercise the bounded-context path")
	}
	if !store.countHadDeadline.Load() {
		t.Fatal("CountPending received a deadline-less context; the depth query is unbounded")
	}
}

// blackHoledHousekeepingStore never answers Expire or CountPending until its
// context ends — the store-outage shape the operation deadline exists for.
type blackHoledHousekeepingStore struct {
	expireProbeStore
}

func (s *blackHoledHousekeepingStore) Expire(ctx context.Context, _ time.Time, _ string, _ persistence.LeaseToken) (int, error) {
	<-ctx.Done()
	s.expireCalls.Add(1)
	return 0, ctx.Err()
}

func (s *blackHoledHousekeepingStore) CountPending(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	s.countCalls.Add(1)
	return 0, ctx.Err()
}

// TestDrainer_BlackHoledHousekeepingReturns proves the bound is effective, not
// merely present: with a store that answers neither call, both return and the
// drainer goroutine is free to run the next cycle. Without the deadline both
// calls block until the process exits and the test times out.
func TestDrainer_BlackHoledHousekeepingReturns(t *testing.T) {
	store := &blackHoledHousekeepingStore{}
	clk := clocktest.NewAt(time.Unix(0, 0))
	d := New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-blackhole",
		PartitionKey: "SESSION#sess-blackhole",
		Policy: routing.RoutePolicy{
			OnExpired:         routing.ExpiredDrop,
			MaxReplayAttempts: 5,
			// A short real budget: the deadline fires on the wall clock inside
			// context.WithTimeout, which no injected clock can drive.
			SendTimeout: 20 * time.Millisecond,
		},
		Clock:   clk,
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
	clk.Advance(2 * time.Minute)

	done := make(chan struct{})
	var expireErr error
	go func() {
		defer close(done)
		expireErr = d.maybeExpire(context.Background(), deferredTestToken())
		d.emitDepth(context.Background(), 0)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("black-holed Expire/CountPending never returned; the drainer is pinned in housekeeping")
	}

	// The sweep must fail with the DEADLINE, not merely return: a store that
	// answered normally would prove nothing about the bound.
	if !errors.Is(expireErr, context.DeadlineExceeded) {
		t.Fatalf("expire error = %v, want context.DeadlineExceeded from the operation bound", expireErr)
	}

	if store.expireCalls.Load() == 0 {
		t.Error("Expire never returned through its deadline")
	}
	if store.countCalls.Load() == 0 {
		t.Error("CountPending never returned through its deadline")
	}
}

// partialExpireStore reports records it DID expire alongside a failure — the
// shape a sweep truncated by its deadline or aborted mid-way by a fence raise
// returns.
type partialExpireStore struct {
	expireProbeStore
	expired   int
	expireErr error
	claims    atomic.Int32
}

func (s *partialExpireStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	s.expireCalls.Add(1)
	return s.expired, s.expireErr
}

func (s *partialExpireStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	s.claims.Add(1)
	return nil, nil
}

// TestDrainer_ExpirePartialCountIsCountedOnFailure pins that records a failed
// sweep already expired are still counted. Those transitions are durable and
// terminal, so dropping the count would silently break the conservation law the
// metric exists to close (received = sent + dropped + filtered + expired + dlq
// + inflight).
//
// Mutation check: move the MessagesExpired emission below the error return and
// this fails with 0.
func TestDrainer_ExpirePartialCountIsCountedOnFailure(t *testing.T) {
	store := &partialExpireStore{expired: 3, expireErr: shared.ErrStaleFencingToken}
	metrics := newRecordingExporter()
	clk := clocktest.NewAt(time.Unix(0, 0))
	d := New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-partial",
		PartitionKey: "SESSION#sess-partial",
		Policy: routing.RoutePolicy{
			OnExpired:         routing.ExpiredDrop,
			MaxReplayAttempts: 5,
			SendTimeout:       time.Second,
		},
		Clock:   clk,
		Metrics: metrics,
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
	clk.Advance(2 * time.Minute)

	err := d.maybeExpire(context.Background(), deferredTestToken())

	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("maybeExpire must surface the store error, got %v", err)
	}
	if got := metrics.sum(shared.MetricMessagesExpired, nil); got != 3 {
		t.Fatalf("MessagesExpired = %d, want 3 (a failed sweep still expired 3 records durably)", got)
	}
}

// TestDrainer_ExpireStaleTokenSkipsDrainCycle pins that a sweep refused by the
// store stops the cycle. A successor owns the partition, so continuing into
// Claim would only produce a second rejection — and on a drainer whose egress
// never becomes ready Claim is never reached at all, making this the only place
// the takeover is ever observed.
//
// Mutation check: drop the ErrStaleFencingToken branch at Run's maybeExpire call
// site and this fails — the cycle proceeds and Claim is attempted.
func TestDrainer_ExpireStaleTokenSkipsDrainCycle(t *testing.T) {
	store := &partialExpireStore{expireErr: shared.ErrStaleFencingToken}
	clk := clocktest.NewAt(time.Unix(0, 0))
	d := New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-staleexpire",
		PartitionKey: "SESSION#sess-staleexpire",
		Policy: routing.RoutePolicy{
			OnExpired:         routing.ExpiredDrop,
			MaxReplayAttempts: 5,
			SendTimeout:       time.Second,
		},
		Clock: clk,
		// Egress IS ready, so only the stale-expiry branch can stop the cycle.
		ReadyFn: func(context.Context) bool { return true },
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()

	wait.Until(t, 5*time.Second, "Run registered its poll timer", func() bool {
		return clk.TimerCount() > 0
	})
	clk.Advance(2 * time.Minute)
	wait.Until(t, 5*time.Second, "Run attempted the expiry sweep", func() bool {
		return store.expireCalls.Load() > 0
	})
	cancel()
	<-done

	if got := store.claims.Load(); got != 0 {
		t.Fatalf("Claim attempted %d times after a refused sweep; the cycle must back off instead", got)
	}
}
