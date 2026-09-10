package sqs

// Production-readiness regression tests for the inbound ACL:
//
//   - SNS unwrap requires Type == "Notification" — a bare TopicArn key
//     in arbitrary producer JSON must not be unwrapped with forged
//     sns.* headers.
//   - Envelope.CreatedAt prefers the broker SentTimestamp over the
//     bridge receive time so TTL/expiry policies measure true message
//     age across hops.
//   - SQSReceiveLatency measures receive WORK, excluding the
//     intentional long-poll idle before a message arrived.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// SNS unwrap type gate
// ---------------------------------------------------------------------------

// Verifies trySNSUnwrap unwraps only real SNS notification shapes
// (Type == "Notification" AND non-empty TopicArn) and never stamps
// sns.* headers for anything else.
func TestTrySNSUnwrap_TypeGate(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "notification with topic arn is unwrapped",
			body: `{"Type":"Notification","TopicArn":"arn:aws:sns:us-east-1:1:t","Message":"m"}`,
			want: true,
		},
		{
			name: "bare TopicArn without Type is NOT unwrapped (forgeable)",
			body: `{"TopicArn":"arn:aws:sns:us-east-1:1:t","Subject":"forged","Message":"m"}`,
			want: false,
		},
		{
			name: "subscription confirmation is NOT unwrapped",
			body: `{"Type":"SubscriptionConfirmation","TopicArn":"arn:aws:sns:us-east-1:1:t","Message":"m"}`,
			want: false,
		},
		{
			name: "notification without TopicArn is NOT unwrapped",
			body: `{"Type":"Notification","Message":"m"}`,
			want: false,
		},
		{
			name: "non-JSON body is NOT unwrapped",
			body: `plain text`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]any{}
			_, ok := trySNSUnwrap(tt.body, headers)
			if ok != tt.want {
				t.Fatalf("trySNSUnwrap ok = %v, want %v", ok, tt.want)
			}
			if !tt.want && len(headers) != 0 {
				t.Fatalf("headers = %v, want none stamped when the body is not unwrapped", headers)
			}
		})
	}
}

// Verifies end-to-end that a producer body merely containing a TopicArn
// key passes through verbatim: payload untouched, no sns.* headers, no
// forged subject.
func TestReceiver_SNSUnwrap_IgnoresBareTopicArnJSON(t *testing.T) {
	body := `{"TopicArn":"arn:aws:sns:us-east-1:1:t","Subject":"forged","Message":"inner"}`
	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL:  "https://my-queue-url",
		SNSUnwrap: true,
	}, sqstypes.Message{
		MessageId:     aws.String("m-forged"),
		ReceiptHandle: aws.String("rh-forged"),
		Body:          aws.String(body),
	})

	if string(env.Payload()) != body {
		t.Fatalf("payload = %q, want the raw body passed through", string(env.Payload()))
	}
	if _, ok := env.Headers()["sns.topic_arn"]; ok {
		t.Fatal("sns.topic_arn must not be stamped for a non-Notification body")
	}
	if env.Subject() != "" {
		t.Fatalf("Envelope.Subject = %q, want empty (no forged SNS subject)", env.Subject())
	}
}

// ---------------------------------------------------------------------------
// CreatedAt from SentTimestamp
// ---------------------------------------------------------------------------

// Verifies Envelope.CreatedAt is the broker SentTimestamp when present,
// so TTL/expiry policies measure the message's true age including queue
// dwell time.
func TestConvertMessage_CreatedAtFromSentTimestamp(t *testing.T) {
	sent := time.UnixMilli(1_700_000_000_000)
	receive := time.UnixMilli(1_700_000_090_000) // 90s later
	fake := clocktest.NewAt(receive)

	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL: "https://my-queue-url",
		Clock:    fake,
	}, sqstypes.Message{
		MessageId:     aws.String("m-sent"),
		ReceiptHandle: aws.String("rh-sent"),
		Body:          aws.String("b"),
		Attributes:    map[string]string{"SentTimestamp": "1700000000000"},
	})

	if !env.CreatedAt().Equal(sent) {
		t.Fatalf("CreatedAt = %v, want broker SentTimestamp %v", env.CreatedAt(), sent)
	}
}

// Verifies the fallback: without a SentTimestamp system attribute,
// CreatedAt is the bridge receive time.
func TestConvertMessage_CreatedAtFallsBackToReceiveTime(t *testing.T) {
	receive := time.UnixMilli(1_700_000_090_000)
	fake := clocktest.NewAt(receive)

	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL: "https://my-queue-url",
		Clock:    fake,
	}, sqstypes.Message{
		MessageId:     aws.String("m-nosent"),
		ReceiptHandle: aws.String("rh-nosent"),
		Body:          aws.String("b"),
	})

	if !env.CreatedAt().Equal(receive) {
		t.Fatalf("CreatedAt = %v, want receive time %v", env.CreatedAt(), receive)
	}
}

