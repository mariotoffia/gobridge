package outbox

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// ---------------------------------------------------------------------------
// WP-REPLAY-BUDGET: age-based outbox replay budget
//
// The poison decision is now a hard AND-gate: a record is DLQ'd only when its
// ReplayCount exceeds MaxReplayAttempts AND wall-clock time since its first
// attempt (FirstAttemptedAt) has reached ReplayBudget (replayBudgetExhausted).
// Records with a zero FirstAttemptedAt (pre-budget schema) fall back
// bit-for-bit to the legacy CreatedAt/poisonMinAge age gate.
//
// White-box: drainBatch is exercised directly with an injected clock so the
// budget decision is deterministic — no wall-clock waits. The store fake
// (deferredFakeStore) returns records verbatim (it does NOT re-stamp
// FirstAttemptedAt on claim), so each record's age is exactly what the test
// rehydrates.
// ---------------------------------------------------------------------------

// budgetBase is a fixed reference instant; the injected clock stays here and
// each record's age is baked into FirstAttemptedAt/CreatedAt relative to it.
var budgetBase = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// budgetPoisonWarnMsg is the point-of-loss WARN emitted by handlePoison. Kept
// verbatim so the observability contract (message + carried attrs) is pinned.
const budgetPoisonWarnMsg = "outbox record poisoned: replay attempts exhausted, routing to DLQ"

func budgetSnapshot(id string, replayCount int, firstAttempted, createdAt time.Time) persistence.OutboxSnapshot {
	return persistence.OutboxSnapshot{
		ID:               id,
		RouteID:          "route-budget",
		EnvelopeID:       "env-" + id,
		BindingID:        "bind-budget",
		SessionID:        "sess-budget",
		Address:          "test/topic",
		Envelope:         *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-" + id, Payload: []byte("payload")}),
		Status:           persistence.OutboxPending,
		ReplayCount:      replayCount,
		FirstAttemptedAt: firstAttempted,
		CreatedAt:        createdAt,
	}
}

// budgetDrainer builds a Drainer over the production route defaults
// (MaxReplayAttempts=5, ReplayBudget=15m, SendTimeout=30s → poisonMinAge
// fallback 2m30s) with an injected clock, metrics and logger. It returns the
// resolved policy so tests can assert against the same defaults.
func budgetDrainer(store *deferredFakeStore, sender ports.Sender, clk *clocktest.Fake, metrics ports.MetricsExporter, logger *slog.Logger) (*Drainer, routing.RoutePolicy) {
	policy := routing.RoutePolicy{}.WithDefaults()
	d := New(Config{
		OutboxStore: store,
		Sender:      sender,
		// A real (fake) DLQ store so HasStore() is true: WithDefaults() sets
		// OnPermanentFailure=FailureDLQ, and now routes poison/permanent to the
		// DROP path when no store is configured. These tests assert the DLQ path
		// (emitDLQ → DLQEntries), so they must run with a store behind the router.
		DLQ:          dlq.New(&fakeDLQStore{}),
		RouteID:      "route-budget",
		PartitionKey: "SESSION#sess-budget",
		Policy:       policy,
		Clock:        clk,
		Metrics:      metrics,
		Logger:       logger,
		TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})
	return d, policy
}

