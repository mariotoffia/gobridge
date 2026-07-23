package outbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// ---------------------------------------------------------------------------
// Production-readiness fixes: H1 (expire-sweep policy gate), H3 (drop-vs-DLQ
// gate on permanent/poison), M3 (OnSettled after Complete), M4 (release-failure
// stops the group), Min1 (empty drainer reports idle). All white-box against
// drainBatch/maybeExpire with an injected fake clock — no wall-clock waits.
// ---------------------------------------------------------------------------

// recordingExporter captures Counter increments so tests can assert exact
// per-metric, per-tag totals (e.g. MessagesDropped by reason, DLQEntries by
// category). All other metric methods are the embedded NoopExporter. Safe for
// the concurrent drain goroutines that emit during drainBatch.
type recordingExporter struct {
	*ports.NoopExporter
	mu    sync.Mutex
	calls []counterCall
}

type counterCall struct {
	name  string
	value int64
	tags  map[string]string
}

func newRecordingExporter() *recordingExporter {
	return &recordingExporter{NoopExporter: &ports.NoopExporter{}}
}

func (e *recordingExporter) Counter(name string, value int64, tags ...shared.Tag) {
	tm := make(map[string]string, len(tags))
	for _, t := range tags {
		tm[t.Key] = t.Value
	}
	e.mu.Lock()
	e.calls = append(e.calls, counterCall{name: name, value: value, tags: tm})
	e.mu.Unlock()
}

// sum totals every Counter increment for metric name whose tags include all of
// want's key/value pairs (want=nil matches any tags).
func (e *recordingExporter) sum(name string, want map[string]string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total int64
	for _, c := range e.calls {
		if c.name != name {
			continue
		}
		match := true
		for k, v := range want {
			if c.tags[k] != v {
				match = false
				break
			}
		}
		if match {
			total += c.value
		}
	}
	return total
}

// settleCountingHook counts OnSettled invocations so M3 can assert the terminal
// hook fires exactly once across a Complete-fails-then-succeeds retry.
type settleCountingHook struct {
	ports.NoopDeliveryHook
	settled atomic.Int64
}

func (h *settleCountingHook) OnSettled(context.Context, ports.DeliveryOutcome) {
	h.settled.Add(1)
}

func (h *settleCountingHook) count() int64 { return h.settled.Load() }

// expiredSnapshot builds a rehydrated pending record whose envelope carries an
// absolute expiry, so processRecord's HasExpiry/IsExpired gate routes it through
// handleExpired.
func expiredSnapshot(id, sessionID string, expiresAt, createdAt time.Time) persistence.OutboxSnapshot {
	return persistence.OutboxSnapshot{
		ID:         id,
		RouteID:    "route-exp",
		EnvelopeID: "env-" + id,
		BindingID:  "bind-exp",
		SessionID:  sessionID,
		Address:    "test/topic",
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:        "env-" + id,
			Payload:   []byte("payload"),
			ExpiresAt: expiresAt,
		}),
		Status:    persistence.OutboxPending,
		CreatedAt: createdAt,
	}
}

// newExpireDrainer builds a drainer over the given OnExpired policy for the H1
// maybeExpire subtests. PartitionKey is fixed so the scoped-sweep assertion is
// exact.
func newExpireDrainer(store ports.OutboxStore, onExpired routing.ExpiredAction, clk *clocktest.Fake) *Drainer {
	return New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-exp",
		PartitionKey: "SESSION#sess-exp",
		Policy:       routing.RoutePolicy{OnExpired: onExpired, MaxReplayAttempts: 5, SendTimeout: time.Second},
		Clock:        clk,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
}

