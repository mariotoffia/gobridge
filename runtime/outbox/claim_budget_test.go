package outbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// A Claim's cost scales with the batch: a backend that claims one record per
// remote transaction pays `limit` round trips. Bounding it with the route's
// send timeout — a per-MESSAGE budget — makes a large batch time out mid-loop
// on every cycle, which strands the records already claimed, charges each one a
// replay attempt on recovery, and can poison a healthy backlog to the
// dead-letter queue without a single send. The claim bound must therefore scale
// with `limit` while never dropping below the send timeout.

// claimDeadlineProbeStore records the deadline each Claim receives.
type claimDeadlineProbeStore struct {
	mu        sync.Mutex
	deadlines []time.Duration
	now       func() time.Time
}

func (s *claimDeadlineProbeStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	return nil
}

func (s *claimDeadlineProbeStore) Claim(ctx context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	dl, ok := ctx.Deadline()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		s.deadlines = append(s.deadlines, 0)
		return nil, nil
	}
	s.deadlines = append(s.deadlines, dl.Sub(s.now()).Round(time.Second))
	return nil, nil
}

func (s *claimDeadlineProbeStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

func (s *claimDeadlineProbeStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}

func (s *claimDeadlineProbeStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

func (s *claimDeadlineProbeStore) budgets() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.deadlines...)
}

var _ ports.OutboxStore = (*claimDeadlineProbeStore)(nil)

func TestDrainBatch_ClaimBudgetScalesWithBatchSize(t *testing.T) {
	tests := []struct {
		name        string
		sendTimeout time.Duration
		batchSize   int
		want        time.Duration
	}{
		{
			// A small batch costs less than one send: the send timeout is the
			// floor, so the bound never undercuts a single claim round trip.
			name:        "small batch keeps the send-timeout floor",
			sendTimeout: 30 * time.Second,
			batchSize:   10,
			want:        30 * time.Second,
		},
		{
			// 100 records at the per-record claim allowance exceed a 1s send
			// timeout, so the batch cost wins.
			name:        "large batch scales past a short send timeout",
			sendTimeout: time.Second,
			batchSize:   100,
			want:        100 * perRecordClaimBudget,
		},
		{
			// A pathological batch cannot pin the drain goroutine forever.
			name:        "budget is capped",
			sendTimeout: time.Second,
			batchSize:   100_000,
			want:        maxClaimBudget,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &claimDeadlineProbeStore{now: time.Now}
			d := New(Config{
				OutboxStore:       store,
				Sender:            &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
				RouteID:           "r1",
				PartitionKey:      "SESSION#sess-claim-budget",
				DrainBatchSize:    tc.batchSize,
				DrainMaxBatchSize: tc.batchSize,
				Policy:            routing.RoutePolicy{SendTimeout: tc.sendTimeout, MaxReplayAttempts: 5},
				TokenFn:           func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
			})

			if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
				t.Fatalf("drainBatch: %v", err)
			}
			got := store.budgets()
			if len(got) != 1 {
				t.Fatalf("expected exactly one Claim, got %d", len(got))
			}
			if got[0] != tc.want.Round(time.Second) {
				t.Fatalf("claim budget = %v, want %v", got[0], tc.want.Round(time.Second))
			}
		})
	}
}

// The batch ceiling (min(batchCount * PerRecordDrainTimeout, MaxDrainTimeout))
// may only RAISE the batch budget, never cut it. The budget is floored at the
// sequential send depth times (SendTimeout + complete margin) — 30s+ on
// defaults — so the 10s default ceiling never binds and a record always gets its
// full send budget. That is why the ceiling's exact value is close to inert in
// practice, and this pins it rather than asserting it.
//
// Mutation this kills: applying the ceiling with min() instead of raise-only
// caps the budget at the ceiling → the assertion below FAILs.
func TestBatchTimeout_CeilingOnlyRaisesNeverCuts(t *testing.T) {
	d := New(Config{
		OutboxStore:  &deferredFakeStore{},
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		RouteID:      "route-ceiling",
		PartitionKey: "SESSION#sess-ceiling",
		// WithDefaults gives SendTimeout 30s; the ceiling below is far smaller.
		Policy:          routing.RoutePolicy{}.WithDefaults(),
		MaxDrainTimeout: time.Second,
		TokenFn:         func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	rec := deferredTestRecord(t, "ceiling-1", "")
	groups := groupByOrderingKey([]*persistence.OutboxRecord{rec})
	if got := d.batchTimeout(1, groups); got < 30*time.Second {
		t.Fatalf("batch budget = %v; a 1s ceiling must not cut below one full send (30s+)", got)
	}
}

// A ceiling LARGER than the send-derived floor does raise the budget — proving
// the knob still has an effect where it is meant to.
func TestBatchTimeout_LargeCeilingRaisesTheBudget(t *testing.T) {
	d := New(Config{
		OutboxStore:  &deferredFakeStore{},
		Sender:       &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }},
		RouteID:      "route-ceiling-2",
		PartitionKey: "SESSION#sess-ceiling-2",
		Policy:       routing.RoutePolicy{SendTimeout: time.Second, MaxReplayAttempts: 5},
		// The ceiling is min(batchCount * PerRecord, Max), so a single record
		// needs a large PER-RECORD allowance to exceed the send-derived floor.
		PerRecordDrainTimeout: 5 * time.Minute,
		MaxDrainTimeout:       10 * time.Minute,
		TokenFn:               func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
	})

	rec := deferredTestRecord(t, "ceiling-2", "")
	groups := groupByOrderingKey([]*persistence.OutboxRecord{rec})
	if got := d.batchTimeout(1, groups); got != 5*time.Minute {
		t.Fatalf("batch budget = %v; a ceiling above the send floor must raise it to 5m", got)
	}
}