// TestDrainer_TransientOutageWithinBudget_NeverPoisons is the HEADLINE test.
// A record that has exhausted its replay COUNT during a transient outage but is
// still WITHIN the wall-clock ReplayBudget must never be poisoned: it is
// released back to pending for retry. CreatedAt is deliberately old enough that
// the LEGACY poisonMinAge gate WOULD fire, proving the budget — not CreatedAt —
// now decides.
//
// Probe: reverting the criterion in processRecord to d.poisonAgeReached(rec)
// makes this test fail (the record would be DLQ'd), which is exactly the
// premature-DLQ-from-outage defect this work package fixes.
func TestDrainer_TransientOutageWithinBudget_NeverPoisons(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-hot", 6,
		budgetBase.Add(-14*time.Minute), budgetBase.Add(-30*time.Minute)))
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}

	var sent int32
	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		atomic.AddInt32(&sent, 1)
		return shared.NewBridgeError("OUTAGE", shared.ErrorTransient, "egress down")
	}}
	metrics := newDLQCountingExporter()
	d, _ := budgetDrainer(store, sender, clk, metrics, nil)

	success, transient, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch err = %v, want nil", err)
	}
	if success != 0 {
		t.Errorf("success = %d, want 0 (within-budget transient must not complete)", success)
	}
	if transient != 1 {
		t.Errorf("transient = %d, want 1 (record released for retry)", transient)
	}
	if got := metrics.total(); got != 0 {
		t.Errorf("DLQ entries = %d, want 0 (within budget must NEVER poison)", got)
	}
	if got := store.completedIDs(); len(got) != 0 {
		t.Errorf("completed = %v, want none (not poisoned, not delivered)", got)
	}
	if got := store.releasedIDs(); len(got) != 1 || got[0] != "rec-hot" {
		t.Errorf("released = %v, want [rec-hot] (returned to pending for retry)", got)
	}
	if n := atomic.LoadInt32(&sent); n != 1 {
		t.Errorf("sender calls = %d, want 1 (send attempted, failed transiently)", n)
	}
}

// TestDrainer_BudgetExhausted_PoisonsWithWarn: once BOTH the replay count and
// the wall-clock budget are spent, the record is poisoned to the DLQ before any
// send, and the point-of-loss WARN carries the age evidence
// (first_attempted_at, replay_budget).
func TestDrainer_BudgetExhausted_PoisonsWithWarn(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-old", 6,
		budgetBase.Add(-16*time.Minute), budgetBase.Add(-30*time.Minute)))
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}

	var sent int32
	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}}
	metrics := newDLQCountingExporter()
	logRec := &budgetLogRecorder{}
	d, policy := budgetDrainer(store, sender, clk, metrics, slog.New(logRec))

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch err = %v, want nil", err)
	}
	if success != 1 {
		t.Errorf("success = %d, want 1 (poison is handled and completed)", success)
	}
	if got := metrics.count("poison"); got != 1 {
		t.Errorf("poison DLQ entries = %d, want 1", got)
	}
	if got := metrics.total(); got != 1 {
		t.Errorf("total DLQ entries = %d, want 1 (only the poison)", got)
	}
	if got := store.completedIDs(); len(got) != 1 || got[0] != "rec-old" {
		t.Errorf("completed = %v, want [rec-old]", got)
	}
	if got := store.releasedIDs(); len(got) != 0 {
		t.Errorf("released = %v, want none (poison completes, not releases)", got)
	}
	if n := atomic.LoadInt32(&sent); n != 0 {
		t.Errorf("sender calls = %d, want 0 (poison decided before send)", n)
	}

	entry, ok := logRec.find(budgetPoisonWarnMsg)
	if !ok {
		t.Fatalf("expected poison WARN %q; got messages %v", budgetPoisonWarnMsg, logRec.messages())
	}
	if entry.level != slog.LevelWarn {
		t.Errorf("poison log level = %v, want WARN", entry.level)
	}
	wantFirst := budgetBase.Add(-16 * time.Minute)
	if got, _ := entry.attrs["first_attempted_at"].(time.Time); !got.Equal(wantFirst) {
		t.Errorf("first_attempted_at attr = %v, want %v", entry.attrs["first_attempted_at"], wantFirst)
	}
	if got, _ := entry.attrs["replay_budget"].(time.Duration); got != policy.ReplayBudget {
		t.Errorf("replay_budget attr = %v, want %v", entry.attrs["replay_budget"], policy.ReplayBudget)
	}
}

// TestDrainer_BudgetNotCheckedBelowAttemptFloor: age past the budget is NEVER
// sufficient on its own — a record at or under MaxReplayAttempts is delivered
// normally, never poisoned, regardless of how old it is.
func TestDrainer_BudgetNotCheckedBelowAttemptFloor(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-under-cap", 5,
		budgetBase.Add(-time.Hour), budgetBase.Add(-time.Hour)))
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
	metrics := newDLQCountingExporter()
	d, _ := budgetDrainer(store, sender, clk, metrics, nil)

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch err = %v, want nil", err)
	}
	if success != 1 {
		t.Errorf("success = %d, want 1 (delivered normally, not poisoned)", success)
	}
	if got := metrics.total(); got != 0 {
		t.Errorf("DLQ entries = %d, want 0 (at/under MaxReplayAttempts is never poison)", got)
	}
	if got := store.completedIDs(); len(got) != 1 || got[0] != "rec-under-cap" {
		t.Errorf("completed = %v, want [rec-under-cap]", got)
	}
}

