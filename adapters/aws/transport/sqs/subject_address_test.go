// Tests covering the subject/address separation contract for the SQS
// adapter:
//
//   - SQS senders are bound to a single queue URL. ports.OutboundMessage
//     .Address must be empty or equal to the resolved queue URL. A
//     mismatch is rejected with shared.ErrInvalidTopic without
//     contacting the SDK or emitting metrics.
//   - SendBatch pre-validates the entire slice; any nil envelope or
//     address mismatch fails the whole batch with sent=0 before any
//     SDK dispatch (fail-fast).
//   - The receiver does NOT fall back to QueueName/QueueURL for
//     Envelope.Subject — when no "Subject" message attribute is
//     present (and SNS unwrap does not provide an inner Subject) the
//     resulting Envelope.Subject is empty.
//   - SNS unwrap with TopicArn but no inner Subject leaves
//     Envelope.Subject empty and preserves TopicArn under
//     headers["sns.topic_arn"].
package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// Sender — Address validation matrix
// ---------------------------------------------------------------------------

// Verifies a non-empty Address that does not match the resolved queue
// URL is rejected with shared.ErrInvalidTopic and the SDK is never
// contacted.
func TestSender_Send_RejectsMismatchedAddress(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageFn: func(_ context.Context, _ *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			t.Fatal("SendMessage must not be called when Address mismatches")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")}),
		Address:  "https://other-queue",
	})
	if err == nil {
		t.Fatal("Send must fail when Address mismatches the configured queue URL")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
	if len(mock.SendCalls) != 0 {
		t.Fatalf("expected 0 SendMessage calls, got %d", len(mock.SendCalls))
	}
}

// Verifies a non-empty Address equal to the resolved queue URL is
// accepted and dispatch proceeds normally.
func TestSender_Send_AcceptsMatchingAddress(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")})
	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: env,
		Address:  "https://q",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(mock.SendCalls) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(mock.SendCalls))
	}
	got := mock.SendCalls[0]
	if got.MessageAttributes["Subject"].StringValue == nil ||
		*got.MessageAttributes["Subject"].StringValue != "evt.x" {
		t.Fatal("Subject attribute must mirror Envelope.Subject regardless of Address")
	}
	if *got.QueueUrl != "https://q" {
		t.Fatalf("queue URL = %q, want %q", *got.QueueUrl, "https://q")
	}
}

// Verifies an empty Address means "use the configured queue URL".
func TestSender_Send_EmptyAddressUsesConfiguredQueue(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("p")}),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(mock.SendCalls) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(mock.SendCalls))
	}
}

// Verifies a nil envelope is rejected with shared.ErrInvalidPayload
// and the SDK is never contacted.
func TestSender_Send_NilEnvelope(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageFn: func(_ context.Context, _ *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			t.Fatal("SendMessage must not be called for nil envelope")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), ports.OutboundMessage{Envelope: nil})
	if err == nil {
		t.Fatal("Send must fail for nil envelope")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
	if len(mock.SendCalls) != 0 {
		t.Fatalf("expected 0 SendMessage calls, got %d", len(mock.SendCalls))
	}
}

// ---------------------------------------------------------------------------
// SendBatch — fail-fast pre-validation
// ---------------------------------------------------------------------------

// Verifies SendBatch fails fast (sent=0, no dispatch) when any entry
// has a mismatched Address.
func TestSendBatch_FailsFastOnAddressMismatch(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			t.Fatal("SendMessageBatch must not be called when any entry mismatches")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok-1", Payload: []byte("a")}), Address: "https://q"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "bad", Payload: []byte("b")}), Address: "https://other"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok-2", Payload: []byte("c")})},
	}

	results, err := sender.SendBatch(context.Background(), msgs)
	if results != nil {
		t.Fatalf("results = %v, want nil (fail-fast, no dispatch)", results)
	}
	if err == nil {
		t.Fatal("SendBatch must return an error when any entry mismatches")
	}
	if !errors.Is(err, shared.ErrInvalidTopic) {
		t.Fatalf("err = %v, want errors.Is(ErrInvalidTopic)", err)
	}
}

// Verifies SendBatch fails fast when any entry has a nil envelope.
func TestSendBatch_FailsFastOnNilEnvelope(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			t.Fatal("SendMessageBatch must not be called when any envelope is nil")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok", Payload: []byte("a")})},
		{Envelope: nil},
	}

	results, err := sender.SendBatch(context.Background(), msgs)
	if results != nil {
		t.Fatalf("results = %v, want nil (fail-fast)", results)
	}
	if err == nil {
		t.Fatal("SendBatch must return an error for nil envelope")
	}
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("err = %v, want errors.Is(ErrInvalidPayload)", err)
	}
}