// newDropDrainer builds a drainer under OnPermanentFailure=drop AND a router
// with no store, so BOTH legs of the H3 gate (drop policy, no store) hold.
func newDropDrainer(store ports.OutboxStore, sender ports.Sender, clk *clocktest.Fake, metrics ports.MetricsExporter) *Drainer {
	return New(Config{
		OutboxStore:  store,
		Sender:       sender,
		DLQ:          dlq.New(nil), // HasStore()==false
		RouteID:      "route-drop",
		PartitionKey: "SESSION#sess-drop",
		Policy: routing.RoutePolicy{
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
			MaxReplayAttempts:  5,
			SendTimeout:        time.Second,
		},
		Clock:   clk,
		Metrics: metrics,
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
}

// ---------------------------------------------------------------------------
// H1: the bulk expire sweep is deferred for on_expired:dlq routes (so expiry
// flows through the claim path handleExpired, preserving the envelope), and
// runs — scoped to the drainer's partition — only for drop-policy routes.
// ---------------------------------------------------------------------------

func TestDrainer_ExpireSweep_SkippedForDLQPolicy(t *testing.T) {
	t.Run("dlq policy defers the bulk sweep entirely", func(t *testing.T) {
		clk := clocktest.NewAt(budgetBase)
		store := &deferredFakeStore{expireCount: 3}
		d := newExpireDrainer(store, routing.ExpiredDLQ, clk)
		// Past the 1m expireInterval seed so the ONLY thing that could stop the
		// sweep is the dlq-policy gate.
		clk.Advance(2 * time.Minute)

		d.maybeExpire(context.Background())

		if got := store.expiredPartitions(); len(got) != 0 {
			t.Errorf("Expire called %v, want none: dlq-policy expiry must flow through handleExpired, not the sweep", got)
		}
	})

	t.Run("drop policy runs the sweep scoped to the partition", func(t *testing.T) {
		clk := clocktest.NewAt(budgetBase)
		store := &deferredFakeStore{expireCount: 3}
		d := newExpireDrainer(store, routing.ExpiredDrop, clk)
		clk.Advance(2 * time.Minute)

		d.maybeExpire(context.Background())

		got := store.expiredPartitions()
		if len(got) != 1 || got[0] != "SESSION#sess-exp" {
			t.Errorf("Expire scoped to %v, want exactly [SESSION#sess-exp] (M1: partition-scoped sweep)", got)
		}
	})
}

// TestDrainer_ClaimPath_ExpiredRecordRoutedToDLQ pins that for a dlq-policy
// route the per-record claim path (handleExpired) preserves the expired
// envelope by routing it to the DLQ and completing the record — the evidence
// the bulk sweep would have destroyed (H1).
func TestDrainer_ClaimPath_ExpiredRecordRoutedToDLQ(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := persistence.RehydrateFromSnapshot(expiredSnapshot("rec-exp", "sess-exp",
		budgetBase.Add(-time.Minute), budgetBase.Add(-10*time.Minute)))
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
	dlqStore := &fakeDLQStore{}
	metrics := newRecordingExporter()

	var sent int32
	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-exp",
		PartitionKey: "SESSION#sess-exp",
		Policy: routing.RoutePolicy{
			OnExpired:          routing.ExpiredDLQ,
			OnPermanentFailure: routing.FailureDLQ,
			MaxReplayAttempts:  5,
			SendTimeout:        time.Second,
		},
		Clock:   clk,
		Metrics: metrics,
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch err = %v, want nil", err)
	}
	if success != 1 {
		t.Errorf("success = %d, want 1 (expired record settled via DLQ)", success)
	}
	if n := atomic.LoadInt32(&sent); n != 0 {
		t.Errorf("sender calls = %d, want 0 (expired decided before any send)", n)
	}
	if got := dlqStore.writes(); got != 1 {
		t.Errorf("DLQ writes = %d, want 1 (expired envelope preserved to DLQ)", got)
	}
	if got := metrics.sum(shared.MetricDLQEntries, map[string]string{shared.TagKeyCategory: "expired"}); got != 1 {
		t.Errorf("DLQEntries[expired] = %d, want 1", got)
	}
	if got := store.completedIDs(); len(got) != 1 || got[0] != "rec-exp" {
		t.Errorf("completed = %v, want [rec-exp]", got)
	}
}

// ---------------------------------------------------------------------------
// H3: permanent and poison terminal handling honors OnPermanentFailure=drop and
// a missing DLQ store — dropping-with-metric (MessagesDropped) instead of
// writing a DLQ entry the operator opted out of (or counting a DLQ entry with
// no store behind it).
// ---------------------------------------------------------------------------

func TestDrainer_PermanentAndPoison_DropWhenDropPolicyNoStore(t *testing.T) {
	t.Run("permanent send error drops with metric, no DLQ", func(t *testing.T) {
		clk := clocktest.NewAt(budgetBase)
		rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-drop-perm", 1, budgetBase, budgetBase))
		store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
		metrics := newRecordingExporter()

		var sent int32
		sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
			atomic.AddInt32(&sent, 1)
			return shared.NewBridgeError("DENIED", shared.ErrorPermanent, "not authorized")
		}}
		d := newDropDrainer(store, sender, clk, metrics)

		success, _, err := d.drainBatch(context.Background(), deferredTestToken())
		if err != nil {
			t.Fatalf("drainBatch err = %v, want nil", err)
		}
		if success != 1 {
			t.Errorf("success = %d, want 1 (dropped record is completed)", success)
		}
		if n := atomic.LoadInt32(&sent); n != 1 {
			t.Errorf("sender calls = %d, want 1 (permanent send attempted once)", n)
		}
		if got := metrics.sum(shared.MetricMessagesDropped, map[string]string{shared.TagKeyReason: "permanent"}); got != 1 {
			t.Errorf("MessagesDropped[permanent] = %d, want 1", got)
		}
		if got := metrics.sum(shared.MetricDLQEntries, nil); got != 0 {
			t.Errorf("DLQEntries = %d, want 0 (drop policy / no store must never DLQ)", got)
		}
		if got := store.completedIDs(); len(got) != 1 || got[0] != "rec-drop-perm" {
			t.Errorf("completed = %v, want [rec-drop-perm]", got)
		}
	})

	t.Run("poison drops with metric, no DLQ", func(t *testing.T) {
		clk := clocktest.NewAt(budgetBase)
		// Replay COUNT exhausted (6 > 5) AND wall-clock budget spent (16m >= 15m).
		rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-drop-poison", 6,
			budgetBase.Add(-16*time.Minute), budgetBase.Add(-30*time.Minute)))
		store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
		metrics := newRecordingExporter()

		var sent int32
		sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
			atomic.AddInt32(&sent, 1)
			return nil
		}}
		d := newDropDrainer(store, sender, clk, metrics)

		success, _, err := d.drainBatch(context.Background(), deferredTestToken())
		if err != nil {
			t.Fatalf("drainBatch err = %v, want nil", err)
		}
		if success != 1 {
			t.Errorf("success = %d, want 1 (poison drop completes the record)", success)
		}
		if n := atomic.LoadInt32(&sent); n != 0 {
			t.Errorf("sender calls = %d, want 0 (poison decided before send)", n)
		}
		if got := metrics.sum(shared.MetricMessagesDropped, map[string]string{shared.TagKeyReason: "poison"}); got != 1 {
			t.Errorf("MessagesDropped[poison] = %d, want 1", got)
		}
		if got := metrics.sum(shared.MetricDLQEntries, nil); got != 0 {
			t.Errorf("DLQEntries = %d, want 0", got)
		}
	})

	t.Run("normal DLQ-configured path still emits DLQEntries and writes", func(t *testing.T) {
		clk := clocktest.NewAt(budgetBase)
		rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-dlq-perm", 1, budgetBase, budgetBase))
		store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
		dlqStore := &fakeDLQStore{}
		metrics := newRecordingExporter()

		sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
			return shared.NewBridgeError("DENIED", shared.ErrorPermanent, "not authorized")
		}}
		d := New(Config{
			OutboxStore:  store,
			Sender:       sender,
			DLQ:          dlq.New(dlqStore),
			RouteID:      "route-dlq",
			PartitionKey: "SESSION#sess-dlq",
			Policy: routing.RoutePolicy{
				OnPermanentFailure: routing.FailureDLQ,
				MaxReplayAttempts:  5,
				SendTimeout:        time.Second,
			},
			Clock:   clk,
			Metrics: metrics,
			TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
		})

		success, _, err := d.drainBatch(context.Background(), deferredTestToken())
		if err != nil {
			t.Fatalf("drainBatch err = %v, want nil", err)
		}
		if success != 1 {
			t.Errorf("success = %d, want 1", success)
		}
		if got := metrics.sum(shared.MetricDLQEntries, map[string]string{shared.TagKeyCategory: "permanent"}); got != 1 {
			t.Errorf("DLQEntries[permanent] = %d, want 1 (DLQ-configured path)", got)
		}
		if got := metrics.sum(shared.MetricMessagesDropped, nil); got != 0 {
			t.Errorf("MessagesDropped = %d, want 0 (DLQ path must not drop)", got)
		}
		if got := dlqStore.writes(); got != 1 {
			t.Errorf("DLQ writes = %d, want 1 (durable entry written)", got)
		}
	})
}