// TestDrainer_LegacyZeroFirstAttempt_FallsBackToCreatedAtGate pins that a
// record with a zero FirstAttemptedAt (persisted before the replay-budget
// schema) is decided BIT-FOR-BIT by the legacy CreatedAt/poisonMinAge gate: old
// enough poisons, young enough does not.
func TestDrainer_LegacyZeroFirstAttempt_FallsBackToCreatedAtGate(t *testing.T) {
	cases := []struct {
		name         string
		createdAt    time.Time
		wantPoison   int
		wantReleased bool
	}{
		{"created older than poisonMinAge -> poison", budgetBase.Add(-30 * time.Minute), 1, false},
		{"created younger than poisonMinAge -> no poison", budgetBase.Add(-1 * time.Minute), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewAt(budgetBase)
			rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-legacy", 6,
				time.Time{}, tc.createdAt))
			store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
			sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
				return shared.NewBridgeError("OUTAGE", shared.ErrorTransient, "egress down")
			}}
			metrics := newDLQCountingExporter()
			d, _ := budgetDrainer(store, sender, clk, metrics, nil)

			if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
				t.Fatalf("drainBatch err = %v, want nil", err)
			}
			if got := metrics.count("poison"); got != tc.wantPoison {
				t.Errorf("poison DLQ = %d, want %d", got, tc.wantPoison)
			}
			released := len(store.releasedIDs()) == 1
			if released != tc.wantReleased {
				t.Errorf("released = %v, want %v", released, tc.wantReleased)
			}
		})
	}
}

// TestDrainer_PermanentErrorStillDLQsImmediately: a permanent send error DLQs
// on the first attempt regardless of replay count or budget — the budget only
// gates the replay-exhaustion poison path, never the permanent-error path.
func TestDrainer_PermanentErrorStillDLQsImmediately(t *testing.T) {
	clk := clocktest.NewAt(budgetBase)
	rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-perm", 1,
		budgetBase, budgetBase))
	store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}

	sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
		return shared.NewBridgeError("DENIED", shared.ErrorPermanent, "not authorized")
	}}
	metrics := newDLQCountingExporter()
	d, _ := budgetDrainer(store, sender, clk, metrics, nil)

	success, _, err := d.drainBatch(context.Background(), deferredTestToken())
	if err != nil {
		t.Fatalf("drainBatch err = %v, want nil", err)
	}
	if success != 1 {
		t.Errorf("success = %d, want 1 (permanent DLQ completes the record)", success)
	}
	if got := metrics.count("permanent"); got != 1 {
		t.Errorf("permanent DLQ = %d, want 1", got)
	}
	if got := metrics.count("poison"); got != 0 {
		t.Errorf("poison DLQ = %d, want 0 (not a replay poison)", got)
	}
	if got := store.completedIDs(); len(got) != 1 || got[0] != "rec-perm" {
		t.Errorf("completed = %v, want [rec-perm]", got)
	}
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// dlqCountingExporter tallies DLQEntries counter increments by their category
// tag so tests can assert exact poison/permanent DLQ counts without a real DLQ
// store. All other metrics are discarded via the embedded NoopExporter.
// Thread-safe: drainBatch may increment on drain goroutines.
type dlqCountingExporter struct {
	*ports.NoopExporter
	mu    sync.Mutex
	byCat map[string]int
}

func newDLQCountingExporter() *dlqCountingExporter {
	return &dlqCountingExporter{NoopExporter: &ports.NoopExporter{}, byCat: make(map[string]int)}
}

func (e *dlqCountingExporter) Counter(name string, value int64, tags ...shared.Tag) {
	if name != shared.MetricDLQEntries {
		return
	}
	cat := ""
	for _, tg := range tags {
		if tg.Key == shared.TagKeyCategory {
			cat = tg.Value
		}
	}
	e.mu.Lock()
	e.byCat[cat] += int(value)
	e.mu.Unlock()
}

func (e *dlqCountingExporter) count(cat string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.byCat[cat]
}