// Verifies SendBatch joins multiple pre-validation errors when the
// batch contains a mix of nil envelopes and address mismatches, and
// still fails fast (no dispatch).
func TestSendBatch_FailsFastOnMixedViolations(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			t.Fatal("SendMessageBatch must not be called when pre-validation fails")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: nil},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "bad", Payload: []byte("a")}), Address: "https://other"},
	}

	results, err := sender.SendBatch(context.Background(), msgs)
	if results != nil {
		t.Fatalf("results = %v, want nil (fail-fast)", results)
	}
	if err == nil {
		t.Fatal("SendBatch must return an error for mixed violations")
	}
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("missing ErrInvalidPayload in joined error: %v", err)
	}
	if !errors.Is(err, shared.ErrInvalidTopic) {
		t.Fatalf("missing ErrInvalidTopic in joined error: %v", err)
	}
}

// Verifies SendBatch accepts a slice with a mix of empty Address and
// matching Address entries and dispatches them all.
func TestSendBatch_AcceptsEmptyAndMatchingAddress(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("a")})},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Payload: []byte("b")}), Address: "https://q"},
	}

	results, err := sender.SendBatch(context.Background(), msgs)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != 2 {
		t.Fatalf("sent = %d, want 2", sent)
	}
}

// ---------------------------------------------------------------------------
// Receiver — no fallback for Envelope.Subject
// ---------------------------------------------------------------------------

// runReceiverOnce wires a Receiver around a mock that returns a single
// message on the first ReceiveMessage call and blocks afterwards. It
// returns the envelope handed to the handler.
func runReceiverOnce(t *testing.T, cfg ReceiverConfig, msg sqstypes.Message) *messaging.Envelope {
	t.Helper()
	var calls int
	var mu sync.Mutex
	mc := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{msg}}, nil
		},
	}
	cfg.Client = mc
	if cfg.VisibilityTimeout == 0 {
		cfg.VisibilityTimeout = 30
	}
	noAutoExtend := false
	cfg.AutoExtend = &noAutoExtend

	recv, err := NewReceiver(cfg, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var got *messaging.Envelope
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		got = del.Envelope()
		cancel()
		return nil
	})
	if got == nil {
		t.Fatal("handler was not invoked")
	}
	return got
}

// Acceptance test: a message with no "Subject" attribute and
// SNSUnwrap disabled produces an empty Envelope.Subject — no fallback
// to the configured queue name or queue URL.
func TestReceiver_NoSubjectAttribute_YieldsEmptySubject(t *testing.T) {
	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL:  "https://my-queue-url",
		QueueName: "my-queue-name",
	}, sqstypes.Message{
		MessageId:     aws.String("m1"),
		ReceiptHandle: aws.String("rh1"),
		Body:          aws.String("plain body"),
	})

	if env.Subject() != "" {
		t.Fatalf("Envelope.Subject = %q, want empty (no fallback to queue name/URL)", env.Subject())
	}
}

// Verifies that with SNSUnwrap enabled but a body that is NOT an SNS
// notification, Envelope.Subject is still empty.
func TestReceiver_SNSUnwrapEnabled_NonSNSBody_YieldsEmptySubject(t *testing.T) {
	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL:  "https://my-queue-url",
		SNSUnwrap: true,
	}, sqstypes.Message{
		MessageId:     aws.String("m1"),
		ReceiptHandle: aws.String("rh1"),
		Body:          aws.String("not-json"),
	})

	if env.Subject() != "" {
		t.Fatalf("Envelope.Subject = %q, want empty", env.Subject())
	}
}

