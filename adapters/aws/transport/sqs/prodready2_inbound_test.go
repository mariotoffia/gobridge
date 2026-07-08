package sqs

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestApproximateReceiveCount parses the SQS ApproximateReceiveCount system
// attribute used by the Finding 6 poison escalation. Absent or unparseable
// values are treated as 0 (no escalation).
func TestApproximateReceiveCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		attrs map[string]string
		want  int
	}{
		{"absent", map[string]string{}, 0},
		{"parseable", map[string]string{"ApproximateReceiveCount": "7"}, 7},
		{"at threshold", map[string]string{"ApproximateReceiveCount": "10"}, 10},
		{"garbage", map[string]string{"ApproximateReceiveCount": "not-a-number"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approximateReceiveCount(tc.attrs); got != tc.want {
				t.Fatalf("approximateReceiveCount(%v) = %d, want %d", tc.attrs, got, tc.want)
			}
		})
	}
}

// newZeroClockReceiver builds a Receiver whose clock returns the zero time so
// convertMessage fails with ErrEnvelopeClockMissing — the only way to exercise
// the malformed/poison drop branch, since a message's ID is otherwise
// defaulted (see TestSQSPollAndConvert_DropsMalformedMessage). The single
// crafted message is returned on ReceiveMessage; logs go to h.
func newZeroClockReceiver(t *testing.T, msg sqstypes.Message, h slog.Handler) *Receiver {
	t.Helper()
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{msg}}, nil
		},
	}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "http://test/q",
		Client:   mock,
		Clock:    clocktest.NewAt(time.Time{}),
		Logger:   slog.New(h),
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	r.storeClient(mock)
	return r
}

// TestPollAndConvert_PoisonMessageEscalation is the regression for Finding 6.
// Malformed messages are dropped WITHOUT a Delete so the queue's own
// maxReceiveCount redrive policy can DLQ them; with no redrive policy the
// message redelivers forever. Once ApproximateReceiveCount crosses the sanity
// bound the drop must escalate from a Warn to an Error so operators get a
// signal that a redrive policy is likely missing. Settlement is unchanged.
func TestPollAndConvert_PoisonMessageEscalation(t *testing.T) {
	t.Parallel()

	poison := func(recvCount string) sqstypes.Message {
		return sqstypes.Message{
			MessageId:     aws.String("poison-1"),
			ReceiptHandle: aws.String("rh"),
			Body:          aws.String("x"),
			// No SentTimestamp: createdAt stays zero so NewEnvelope fails.
			Attributes: map[string]string{"ApproximateReceiveCount": recvCount},
		}
	}

	t.Run("at or above threshold escalates to Error", func(t *testing.T) {
		h := &captureHandler{}
		r := newZeroClockReceiver(t, poison("15"), h)

		results, err := r.pollAndConvert(context.Background(), "http://test/q", 10, 0)
		if err != nil {
			t.Fatalf("pollAndConvert: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("results = %d, want 0 (poison message dropped)", len(results))
		}

		var escalated bool
		for _, rc := range h.snapshot() {
			if rc.Level >= slog.LevelError {
				var hasCount bool
				rc.Attrs(func(a slog.Attr) bool {
					if a.Key == "approximate_receive_count" {
						hasCount = true
						return false
					}
					return true
				})
				if hasCount {
					escalated = true
				}
			}
		}
		if !escalated {
			t.Fatal("Finding 6: a poison message past the receive-count bound must emit an Error-level log")
		}
	})

	t.Run("below threshold stays Warn", func(t *testing.T) {
		h := &captureHandler{}
		r := newZeroClockReceiver(t, poison("3"), h)

		if _, err := r.pollAndConvert(context.Background(), "http://test/q", 10, 0); err != nil {
			t.Fatalf("pollAndConvert: %v", err)
		}
		for _, rc := range h.snapshot() {
			if rc.Level >= slog.LevelError {
				t.Fatalf("unexpected Error-level log below the poison threshold: %q", rc.Message)
			}
		}
	})
}

// TestBridgeAttrString_PrefersExactCaseDeterministically is the regression for
// Finding 12. When both an exact-case and a case-variant attribute are
// present, the idempotency-key lift iterated the map and let Go's randomised
// iteration order pick a winner. The fix prefers the exact-case value, then
// falls back to a deterministic (smallest-key) fold scan.
func TestBridgeAttrString_PrefersExactCaseDeterministically(t *testing.T) {
	t.Parallel()

	key := messaging.HeaderIdempotencyKey
	variant := strings.ToUpper(key)
	require.NotEqual(t, key, variant, "need a distinct case-variant for the test")

	t.Run("exact case wins over a variant", func(t *testing.T) {
		attrs := map[string]sqstypes.MessageAttributeValue{
			key:     {DataType: aws.String("String"), StringValue: aws.String("exact")},
			variant: {DataType: aws.String("String"), StringValue: aws.String("VARIANT")},
		}
		for i := 0; i < 200; i++ {
			require.Equalf(t, "exact", bridgeAttrString(attrs, key),
				"iteration %d: exact-case value must always win", i)
		}
	})

	t.Run("fold fallback picks the smallest key deterministically", func(t *testing.T) {
		// Neither key matches the canonical lowercase key exactly, so the
		// fold scan runs and must pick the lexicographically smallest match.
		attrs := map[string]sqstypes.MessageAttributeValue{
			"X-BRIDGE.IDEMPOTENCY-KEY": {DataType: aws.String("String"), StringValue: aws.String("A")},
			"x-bridge.IDEMPOTENCY-key": {DataType: aws.String("String"), StringValue: aws.String("B")},
		}
		for i := 0; i < 200; i++ {
			require.Equalf(t, "A", bridgeAttrString(attrs, key),
				"iteration %d: smallest matching key must always win", i)
		}
	})
}