func (e *dlqCountingExporter) total() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, v := range e.byCat {
		n += v
	}
	return n
}

// budgetLogRecorder is a slog.Handler that captures level, message and
// flattened attributes so tests can assert on structured WARN fields. A local
// copy is required because the equivalent recorder in the runtime_test package
// is not importable from package outbox. Thread-safe.
type budgetLogRecorder struct {
	mu      sync.Mutex
	entries []budgetLog
}

type budgetLog struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (h *budgetLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *budgetLogRecorder) Handle(_ context.Context, r slog.Record) error {
	e := budgetLog{level: r.Level, msg: r.Message, attrs: make(map[string]any, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		e.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, e)
	h.mu.Unlock()
	return nil
}

func (h *budgetLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *budgetLogRecorder) WithGroup(string) slog.Handler      { return h }

func (h *budgetLogRecorder) find(msg string) (budgetLog, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.msg == msg {
			return e, true
		}
	}
	return budgetLog{}, false
}

func (h *budgetLogRecorder) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.entries))
	for i, e := range h.entries {
		out[i] = e.msg
	}
	return out
}

// ═══════════════════════════════════════════════════════════════════════════
// Production contract: an "attempt" is a CLAIM, not a transport invocation.
//
// The stores increment ReplayCount when a record is CLAIMED (SQLite:
// acl_query.go claim transaction), so work that was claimed and then deferred —
// a batch deadline expiring before its send launched — burns the same budget as
// work that actually reached the sender. A record can therefore be poisoned
// having NEVER been handed to a transport.
//
// That is accepted, bounded-backlog policy, not a defect: the count half of the
// poison AND-gate is deliberately cheap to spend, and the wall-clock
// ReplayBudget is what bounds the resulting loss. This test pins BOTH halves so
// the semantics cannot drift silently — within the budget an unsent record
// still gets its send; past it the record is terminalized without one.
// ═══════════════════════════════════════════════════════════════════════════

// TestDrainer_ClaimCountedAttemptsBoundedByBudget_ProductionContract drives a
// record whose ReplayCount was spent entirely by claims that never reached the
// sender.
func TestDrainer_ClaimCountedAttemptsBoundedByBudget_ProductionContract(t *testing.T) {
	cases := []struct {
		name         string
		firstAttempt time.Duration // age of FirstAttemptedAt at drain time
		wantSends    int32
		wantPoison   int
	}{
		{
			// Count spent by claims alone, budget NOT yet reached: the record must
			// still get a real transport attempt. Claim-counting must never
			// terminalize work early.
			name:         "within budget: unsent record still gets its send",
			firstAttempt: 14 * time.Minute,
			wantSends:    1,
			wantPoison:   0,
		},
		{
			// Budget reached: the record is poisoned WITHOUT a send, because the
			// attempts it consumed were claims. This is the documented loss.
			name:         "budget exhausted: poisoned without ever being sent",
			firstAttempt: 16 * time.Minute,
			wantSends:    0,
			wantPoison:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewAt(budgetBase)
			// ReplayCount 6 > MaxReplayAttempts 5, accumulated purely by claims:
			// the record was claimed and deferred on every prior cycle.
			rec := persistence.RehydrateFromSnapshot(budgetSnapshot("rec-never-sent", 6,
				budgetBase.Add(-tc.firstAttempt), budgetBase.Add(-tc.firstAttempt)))
			store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}

			var sent int32
			sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error {
				atomic.AddInt32(&sent, 1)
				return nil
			}}
			metrics := newDLQCountingExporter()
			d, policy := budgetDrainer(store, sender, clk, metrics, nil)

			if policy.MaxReplayAttempts >= 6 {
				t.Fatalf("precondition: the record's claim count (6) must exceed MaxReplayAttempts, got %d",
					policy.MaxReplayAttempts)
			}
			if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
				t.Fatalf("drainBatch err = %v, want nil", err)
			}

			if n := atomic.LoadInt32(&sent); n != tc.wantSends {
				t.Errorf("sender calls = %d, want %d", n, tc.wantSends)
			}
			if got := metrics.count("poison"); got != tc.wantPoison {
				t.Errorf("poison DLQ entries = %d, want %d", got, tc.wantPoison)
			}
		})
	}
}
