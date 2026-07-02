package route

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestReceiveCount_TransportBases pins the per-transport normalization of
// receiveCount: SQS and ASB counts are already 1-based, while the amqp10 raw
// AMQP delivery-count is 0-based and must be incremented. Regression for
// E5 (asb) / E5-AMQP10 — before the fix only the SQS header was read, so ASB
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
