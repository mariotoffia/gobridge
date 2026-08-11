package route

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestReceiveCount_TransportBases pins the per-transport normalization of
// receiveCount: SQS and ASB counts are already 1-based, while the amqp10 raw
// AMQP delivery-count is 0-based and must be incremented. Regression for
// (asb) / — before the fix only the SQS header was read, so ASB
// and amqp10 sources always reported 0 and MaxReplayAttempts never fired.
func TestReceiveCount_TransportBases(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]any
		want    int
	}{
		{"no count header", map[string]any{"other": "x"}, 0},
		{"sqs 1-based int", map[string]any{headerSQSReceiveCount: 3}, 3},
		{"sqs first delivery", map[string]any{headerSQSReceiveCount: 1}, 1},
		{"sqs decimal string", map[string]any{headerSQSReceiveCount: "4"}, 4},
		{"sqs int64", map[string]any{headerSQSReceiveCount: int64(5)}, 5},
		{"asb 1-based int", map[string]any{headerASBDeliveryCount: 3}, 3},
		{"asb first delivery", map[string]any{headerASBDeliveryCount: 1}, 1},
		{"amqp10 0-based uint32", map[string]any{headerAMQP10DeliveryCount: uint32(3)}, 4},
		{"amqp10 first delivery (raw 0)", map[string]any{headerAMQP10DeliveryCount: uint32(0)}, 1},
		{
			name:    "precedence sqs wins over amqp10",
			headers: map[string]any{headerSQSReceiveCount: 2, headerAMQP10DeliveryCount: uint32(9)},
			want:    2,
		},
		{
			name:    "precedence sqs wins over asb",
			headers: map[string]any{headerSQSReceiveCount: 2, headerASBDeliveryCount: 9},
			want:    2,
		},
		{
			name:    "precedence asb wins over amqp10",
			headers: map[string]any{headerASBDeliveryCount: 5, headerAMQP10DeliveryCount: uint32(0)},
			want:    5,
		},
		{"sqs float64 (json-decoded)", map[string]any{headerSQSReceiveCount: float64(7)}, 7},
		{"sqs malformed string -> first delivery", map[string]any{headerSQSReceiveCount: "notanumber"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "rc",
				Payload: []byte("x"),
				Headers: tt.headers,
			})
			if got := receiveCount(env); got != tt.want {
				t.Errorf("receiveCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestReceiveCount_NoHeaders verifies an envelope with no headers is treated as
// a first delivery (count 0).
func TestReceiveCount_NoHeaders(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "rc", Payload: []byte("x")})
	if got := receiveCount(env); got != 0 {
		t.Errorf("receiveCount() = %d, want 0", got)
	}
}

// TestStripInboundReceiveCounts pins egress chokepoint helper: it
// must delete EVERY transport redelivery-count header so a stale upstream count
// cannot ride a bridge-to-bridge hop, while leaving all other headers untouched.
func TestStripInboundReceiveCounts(t *testing.T) {
	t.Run("removes every count header, keeps the rest", func(t *testing.T) {
		// WHY: bridge B may receive an envelope carrying every upstream count
		// header at once (defensive) alongside a benign app header and a
		// transport-namespaced sibling that is NOT a count. The strip must be
		// key-exact: only the three count keys go, proving it is not a blunt
		// "asb.*" prefix wipe that would also clobber unrelated transport
		// metadata. (A true x-bridge.* reserved header cannot be built via
		// MustEnvelope — its panic guard — so the sibling is the witness.)
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "strip",
			Payload: []byte("p"),
			Headers: map[string]any{
				headerSQSReceiveCount:     3,
				headerASBDeliveryCount:    5,
				headerAMQP10DeliveryCount: uint32(7),
				"x-app.tenant":            "acme",
				"asb.enqueued-time":       "2025-01-01T00:00:00Z",
			},
		})

		stripInboundReceiveCounts(env)

		for _, k := range []string{headerSQSReceiveCount, headerASBDeliveryCount, headerAMQP10DeliveryCount} {
			if _, ok := env.Headers()[k]; ok {
				t.Errorf("count header %q must be stripped", k)
			}
		}
		if v, ok := env.Headers()["x-app.tenant"]; !ok || v != "acme" {
			t.Errorf("benign header x-app.tenant = %v (present=%v), want \"acme\"", v, ok)
		}
		if _, ok := env.Headers()["asb.enqueued-time"]; !ok {
			t.Error("transport-namespaced non-count header asb.enqueued-time must survive the strip")
		}
	})

	t.Run("stale upstream count can no longer win receiveCount", func(t *testing.T) {
		// WHY: cross-hop precedence proof. Before the strip the stale upstream
		// asb count (9) is what receiveCount returns; after the strip it returns
		// 0 (first delivery), so bridge B establishes its own count instead of
		// inheriting bridge A's.
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "precedence",
			Payload: []byte("p"),
			Headers: map[string]any{headerASBDeliveryCount: 9},
		})
		if got := receiveCount(env); got != 9 {
			t.Fatalf("receiveCount before strip = %d, want 9 (stale upstream count wins)", got)
		}
		stripInboundReceiveCounts(env)
		if got := receiveCount(env); got != 0 {
			t.Fatalf("receiveCount after strip = %d, want 0 (stale count no longer rides the hop)", got)
		}
	})

	t.Run("nil headers do not panic", func(t *testing.T) {
		// WHY: DeleteHeader is nil-safe; a first-delivery envelope built without
		// headers must strip cleanly rather than panic.
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x", Payload: []byte("p")})
		stripInboundReceiveCounts(env) // must not panic
	})
}