// ---------------------------------------------------------------------------
// M3: OnSettled fires only AFTER the terminal Complete durably lands. A Complete
// that fails after a successful DLQ write must NOT fire OnSettled (the record is
// re-claimed and re-settled), so the hook fires exactly once across the retry —
// while the DLQ write itself is per-write and counted at-least-once.
// ---------------------------------------------------------------------------

// completeFailOnceStore serves a single record until it is completed, and fails
// the FIRST Complete call (simulating a store hiccup after a terminal DLQ write)
// then succeeds on the retry.
type completeFailOnceStore struct {
	mu            sync.Mutex
	rec           *persistence.OutboxRecord
	completed     bool
	completeCalls int
}

func (s *completeFailOnceStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	return nil
}

func (s *completeFailOnceStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed || s.rec == nil {
		return nil, nil
	}
	return []*persistence.OutboxRecord{s.rec}, nil
}

func (s *completeFailOnceStore) Complete(_ context.Context, _ []string, _ persistence.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	if s.completeCalls == 1 {
		return errors.New("store: complete failed (simulated)")
	}
	s.completed = true
	return nil
}

func (s *completeFailOnceStore) Expire(context.Context, time.Time, string) (int, error) {
	return 0, nil
}

func (s *completeFailOnceStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func TestDrainer_CompleteFailsAfterTerminalDLQ_OnSettledFiresOnceAcrossRetry(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-m3", 1, budgetBase, budgetBase))
	store := &completeFailOnceStore{rec: rec}
	dlqStore := &fakeDLQStore{}
	metrics := newRecordingExporter()
	hook := &settleCountingHook{}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		return shared.NewBridgeError("DENIED", shared.ErrorPermanent, "not authorized")
	}}
	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		DLQ:          dlq.New(dlqStore),
		RouteID:      "route-m3",
		PartitionKey: "SESSION#sess-m3",
		Policy: routing.RoutePolicy{
			OnPermanentFailure: routing.FailureDLQ,
			MaxReplayAttempts:  5,
			SendTimeout:        time.Second,
		},
		Clock:   clk,
		Metrics: metrics,
		Hook:    hook,
		TokenFn: func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	// Attempt 1: permanent -> DLQ write succeeds, Complete FAILS. The record is
	// neither counted as a success nor settled via the hook.
	success1, _, err1 := d.drainBatch(context.Background(), deferredTestToken())
	if err1 != nil {
		t.Fatalf("drainBatch #1 err = %v, want nil", err1)
	}
	if success1 != 0 {
		t.Errorf("attempt #1 success = %d, want 0 (Complete failed: record must not count as settled)", success1)
	}
	if got := hook.count(); got != 0 {
		t.Errorf("attempt #1 OnSettled fired %d times, want 0 (Complete failed -> hook deferred)", got)
	}
	if got := dlqStore.writes(); got != 1 {
		t.Errorf("attempt #1 DLQ writes = %d, want 1 (DLQ write is per-write; it stands even when Complete fails)", got)
	}
	if got := metrics.sum(shared.MetricDLQEntries, map[string]string{shared.TagKeyCategory: "permanent"}); got != 1 {
		t.Errorf("attempt #1 DLQEntries[permanent] = %d, want 1", got)
	}

	// Attempt 2: record re-claimed, DLQ written AGAIN (per-write), Complete
	// SUCCEEDS -> hook fires, now exactly once across both attempts.
	success2, _, err2 := d.drainBatch(context.Background(), deferredTestToken())
	if err2 != nil {
		t.Fatalf("drainBatch #2 err = %v, want nil", err2)
	}
	if success2 != 1 {
		t.Errorf("attempt #2 success = %d, want 1 (Complete succeeded)", success2)
	}
	if got := hook.count(); got != 1 {
		t.Errorf("OnSettled fired %d times across both attempts, want exactly 1 (fires only after Complete durably lands)", got)
	}
	if got := dlqStore.writes(); got != 2 {
		t.Errorf("DLQ writes = %d across attempts, want 2 (per-write: each attempt writes evidence, at-least-once)", got)
	}
	if got := metrics.sum(shared.MetricDLQEntries, map[string]string{shared.TagKeyCategory: "permanent"}); got != 2 {
		t.Errorf("DLQEntries[permanent] = %d across attempts, want 2 (per-write count)", got)
	}
}

