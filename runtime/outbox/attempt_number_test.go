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

// ports.DeliveryAttempt.Attempt and ports.DeliveryOutcome.Attempt are 1-based:
// the first delivery of a message is attempt 1 on every path. On the outbox
// path OutboxRecord.Claim increments ReplayCount for the claim being attempted
// right now, so the count already INCLUDES this attempt — the number is the
// replay count itself, not the count plus one. Reporting one too many makes
// hooks and audit trails disagree with the direct path for the same message.

// attemptRecordingHook captures every attempt/outcome number the drainer emits.
type attemptRecordingHook struct {
	mu       sync.Mutex
	attempts []int
	settled  []int
}

func (h *attemptRecordingHook) OnAttempt(_ context.Context, a ports.DeliveryAttempt) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attempts = append(h.attempts, a.Attempt)
}

func (h *attemptRecordingHook) OnSettled(_ context.Context, o ports.DeliveryOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settled = append(h.settled, o.Attempt)
}

func (h *attemptRecordingHook) snapshot() (attempts, settled []int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.attempts...), append([]int(nil), h.settled...)
}

func TestProcessRecord_AttemptNumberIsOneBasedOnFirstClaim(t *testing.T) {
	tests := []struct {
		name        string
		replayCount int
		want        int
	}{
		{"first claim reports attempt 1", 1, 1},
		{"third claim reports attempt 3", 3, 3},
		// A record rehydrated with no replay count at all — an out-of-tree store
		// that does not increment it — must still report a 1-based number.
		{"uncounted record floors at 1", 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := persistence.RehydrateFromSnapshot(budgetSnapshot(
				"attempt-1", tc.replayCount, budgetBase, budgetBase))
			store := &deferredFakeStore{claimable: []*persistence.OutboxRecord{rec}}
			hook := &attemptRecordingHook{}

			sender := &fnSender{send: func(context.Context, ports.OutboundMessage) error { return nil }}
			d := New(Config{
				OutboxStore:  store,
				Sender:       sender,
				RouteID:      "r1",
				PartitionKey: "SESSION#sess-budget",
				Policy:       routing.RoutePolicy{SendTimeout: 5 * time.Second, MaxReplayAttempts: 5},
				Hook:         hook,
				TokenFn:      func() (persistence.LeaseToken, bool) { return deferredTestToken(), true },
			})

			if _, _, err := d.drainBatch(context.Background(), deferredTestToken()); err != nil {
				t.Fatalf("drainBatch: %v", err)
			}

			attempts, settled := hook.snapshot()
			if len(attempts) != 1 || attempts[0] != tc.want {
				t.Fatalf("OnAttempt numbers %v, want [%d]", attempts, tc.want)
			}
			if len(settled) != 1 || settled[0] != tc.want {
				t.Fatalf("OnSettled numbers %v, want [%d]", settled, tc.want)
			}
		})
	}
}