// ---------------------------------------------------------------------------
// SQSReceiveLatency — receive work, not long-poll idle
// ---------------------------------------------------------------------------

// Verifies the per-message latency computation in isolation.
func TestReceiveWorkLatency(t *testing.T) {
	pollStart := time.UnixMilli(1_700_000_000_000)
	sysAttrs := func(sent time.Time) map[string]string {
		return map[string]string{"SentTimestamp": timeToMillisString(sent)}
	}

	tests := []struct {
		name  string
		attrs map[string]string
		end   time.Time
		want  time.Duration
	}{
		{
			name:  "message arrived mid-poll: only post-arrival time counts",
			attrs: sysAttrs(pollStart.Add(18 * time.Second)),
			end:   pollStart.Add(19 * time.Second),
			want:  time.Second,
		},
		{
			name:  "message queued before the poll: full poll duration counts",
			attrs: sysAttrs(pollStart.Add(-time.Minute)),
			end:   pollStart.Add(50 * time.Millisecond),
			want:  50 * time.Millisecond,
		},
		{
			name:  "no SentTimestamp: falls back to poll start",
			attrs: nil,
			end:   pollStart.Add(20 * time.Second),
			want:  20 * time.Second,
		},
		{
			name:  "broker clock skew past receive end: clamped to zero",
			attrs: sysAttrs(pollStart.Add(25 * time.Second)),
			end:   pollStart.Add(20 * time.Second),
			want:  0,
		},
		{
			name:  "malformed SentTimestamp: falls back to poll start",
			attrs: map[string]string{"SentTimestamp": "not-a-number"},
			end:   pollStart.Add(3 * time.Second),
			want:  3 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := receiveWorkLatency(pollStart, tt.end, tt.attrs)
			if got != tt.want {
				t.Fatalf("receiveWorkLatency = %v, want %v", got, tt.want)
			}
		})
	}
}

func timeToMillisString(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// Verifies through pollAndConvert that a message arriving late in a
// long poll records only the post-arrival work, and that a quiet poll
// (no messages) records no SQSReceiveLatency at all.
func TestPollAndConvert_ReceiveLatencyExcludesLongPollIdle(t *testing.T) {
	base := time.UnixMilli(1_700_000_000_000)
	fake := clocktest.NewAt(base)
	rec := &ports.RecordingExporter{}

	sentAt := base.Add(19 * time.Second) // arrives 19s into the poll
	var polls int
	mock := &mockSQSClient{
		ReceiveMessageFn: func(_ context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			polls++
			if polls == 1 {
				// The long poll "takes" 19.5s of fake time before
				// returning the message that arrived at +19s.
				fake.Advance(19500 * time.Millisecond)
				return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{{
					MessageId:     aws.String("m-1"),
					ReceiptHandle: aws.String("rh-1"),
					Body:          aws.String("b"),
					Attributes:    map[string]string{"SentTimestamp": timeToMillisString(sentAt)},
				}}}, nil
			}
			// Quiet poll: full wait elapses, nothing returned.
			fake.Advance(20 * time.Second)
			return &awssqs.ReceiveMessageOutput{}, nil
		},
	}

	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "https://q",
		Client:   mock,
		Clock:    fake,
		Metrics:  rec,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	if err := r.ensureClient(context.Background()); err != nil {
		t.Fatalf("ensureClient: %v", err)
	}

	if _, err := r.pollAndConvert(context.Background(), r.loadClient(), "https://q", r.cfg.MaxMessages, time.Minute); err != nil {
		t.Fatalf("pollAndConvert #1: %v", err)
	}
	entries := rec.FindEntries(MetricSQSReceiveLatency)
	if len(entries) != 1 {
		t.Fatalf("SQSReceiveLatency entries = %d, want 1", len(entries))
	}
	if got, want := entries[0].Duration, 500*time.Millisecond; got != want {
		t.Fatalf("SQSReceiveLatency = %v, want %v (idle until arrival excluded)", got, want)
	}

	rec.Reset()
	if _, err := r.pollAndConvert(context.Background(), r.loadClient(), "https://q", r.cfg.MaxMessages, time.Minute); err != nil {
		t.Fatalf("pollAndConvert #2: %v", err)
	}
	if n := len(rec.FindEntries(MetricSQSReceiveLatency)); n != 0 {
		t.Fatalf("quiet poll recorded %d SQSReceiveLatency entries, want 0", n)
	}
}