// ---------------------------------------------------------------------------
// M4: a transient send failure whose subsequent Release ALSO fails (store I/O
// error, not a stale token) must STOP the ordering group without counting the
// still-claimed head as a success and without letting a later same-key record
// overtake it.
// ---------------------------------------------------------------------------

func TestDrainer_ReleaseStoreError_StopsGroupNoOvertake(t *testing.T) {
	rec1 := deferredTestRecord(t, "rec-1", "k1")
	rec2 := deferredTestRecord(t, "rec-2", "k1") // same ordering key, persisted after rec-1
	store := &deferredFakeStore{
		claimable:  []*persistence.OutboxRecord{rec1, rec2},
		releaseErr: errors.New("store: release I/O error"),
	}

	var sentIDs sync.Map
	var sendCalls int32
	sender := &fnSender{send: func(_ context.Context, msg ports.OutboundMessage) error {
		atomic.AddInt32(&sendCalls, 1)
		sentIDs.Store(msg.Envelope.ID(), struct{}{})
		return shared.NewBridgeError("OUTAGE", shared.ErrorTransient, "egress down")
	}}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: "SESSION#sess-deferred",
		Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	success, transient, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch err = %v, want nil", err)
	}
	if success != 0 {
		t.Errorf("success = %d, want 0 (release-failed head must not count as a drain success)", success)
	}
	if transient < 1 {
		t.Errorf("transient = %d, want >=1 (release failure drives the transient backoff floor)", transient)
	}
	if _, ok := sentIDs.Load("env-rec-2"); ok {
		t.Errorf("rec-2 was sent: a later same-key record overtook the release-failed head")
	}
	if n := atomic.LoadInt32(&sendCalls); n != 1 {
		t.Errorf("sender calls = %d, want 1 (only the head is attempted; the group stops)", n)
	}
	if got := store.releasedIDs(); len(got) != 0 {
		t.Errorf("released = %v, want none (Release failed, so the record stays durably claimed)", got)
	}
	if got := store.completedIDs(); len(got) != 0 {
		t.Errorf("completed = %v, want none (a transiently-failed record is never completed)", got)
	}
}

// ---------------------------------------------------------------------------
// Min1: a drainer that is constructed and never fed a pending record must still
// report idle after minQuiet (idleSince is seeded at construction), rather than
// blocking until the context times out.
// ---------------------------------------------------------------------------

