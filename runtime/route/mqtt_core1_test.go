package route

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// generatedIDEnv builds a COUNT-LESS envelope whose identity is marked
// ADAPTER-GENERATED (messaging.HeaderGeneratedID) — the shape an MQTT source
// produces for a publish without a stable producer mqtt.message-id / correlation
// data. The reserved marker is set through SetHeader (the trusted per-key path);
// MustEnvelope would panic on a reserved key supplied via EnvelopeInput.Headers.
func generatedIDEnv(id string) *messaging.Envelope {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Payload: []byte("p")})
	env.SetHeader(messaging.HeaderGeneratedID, "true")
	return env
}

// TestMQTTCore1_UncountableRedelivery_PoisonsWithoutRetry proves
// fix: a count-less source whose identity is adapter-generated cannot be counted
// by the replay ledger (each broker redelivery mints a fresh id, resetting the
// count), so a deterministically-failing message would recycle the source
// session forever. It must instead poison TERMINALLY on the first failure —
// acked, never retried — under a finite MaxReplayAttempts.
//
// Mutation check: delete the uncountableRedelivery branch in replayCapReached and
// this fails — the delivery is retried instead of poisoned (del.retried true).
func TestMQTTCore1_UncountableRedelivery_PoisonsWithoutRetry(t *testing.T) {
	const cap = 3
	store := &recordingDLQStore{}
	rec := &ports.RecordingExporter{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "core1",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: cap,
		},
		Sender:  stubSender{err: shared.ErrUnavailable}, // deterministic transient failure
		DLQ:     dlq.New(store),
		Metrics: rec,
	})

	del := &stubDelivery{env: generatedIDEnv("core1-adapter-minted")}
	if err := r.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if del.retried {
		t.Fatalf("uncountable redelivery was retried; want terminal poison on first failure")
	}
	if !del.acked {
		t.Fatalf("uncountable redelivery not settled; want terminal ack")
	}
	if got := store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1 (poison to DLQ on the uncountable first failure)", got)
	}
}

// TestMQTTCore1_GeneratedID_WithStableKey_StillRetries proves the fix is scoped:
// a generated-identity message that ALSO carries a stable bridge key (dedup id)
// is countable again — the ledger keys on the stable key across redeliveries — so
// it retries to the cap exactly like any count-less-but-stable source, and does
// NOT poison prematurely.
//
// Mutation check: drop the dedup/idempotency guard in uncountableRedelivery and
// this fails — the message poisons at delivery 0 instead of after cap retries.
func TestMQTTCore1_GeneratedID_WithStableKey_StillRetries(t *testing.T) {
	const cap = 3
	store := &recordingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "core1-dedup",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: cap,
			// TrustBridgeHeaders keeps the BRIDGE-TO-BRIDGE dedup id across ingress —
			// the default posture strips all reserved headers, so a stable key only
			// survives on a trusted bridge-to-bridge receiver.
			TrustBridgeHeaders: true,
		},
		Sender:  stubSender{err: shared.ErrUnavailable},
		DLQ:     dlq.New(store),
		Metrics: &ports.RecordingExporter{},
	})

	env := generatedIDEnv("core1-with-dedup")
	env.SetHeader(messaging.HeaderDeduplicationID, "stable-dedup-1")

	retries := 0
	poisonedAt := -1
	for i := range cap + 5 {
		del := &stubDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("delivery %d: HandleDelivery: %v", i, err)
		}
		switch {
		case del.retried && !del.acked:
			retries++
		case del.acked && !del.retried:
			poisonedAt = i
		default:
			t.Fatalf("delivery %d: ambiguous settlement acked=%v retried=%v", i, del.acked, del.retried)
		}
		if poisonedAt >= 0 {
			break
		}
	}
	if poisonedAt != cap {
		t.Fatalf("poisoned at delivery %d, want %d (a stable dedup key restores countability)", poisonedAt, cap)
	}
	if retries != cap {
		t.Fatalf("retries before poison = %d, want %d", retries, cap)
	}
}

// TestMQTTCore1_UncountablePredicate_GatedOnFiniteCap unit-checks the
// MaxReplayAttempts<=0 guard in uncountableRedelivery directly. The route policy
// normalizes a 0/negative cap to DefaultMaxReplayAttempts (so a live route always
// has a finite cap), but the predicate keeps the guard as defense in depth for a
// raw policy — an uncountable message must never be treated as capped when no
// finite cap is set.
func TestMQTTCore1_UncountablePredicate_GatedOnFiniteCap(t *testing.T) {
	env := generatedIDEnv("core1-raw")
	base := &RouteRunner{policy: routing.RoutePolicy{MaxReplayAttempts: 3}}
	if !base.uncountableRedelivery(env) {
		t.Fatalf("finite cap + generated identity: want uncountable=true")
	}
	nocap := &RouteRunner{policy: routing.RoutePolicy{MaxReplayAttempts: 0}}
	if nocap.uncountableRedelivery(env) {
		t.Fatalf("no finite cap: want uncountable=false (never override an unset cap)")
	}
}