// Verifies that an SNS notification with a TopicArn but no inner
// Subject leaves Envelope.Subject empty and exposes the TopicArn under
// headers["sns.topic_arn"]. A transport-level topic name must never be
// promoted into the logical Subject.
func TestReceiver_SNSUnwrap_TopicArnOnly_YieldsEmptySubject(t *testing.T) {
	body, _ := json.Marshal(map[string]string{
		"Type":     "Notification",
		"TopicArn": "arn:aws:sns:us-west-1:123:my-topic",
		"Message":  `{"inner":"data"}`,
	})

	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL:  "https://my-queue-url",
		SNSUnwrap: true,
	}, sqstypes.Message{
		MessageId:     aws.String("m-sns"),
		ReceiptHandle: aws.String("rh-sns"),
		Body:          aws.String(string(body)),
	})

	if env.Subject() != "" {
		t.Fatalf("Envelope.Subject = %q, want empty (TopicArn must not be promoted)", env.Subject())
	}
	if env.Headers()["sns.topic_arn"] != "arn:aws:sns:us-west-1:123:my-topic" {
		t.Fatalf("headers[sns.topic_arn] = %v, want the TopicArn", env.Headers()["sns.topic_arn"])
	}
	if string(env.Payload()) != `{"inner":"data"}` {
		t.Fatalf("payload = %q, want unwrapped inner Message", string(env.Payload()))
	}
}

// Verifies SNS notifications with an inner Subject still propagate
// that Subject into Envelope.Subject (SNS subject path remains the
// preferred source when SNSUnwrap is enabled).
func TestReceiver_SNSUnwrap_WithSubject_SetsSubject(t *testing.T) {
	body, _ := json.Marshal(map[string]string{
		"Type":     "Notification",
		"TopicArn": "arn:aws:sns:us-west-1:123:my-topic",
		"Subject":  "Logical-Subject",
		"Message":  `{"inner":"data"}`,
	})

	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL:  "https://my-queue-url",
		SNSUnwrap: true,
	}, sqstypes.Message{
		MessageId:     aws.String("m-sns"),
		ReceiptHandle: aws.String("rh-sns"),
		Body:          aws.String(string(body)),
	})

	if env.Subject() != "Logical-Subject" {
		t.Fatalf("Envelope.Subject = %q, want %q", env.Subject(), "Logical-Subject")
	}
	if env.Headers()["sns.subject"] != "Logical-Subject" {
		t.Fatalf("headers[sns.subject] = %v, want %q", env.Headers()["sns.subject"], "Logical-Subject")
	}
}

// Verifies an explicit "Subject" message attribute populates
// Envelope.Subject (the canonical happy path).
func TestReceiver_SubjectAttribute_PopulatesSubject(t *testing.T) {
	env := runReceiverOnce(t, ReceiverConfig{
		QueueURL: "https://my-queue-url",
	}, sqstypes.Message{
		MessageId:     aws.String("m1"),
		ReceiptHandle: aws.String("rh1"),
		Body:          aws.String("payload"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"Subject": {DataType: aws.String("String"), StringValue: aws.String("evt.created")},
		},
	})

	if env.Subject() != "evt.created" {
		t.Fatalf("Envelope.Subject = %q, want %q", env.Subject(), "evt.created")
	}
}

// ---------------------------------------------------------------------------
// FIFO deduplication review
// ---------------------------------------------------------------------------

// Verifies dedup IDs match for envelopes with identical fields, and
// differ when only Subject differs (one empty vs. populated). This
// pins the documented behavior: subject participation in the hash is
// deliberate.
func TestGenerateDeduplicationID_SubjectMattersForT08(t *testing.T) {
	a := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "evt.x", Payload: []byte("p")})
	b := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "evt.x", Payload: []byte("p")})
	if generateDeduplicationID(a) != generateDeduplicationID(b) {
		t.Fatal("identical envelopes must produce identical dedup IDs")
	}

	withSubject := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "evt.x", Payload: []byte("p")})
	emptySubject := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "", Payload: []byte("p")})
	if generateDeduplicationID(withSubject) == generateDeduplicationID(emptySubject) {
		t.Fatal("dedup IDs must differ when Subject differs (one empty)")
	}

	otherSubject := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "evt.y", Payload: []byte("p")})
	if generateDeduplicationID(withSubject) == generateDeduplicationID(otherSubject) {
		t.Fatal("dedup IDs must differ when Subject differs")
	}
}

// Sanity guard: the address-mismatch error message includes both the
// mismatching address and the configured queue URL so on-call has a
// usable diagnostic without rerunning with trace logs.
func TestSender_Send_MismatchErrorMessageContainsBothAddresses(t *testing.T) {
	sender, err := NewSender(SenderConfig{QueueURL: "https://configured", Client: &mockSQSClient{}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("p")}),
		Address:  "https://other",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "https://other") || !strings.Contains(msg, "https://configured") {
		t.Fatalf("error message %q must include both addresses", msg)
	}
}