func TestDrainer_WaitIdle_EmptyDrainerReportsIdleAfterMinQuiet(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	d := New(Config{
		OutboxStore:  &deferredFakeStore{},
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		RouteID:      "route-idle",
		PartitionKey: "SESSION#sess-idle",
		Policy:       routing.RoutePolicy{MaxReplayAttempts: 5, SendTimeout: time.Second},
		Clock:        clk,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	const minQuiet = 30 * time.Second
	// Advance past minQuiet so WaitIdle's first check (clk.Since(idleSince) >=
	// minQuiet) is satisfied immediately for the seeded idleSince.
	clk.Advance(minQuiet)

	// The real ctx timeout is only a safety net: the fixed drainer returns nil
	// without blocking. The pre-Min1 code (idleSince never seeded) would block on
	// the fake clk.After forever and fail here with DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := d.WaitIdle(ctx, minQuiet); err != nil {
		t.Fatalf("WaitIdle = %v, want nil (an always-empty drainer must report idle after minQuiet)", err)
	}
}

// ---------------------------------------------------------------------------
// Min2: a Sender that ignores ctx wedges wg.Wait. The watchdog must (a) emit
// MetricOutboxDrainStalled so the wedge is observable, and (b) UNBLOCK the
// drainer within the bound (batchTimeout+drainWedgeGrace) instead of blocking
// forever — otherwise a single hung sender wedges every other record and
// shutdown. Because the send never returned, the record must NOT be falsely
// completed: it stays Claimed and is re-drained later (at-least-once).
// ---------------------------------------------------------------------------

// signalingClock announces the moment the drainer registers a timer, so the test
// can advance the clock exactly once the watchdog timer exists (the only
// NewTimer call inside drainBatch).
type signalingClock struct {
	*clocktest.Fake
	timerCreated chan struct{}
}

func (c *signalingClock) NewTimer(d time.Duration) clock.Timer {
	tm := c.Fake.NewTimer(d)
	select {
	case c.timerCreated <- struct{}{}:
	default:
	}
	return tm
}

// stallSignalExporter is a recordingExporter that also announces the moment the
// drain-stalled counter is emitted, so the test proceeds only once the watchdog
// case has been taken — fully deterministic, no sleeps or polls.
type stallSignalExporter struct {
	*recordingExporter
	stalled chan struct{}
}

func (e *stallSignalExporter) Counter(name string, value int64, tags ...shared.Tag) {
	e.recordingExporter.Counter(name, value, tags...)
	if name == shared.MetricOutboxDrainStalled {
		select {
		case e.stalled <- struct{}{}:
		default:
		}
	}
}

func TestDrainer_WedgedSender_UnblocksDrainerWithoutFalseComplete(t *testing.T) {
	clk := &signalingClock{Fake: clocktest.NewAt(budgetBase), timerCreated: make(chan struct{}, 1)}
	metrics := &stallSignalExporter{recordingExporter: newRecordingExporter(), stalled: make(chan struct{}, 1)}
	rec := deferredTestRecord(t, "rec-wedge", "")
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}

	inflight := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	sender := &fnSender{send: func(_ context.Context, _ ports.OutboundMessage) error {
		once.Do(func() { close(inflight) })
		<-unblock // deliberately IGNORE ctx: simulate a sender that wedges the loop
		return nil
	}}
	// Never leak the abandoned goroutine out of the test: release the hung send
	// after the assertions so it can return.
	defer close(unblock)

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "route-wedge",
		PartitionKey: "SESSION#sess-wedge",
		Policy:       routing.RoutePolicy{SendTimeout: time.Hour, MaxReplayAttempts: 5},
		DrainTimeout: time.Hour, // keep the REAL workCtx deadline far away
		Clock:        clk,
		Metrics:      metrics,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	type result struct {
		success int
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		s, _, err := d.drainBatch(context.Background(), deferredTestToken())
		resCh <- result{s, err}
	}()

	<-inflight                 // the send is in-flight and ignoring ctx
	<-clk.timerCreated         // waitBatch has registered the watchdog timer
	clk.Advance(3 * time.Hour) // fire the watchdog (past batchTimeout+drainWedgeGrace)
	<-metrics.stalled          // the drain-stalled signal fired: the watchdog case was taken

	// The drainer must UNBLOCK now even though the send is still hung on
	// <-unblock. If waitBatch still blocked on <-done this receive would hang
	// (and the -timeout in CI would fail the test) — that is the wedge the
	// finding is about.
	res := <-resCh
	if res.err != nil {
		t.Fatalf("drainBatch err = %v, want nil (a wedged sender must not surface an error)", res.err)
	}
	if res.success != 0 {
		t.Errorf("success = %d, want 0 (the send never returned, so nothing may be counted delivered)", res.success)
	}
	// No false completion: the send never returned nil, so Complete must never
	// have run — the record stays Claimed and is re-drained later (at-least-once).
	if got := store.completedIDs(); len(got) != 0 {
		t.Errorf("completed = %v, want [] (a record may only be Completed on durable send success)", got)
	}
	if got := metrics.sum(shared.MetricOutboxDrainStalled, nil); got != 1 {
		t.Errorf("OutboxDrainStalled = %d, want 1 (emitted once for the wedged batch)", got)
	}
	// CORE-RES-1: the watchdog must LATCH the partition stalled so Run stops
	// scheduling further batches (each could leak another parked sender).
	if !d.drainStalled.Load() {
		t.Errorf("drainStalled not latched after watchdog abandoned a hung sender (CORE-RES-1)")
	}
}

// TestDrainer_CORE_RES1_StalledLatchEscalatesTerminal proves the CORE-RES-1
// bound: once the partition is latched stalled, Run stops scheduling batches and
// returns ErrDrainStalled — a terminal (non-ctx) error so startBackground
// escalates to a runtime restart that reclaims the leaked goroutine, instead of
// draining forever and leaking one parked sender per batch.
//
// Mutation check: remove the drainStalled check in Run's loop and this hangs
// (Run keeps polling and never returns the terminal error).
func TestDrainer_CORE_RES1_StalledLatchEscalatesTerminal(t *testing.T) {
	clk := &signalingClock{Fake: clocktest.NewAt(budgetBase), timerCreated: make(chan struct{}, 4)}
	store := &deferredFakeStore{} // empty: drainBatch returns fast, exercising only the latch gate
	d := New(Config{
		OutboxStore:  store,
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		RouteID:      "route-core1",
		PartitionKey: "SESSION#sess-core1",
		Policy:       routing.RoutePolicy{SendTimeout: time.Hour, MaxReplayAttempts: 5},
		DrainTimeout: time.Hour,
		Clock:        clk,
		Metrics:      newRecordingExporter(),
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
	// Pre-latch the stall (as the watchdog would after abandoning a hung sender).
	d.drainStalled.Store(true)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(context.Background()) }()

	<-clk.timerCreated         // Run registered its poll timer
	clk.Advance(1 * time.Hour) // fire the poll timer -> one drain cycle -> latch gate

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrDrainStalled) {
			t.Fatalf("Run returned %v, want ErrDrainStalled (terminal escalation)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the stall latch; it kept scheduling batches (CORE-RES-1)")
	}
}

