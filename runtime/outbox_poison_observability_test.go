package runtime_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
)

// ---------------------------------------------------------------------------
// A4-R1: premature-DLQ-from-outage is observable at the point of loss
//
// The age-based root-cause decoupling of replay_count is a deferred
// cross-store change (aggregate + snapshot + Dynamo). The ship-gate
// mitigation is the 5s transientRetryFloor, which bounds how fast a
// transient outage burns the replay budget but does NOT stop a long enough
// outage from DLQ'ing an otherwise-good message once the budget is spent.
//
// This test pins the small, self-contained observability improvement that
// accompanies the deferral: the drainer emits an explicit WARN at the
// replay-exhaustion poison site so operators can see a good message being
// DLQ'd by budget exhaustion instead of it surfacing only as a generic,
// reason-less DLQ entry. Replay-count exhaustion is the ONLY route to this
// poison path (permanent errors DLQ immediately), so the WARN is a faithful
// signal of premature-DLQ-from-outage.
// ---------------------------------------------------------------------------

const poisonWarnMsg = "outbox record poisoned: replay attempts exhausted, routing to DLQ"

// TestOutboxDrainer_PoisonReplayExhaustion_EmitsObservableWarn drives a
// record already past MaxReplayAttempts through the drainer and asserts the
// point-of-loss WARN fires with the record identity and replay budget so the
// premature DLQ is observable. The record must NOT be sent (poison is checked
// before send).
func TestOutboxDrainer_PoisonReplayExhaustion_EmitsObservableWarn(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	handler := &attrLogRecorder{}
	logger := slog.New(handler)

	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	lease := NewFakeLeaseStore()
	pk := persistence.OutboxPartitionKey("sess-poison", "")
	_, _ = lease.Acquire(context.Background(), "sess-poison", token.Owner, 30*time.Second, nil)

	policy := routing.RoutePolicy{}.WithDefaults() // MaxReplayAttempts == 5

	drainer := outboxpkg.New(outboxpkg.Config{
		OutboxStore:    outbox,
		LeaseStore:     lease,
		Sender:         sender,
		DLQ:            dlq.New(nil),
		RouteID:        "poison-route",
		PartitionKey:   pk,
		LeaseID:        "sess-poison",
		Policy:         policy,
		Strategy:       persistence.NewFixedPoll(20 * time.Millisecond),
		DrainBatchSize: 10,
		Logger:         logger,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	ctx := context.Background()
	// Rehydrate already past the replay budget so the first claim tips it
	// over MaxReplayAttempts and the drainer routes it straight to the
	// poison DLQ without ever attempting a send.
	poison := persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:          "rec-poison",
		RouteID:     "poison-route",
		EnvelopeID:  "env-poison",
		BindingID:   "bind-1",
		SessionID:   "sess-poison",
		Envelope:    *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-poison", Payload: []byte("good-message")}),
		Status:      persistence.OutboxPending,
		ReplayCount: policy.MaxReplayAttempts + 1,
	})
	if err := outbox.Persist(ctx, []*persistence.OutboxRecord{poison}); err != nil {
		t.Fatalf("persist poison record: %v", err)
	}

	drainCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	entry, ok := handler.find(poisonWarnMsg)
	if !ok {
		t.Fatalf("expected poison WARN %q; captured messages: %v", poisonWarnMsg, handler.messages())
	}
	if entry.level != slog.LevelWarn {
		t.Errorf("poison log level = %v, want WARN", entry.level)
	}
	if got := entry.attrs["record_id"]; got != "rec-poison" {
		t.Errorf("record_id attr = %v, want %q", got, "rec-poison")
	}
	if got, has := attrInt64(entry.attrs["max_replay_attempts"]); !has || got != int64(policy.MaxReplayAttempts) {
		t.Errorf("max_replay_attempts attr = %v, want %d", entry.attrs["max_replay_attempts"], policy.MaxReplayAttempts)
	}
	// replay_count must exceed the budget — that is what makes it poison.
	if got, has := attrInt64(entry.attrs["replay_count"]); !has || got <= int64(policy.MaxReplayAttempts) {
		t.Errorf("replay_count attr = %v, want > %d", entry.attrs["replay_count"], policy.MaxReplayAttempts)
	}
	// Poison is decided before send: a good-but-budget-exhausted message is
	// DLQ'd, never delivered.
	if sender.SentCount() != 0 {
		t.Errorf("poison record must not be sent; SentCount = %d", sender.SentCount())
	}
}

// attrLogRecorder is an slog.Handler that captures level, message and flattened
// attributes for every record, so tests can assert on structured observability
// signals (not just the message text). Thread-safe: Handle may run on drain
// goroutines while the test goroutine reads.
type attrLogRecorder struct {
	mu      sync.Mutex
	entries []capturedLog
}

type capturedLog struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (h *attrLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *attrLogRecorder) Handle(_ context.Context, r slog.Record) error {
	e := capturedLog{level: r.Level, msg: r.Message, attrs: make(map[string]any, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		e.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, e)
	h.mu.Unlock()
	return nil
}

// WithAttrs is a pass-through: the drainer supplies partition/route/record
// context as inline Log args (not via WithAttrs), so no scoping is needed here.
func (h *attrLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *attrLogRecorder) WithGroup(string) slog.Handler      { return h }

func (h *attrLogRecorder) find(msg string) (capturedLog, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.msg == msg {
			return e, true
		}
	}
	return capturedLog{}, false
}

func (h *attrLogRecorder) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.entries))
	for i, e := range h.entries {
		out[i] = e.msg
	}
	return out
}

// attrInt64 coerces a captured slog attribute to int64. slog stores Go ints as
// KindInt64, so Value.Any() yields int64; the int case is defensive.
func attrInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