// ---------------------------------------------------------------------------
// HIGH-1: a stale owner must NOT Complete a record after losing the lease. Owner
// A claims a record and passes the pre-send tokenFn check; its Send blocks until
// AFTER A's lease has transferred to owner B; when Send finally returns nil, the
// post-send fence must observe the lost lease and REFUSE the Complete, leaving
// the record Claimed for B to reclaim. Deterministic: the fake sender itself
// flips the live-token state the instant it runs, so the transfer is sequenced
// by the send call — no sleeps.
// ---------------------------------------------------------------------------

func TestDrainer_TokenGoesStaleBetweenSendAndComplete_DoesNotComplete(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := deferredTestRecord(t, "rec-stale", "")
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
	metrics := newRecordingExporter()

	// hasLease models the runtime-side live token: true while A owns the session,
	// flipped to false when the lease transfers to B mid-Send.
	var hasLease atomic.Bool
	hasLease.Store(true)
	tokenFn := func() (persistence.LeaseToken, bool) {
		return deferredTestToken(), hasLease.Load()
	}

	var sent int32
	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		atomic.AddInt32(&sent, 1)
		// A's Send outlived its lease: B has taken the session. The target
		// accepted the message (return nil), but A is no longer authoritative.
		hasLease.Store(false)
		return nil
	}}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "route-stale",
		PartitionKey: "SESSION#sess-stale",
		Policy:       routing.RoutePolicy{SendTimeout: time.Second, MaxReplayAttempts: 5},
		Clock:        clk,
		Metrics:      metrics,
		TokenFn:      tokenFn,
	})

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("drainBatch err = %v, want ErrStaleFencingToken (lost lease must stop the drain and back off)", err)
	}
	if success != 0 {
		t.Errorf("success = %d, want 0 (a record may not count delivered once the owner lost the lease)", success)
	}
	if n := atomic.LoadInt32(&sent); n != 1 {
		t.Errorf("sender calls = %d, want 1 (Send ran once; the lease was lost during it)", n)
	}
	if got := store.completedIDs(); len(got) != 0 {
		t.Errorf("completed = %v, want [] (a stale owner must NOT Complete after losing the lease — HIGH-1)", got)
	}
	if got := metrics.sum(shared.MetricOutboxDuplicateRisk, nil); got != 1 {
		t.Errorf("OutboxDuplicateRisk = %d, want 1 (a fenced completion is a duplicate-delivery risk)", got)
	}
}

// dualSignalExporter announces the moment the drainer emits OutboxDrainStalled
// (watchdog fired) AND the moment it emits OutboxDuplicateRisk (post-send fence
// tripped), so the HIGH-2 test can sequence both transitions deterministically —
// no sleeps or polls.
type dualSignalExporter struct {
	*recordingExporter
	stalled       chan struct{}
	duplicateRisk chan struct{}
}

func (e *dualSignalExporter) Counter(name string, value int64, tags ...shared.Tag) {
	e.recordingExporter.Counter(name, value, tags...)
	switch name {
	case shared.MetricOutboxDrainStalled:
		select {
		case e.stalled <- struct{}{}:
		default:
		}
	case shared.MetricOutboxDuplicateRisk:
		select {
		case e.duplicateRisk <- struct{}{}:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// HIGH-2: a watchdog-abandoned send goroutine must NOT complete its old claim if
// it later returns nil. The sender ignores ctx; waitBatch hits the watchdog,
// records OutboxDrainStalled and RETURNS (cancelling batchCtx) so the drainer
// does not wedge. When the abandoned send LATER returns nil, the post-send fence
// (batchCtx cancelled) must refuse the Complete — the drainer already moved on.
// Deterministic sequencing via the stall + duplicate-risk signals and a Complete
// signal channel (which must never fire).
// ---------------------------------------------------------------------------

func TestDrainer_WatchdogAbandonedSend_LaterReturnsNil_DoesNotComplete(t *testing.T) {
	clk := &signalingClock{Fake: clocktest.NewAt(budgetBase), timerCreated: make(chan struct{}, 1)}
	metrics := &dualSignalExporter{
		recordingExporter: newRecordingExporter(),
		stalled:           make(chan struct{}, 1),
		duplicateRisk:     make(chan struct{}, 1),
	}
	rec := deferredTestRecord(t, "rec-abandon", "")
	completeCh := make(chan string, 1)
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}, completeSignal: completeCh}

	inflight := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sender := &fnSender{send: func(_ context.Context, _ ports.OutboundMessage) error {
		once.Do(func() { close(inflight) })
		<-release  // deliberately IGNORE ctx: block until the test releases us
		return nil // then "succeed" AFTER the watchdog already abandoned the batch
	}}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "route-abandon",
		PartitionKey: "SESSION#sess-abandon",
		// Lease stays live throughout: this proves the BATCH-abandonment fence
		// (not the lease fence) is what refuses the completion.
		Policy:       routing.RoutePolicy{SendTimeout: time.Hour, MaxReplayAttempts: 5},
		DrainTimeout: time.Hour, // keep the REAL workCtx deadline far away
		Clock:        clk,
		Metrics:      metrics,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	drainReturned := make(chan struct{})
	go func() {
		_, _, _ = d.drainBatch(context.Background(), deferredTestToken())
		close(drainReturned)
	}()

	<-inflight                 // the send is in-flight and ignoring ctx
	<-clk.timerCreated         // waitBatch has registered the watchdog timer
	clk.Advance(3 * time.Hour) // fire the watchdog (past batchTimeout+drainWedgeGrace)
	<-metrics.stalled          // watchdog case taken: batchCtx cancelled, drainer proceeds
	<-drainReturned            // the drainer unblocked and moved on (never wedged)

	// Now release the abandoned send. It returns nil and its goroutine reaches
	// the post-send fence, which must refuse the Complete because batchCtx is
	// cancelled. If the fence were absent, Complete would run (completeCh fires)
	// — that is exactly the HIGH-2 regression.
	close(release)
	select {
	case id := <-completeCh:
		t.Fatalf("Complete called for %q after the batch was abandoned; a watchdog-abandoned send must NOT complete (HIGH-2)", id)
	case <-metrics.duplicateRisk:
		// Fence tripped and counted the duplicate risk: Complete was skipped.
	}

	if got := store.completedIDs(); len(got) != 0 {
		t.Errorf("completed = %v, want [] (a watchdog-abandoned send must never Complete — HIGH-2)", got)
	}
	if got := metrics.sum(shared.MetricOutboxDrainStalled, nil); got != 1 {
		t.Errorf("OutboxDrainStalled = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// FINDING 1 (adversarial): the pre-send lease re-check must compare the live
// token VERSION to the claim token, not merely presence. A lease reacquired at a
// HIGHER version by a new owner BEFORE this record's Send still returns
// (hasLease=true); a bare !hasLease check would Send a record claimed under the
// superseded version — a duplicate target-side delivery the post-send fence can
// no longer prevent (it only refuses the Complete, after Send already fired).
// The pre-send version check refuses to Send at all.
// ---------------------------------------------------------------------------

func TestDrainer_LeaseReacquiredBeforeSend_DoesNotSend(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := deferredTestRecord(t, "rec-presend-stale", "")
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
	metrics := newRecordingExporter()

	// The record is claimed under v1 (the token passed to drainBatch), but the
	// live token has already advanced to v2 held by a NEW owner before Send.
	liveToken := persistence.LeaseToken{Owner: "owner-2", Version: 2}
	tokenFn := func() (persistence.LeaseToken, bool) { return liveToken, true }

	var sent int32
	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}}

	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "route-presend-stale",
		PartitionKey: "SESSION#sess-presend-stale",
		Policy:       routing.RoutePolicy{SendTimeout: time.Second, MaxReplayAttempts: 5},
		Clock:        clk,
		Metrics:      metrics,
		TokenFn:      tokenFn,
	})

	// drainBatch runs under the ORIGINAL claim token (v1); the live token is v2.
	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if !errors.Is(err, shared.ErrStaleFencingToken) {
		t.Fatalf("drainBatch err = %v, want ErrStaleFencingToken (superseded lease version must stop the drain)", err)
	}
	if success != 0 {
		t.Errorf("success = %d, want 0", success)
	}
	if n := atomic.LoadInt32(&sent); n != 0 {
		t.Errorf("sender calls = %d, want 0 (must NOT send a record claimed under a superseded lease version)", n)
	}
	if got := store.completedIDs(); len(got) != 0 {
		t.Errorf("completed = %v, want []", got)
	}
}

// versionOnlyStore models the SANCTIONED version-only claim semantics of the
// in-memory outbox backend (ports/stores.go): a record is claimable only when
// Pending or when Claimed at a STRICTLY-OLDER claim_version. There is NO
// same-version time-stale reclaim, so a record left Claimed at the CURRENT lease
// version can never be re-claimed by the same owner — the exact stranding
// finding 2 is about. Complete/Release are owner+version+status fenced. It is
// deliberately minimal (single partition, no wall-clock, no high-water-mark).
type versionOnlyRecord struct {
	rec       *persistence.OutboxRecord
	state     string // "pending" | "claimed" | "completed"
	claimVer  uint64
	claimedBy string
}

type versionOnlyStore struct {
	mu    sync.Mutex
	order []string
	recs  map[string]*versionOnlyRecord
	// releaseSignal, when non-nil, receives each id transitioned back to pending
	// by Release, so a test can wait for the fence's release before re-draining.
	releaseSignal chan string
}

func newVersionOnlyStore(recs ...*persistence.OutboxRecord) *versionOnlyStore {
	s := &versionOnlyStore{recs: make(map[string]*versionOnlyRecord)}
	for _, r := range recs {
		s.order = append(s.order, r.ID())
		s.recs[r.ID()] = &versionOnlyRecord{rec: r, state: "pending"}
	}
	return s
}

func (s *versionOnlyStore) Persist(context.Context, []*persistence.OutboxRecord) error { return nil }

func (s *versionOnlyStore) Claim(_ context.Context, _ string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*persistence.OutboxRecord
	for _, id := range s.order {
		if limit > 0 && len(out) >= limit {
			break
		}
		vr := s.recs[id]
		claimable := vr.state == "pending" || (vr.state == "claimed" && vr.claimVer < token.Version)
		if !claimable {
			continue
		}
		vr.state = "claimed"
		vr.claimVer = token.Version
		vr.claimedBy = token.Owner
		out = append(out, vr.rec)
	}
	return out, nil
}

func (s *versionOnlyStore) fenced(vr *versionOnlyRecord, token persistence.LeaseToken) bool {
	return vr != nil && vr.state == "claimed" && vr.claimedBy == token.Owner && vr.claimVer == token.Version
}

func (s *versionOnlyStore) Complete(_ context.Context, ids []string, token persistence.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if !s.fenced(s.recs[id], token) {
			return shared.ErrStaleFencingToken
		}
	}
	for _, id := range ids {
		s.recs[id].state = "completed"
	}
	return nil
}

func (s *versionOnlyStore) Release(_ context.Context, ids []string, token persistence.LeaseToken) error {
	s.mu.Lock()
	for _, id := range ids {
		if !s.fenced(s.recs[id], token) {
			s.mu.Unlock()
			return shared.ErrStaleFencingToken
		}
	}
	for _, id := range ids {
		vr := s.recs[id]
		vr.state = "pending"
		vr.claimVer = 0
		vr.claimedBy = ""
	}
	s.mu.Unlock()
	if s.releaseSignal != nil {
		for _, id := range ids {
			s.releaseSignal <- id
		}
	}
	return nil
}

func (s *versionOnlyStore) Expire(context.Context, time.Time, string) (int, error) { return 0, nil }

func (s *versionOnlyStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *versionOnlyStore) stateOf(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vr := s.recs[id]; vr != nil {
		return vr.state
	}
	return ""
}

var _ ports.OutboxStore = (*versionOnlyStore)(nil)
var _ ports.OutboxReleaser = (*versionOnlyStore)(nil)

// ---------------------------------------------------------------------------
// FINDING 2 (adversarial): errCompletionFenced must RELEASE the fenced head AND
// the never-attempted ordering-group suffix back to pending, or they strand
// Claimed forever on a version-only store (the in-memory backend). Proof end to
// end: two records in ONE ordering group; the head is abandoned by the watchdog
// and its slow Send later returns nil (errCompletionFenced); a SECOND drain with
// the SAME lease token must re-claim and deliver BOTH — only possible because the
// fence released them (a version-only store cannot re-claim a still-Claimed
// record at the same version).
// ---------------------------------------------------------------------------

func TestDrainer_WatchdogAbandon_VersionOnlyStore_ReleasesHeadAndSuffixForReclaim(t *testing.T) {
	clk := &signalingClock{Fake: clocktest.NewAt(budgetBase), timerCreated: make(chan struct{}, 1)}
	metrics := &dualSignalExporter{
		recordingExporter: newRecordingExporter(),
		stalled:           make(chan struct{}, 1),
		duplicateRisk:     make(chan struct{}, 1),
	}
	// Two records sharing ONE ordering key -> one sequential group: the head is
	// attempted; the suffix is never reached because the group stops on the
	// head's fence, so it too is a claimed-but-unattempted record.
	head := deferredTestRecord(t, "rec-head", "grp")
	tail := deferredTestRecord(t, "rec-tail", "grp")
	store := newVersionOnlyStore(head, tail)
	store.releaseSignal = make(chan string, 2)

	var sendCount int32
	inflight := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sender := &fnSender{send: func(_ context.Context, _ ports.OutboundMessage) error {
		if atomic.AddInt32(&sendCount, 1) == 1 {
			once.Do(func() { close(inflight) })
			<-release // ignore ctx: block the FIRST send past the watchdog
		}
		return nil
	}}

	tok := deferredTestToken() // v1, owner-1
	d := New(Config{
		OutboxStore:  store,
		Sender:       sender,
		RouteID:      "route-vonly",
		PartitionKey: "SESSION#sess-vonly",
		Policy:       routing.RoutePolicy{SendTimeout: time.Hour, MaxReplayAttempts: 5},
		DrainTimeout: time.Hour,
		Clock:        clk,
		Metrics:      metrics,
		TokenFn:      func() (persistence.LeaseToken, bool) { return tok, true },
	})

	drainReturned := make(chan struct{})
	go func() {
		_, _, _ = d.drainBatch(context.Background(), tok)
		close(drainReturned)
	}()

	<-inflight                 // head send in-flight, ignoring ctx
	<-clk.timerCreated         // watchdog registered
	clk.Advance(3 * time.Hour) // fire the watchdog
	<-metrics.stalled          // batchCtx cancelled, drainer proceeds
	<-drainReturned            // the drainer moved on (never wedged)

	// Release the abandoned head send: it returns nil, trips the batch-abandon
	// fence (errCompletionFenced), and the group loop releases the head AND the
	// unattempted tail back to pending on the version-only store.
	close(release)
	<-metrics.duplicateRisk
	// Wait for BOTH records to be released before re-draining (deterministic).
	released := map[string]bool{}
	for len(released) < 2 {
		released[<-store.releaseSignal] = true
	}
	if s := store.stateOf("rec-head"); s != "pending" {
		t.Fatalf("head state = %q, want pending (fence must release the abandoned head)", s)
	}
	if s := store.stateOf("rec-tail"); s != "pending" {
		t.Fatalf("tail state = %q, want pending (fence must release the unattempted suffix)", s)
	}

	// The crux: a SECOND drain with the SAME token (same version) must re-claim
	// and deliver BOTH. On a version-only store this is possible ONLY because the
	// fence released them; without the release they stay Claimed at v1 and this
	// drain finds nothing (the mutation).
	success, _, err := d.drainBatch(context.Background(), tok)
	if err != nil {
		t.Fatalf("second drainBatch err = %v, want nil", err)
	}
	if success != 2 {
		t.Fatalf("second drain success = %d, want 2 (both released records re-drained under the SAME token)", success)
	}
	if s := store.stateOf("rec-head"); s != "completed" {
		t.Fatalf("head state = %q, want completed after reclaim", s)
	}
	if s := store.stateOf("rec-tail"); s != "completed" {
		t.Fatalf("tail state = %q, want completed after reclaim", s)
	}
}
