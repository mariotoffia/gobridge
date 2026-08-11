// Tests covering the subject/address separation contract for the
// Azure Service Bus adapter:
//
//   - Service Bus senders are bound to a single queue or topic
//     (cfg.QueueName / cfg.TopicName). ports.OutboundMessage.Address
//     must be empty or equal to the resolved entity name. A mismatch
//     is rejected with shared.ErrInvalidTopic without contacting the
//     SDK or emitting metrics.
//   - SendBatch pre-validates the entire slice; any nil envelope or
//     address mismatch fails the whole batch with sent=0 before any
//     SDK dispatch (fail-fast), with errors joined via errors.Join.
//   - The receiver does NOT fall back to QueueName/TopicName for
//     Envelope.Subject — when the broker carries no native Service Bus
//     Subject the resulting Envelope.Subject is empty.
package servicebus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingMetrics is a minimal MetricsExporter that records every
// emission, used to assert that mismatched-address rejections never
// emit transport metrics.
type recordingMetrics struct {
	mu     sync.Mutex
	timers int
	counts int
}

func (r *recordingMetrics) Counter(_ string, _ int64, _ ...shared.Tag) {
	r.mu.Lock()
	r.counts++
	r.mu.Unlock()
}

func (r *recordingMetrics) Gauge(_ string, _ float64, _ ...shared.Tag)     {}
func (r *recordingMetrics) Histogram(_ string, _ float64, _ ...shared.Tag) {}

func (r *recordingMetrics) Timer(_ string, _ time.Duration, _ ...shared.Tag) {
	r.mu.Lock()
	r.timers++
	r.mu.Unlock()
}

func (r *recordingMetrics) Flush(_ context.Context) error { return nil }
func (r *recordingMetrics) Close(_ context.Context) error { return nil }

func (r *recordingMetrics) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.timers + r.counts
}

// ---------------------------------------------------------------------------
// Sender — Address validation matrix
// ---------------------------------------------------------------------------

// Verifies a non-empty Address that does not match the configured
// entity is rejected with shared.ErrInvalidTopic, the SDK is never
// contacted, and no metric is emitted.
func TestSender_Send_RejectsMismatchedAddress(t *testing.T) {
	mock := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			t.Fatal("SendMessage must not be called when Address mismatches")
			return nil
		},
	}
	rec := &recordingMetrics{}
	sender, err := NewSender(SenderConfig{
		QueueName: "configured-queue",
		Client:    mock,
		Metrics:   rec,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")}),
		Address:  "other-queue",
	})
	if err == nil {
		t.Fatal("Send must fail when Address mismatches the configured entity")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
	if len(mock.sentMessages) != 0 {
		t.Fatalf("expected 0 SendMessage calls, got %d", len(mock.sentMessages))
	}
	if rec.total() != 0 {
		t.Fatalf("no metric must be emitted on address mismatch, got %d emissions", rec.total())
	}
}

// Verifies a non-empty Address equal to the configured entity is
// accepted and dispatch proceeds normally.
func TestSender_Send_AcceptsMatchingAddress(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "configured-queue",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte("p")})
	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: env,
		Address:  "configured-queue",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(mock.sentMessages) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(mock.sentMessages))
	}
	got := mock.sentMessages[0]
	if got.Subject == nil || *got.Subject != "evt.x" {
		t.Fatal("native Subject must mirror Envelope.Subject regardless of Address")
	}
}

// Verifies an empty Address means "use the configured entity".
func TestSender_Send_EmptyAddressUsesConfiguredEntity(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "configured-queue",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("p")}),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(mock.sentMessages) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(mock.sentMessages))
	}
}

// Verifies a nil envelope is rejected with shared.ErrInvalidPayload
// and the SDK is never contacted.
func TestSender_Send_NilEnvelope(t *testing.T) {
	mock := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			t.Fatal("SendMessage must not be called for nil envelope")
			return nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "configured-queue", Client: mock})
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
	if len(mock.sentMessages) != 0 {
		t.Fatalf("expected 0 SendMessage calls, got %d", len(mock.sentMessages))
	}
}

// Sanity guard: the address-mismatch error message includes both the
// mismatching address and the configured entity so on-call has a
// usable diagnostic without rerunning with trace logs.
func TestSender_Send_MismatchErrorMessageContainsBothAddresses(t *testing.T) {
	sender, err := NewSender(SenderConfig{QueueName: "configured-queue", Client: &mockSenderAPI{}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("p")}),
		Address:  "other-queue",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "other-queue") || !strings.Contains(msg, "configured-queue") {
		t.Fatalf("error message %q must include both addresses", msg)
	}
}

// ---------------------------------------------------------------------------
// SendBatch — fail-fast pre-validation
// ---------------------------------------------------------------------------

// Verifies SendBatch fails fast (sent=0, no dispatch) when any entry
// has a mismatched Address.
func TestSendBatch_FailsFastOnAddressMismatch(t *testing.T) {
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			t.Fatal("NewMessageBatch must not be called when any entry mismatches")
			return nil, nil
		},
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			t.Fatal("SendMessage must not be called when any entry mismatches")
			return nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "configured-queue", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok-1", Payload: []byte("a")}), Address: "configured-queue"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "bad", Payload: []byte("b")}), Address: "other-queue"},
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
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			t.Fatal("NewMessageBatch must not be called when any envelope is nil")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "configured-queue", Client: mock})
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
// still fails fast (no dispatch). The joined error must surface both
// sentinels via errors.Is.
func TestSendBatch_FailsFastOnMixedViolations(t *testing.T) {
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			t.Fatal("NewMessageBatch must not be called when pre-validation fails")
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "configured-queue", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: nil},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "bad", Payload: []byte("a")}), Address: "other-queue"},
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
// matching Address entries. Because *azservicebus.MessageBatch is an
// SDK-private struct that cannot be constructed in tests, this test
// asserts only that pre-validation does not abort dispatch — i.e.
// NewMessageBatch is invoked at least once (the call site reachable
// only after pre-validation accepts the slice). The dispatch outcome
// after that is exercised by the existing batch sender tests.
func TestSendBatch_AcceptsEmptyAndMatchingAddress(t *testing.T) {
	var newBatchCalls int
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			newBatchCalls++
			return nil, nil
		},
		sendMessageBatchFn: func(context.Context, *azservicebus.MessageBatch, *azservicebus.SendMessageBatchOptions) error {
			return nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "configured-queue", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("a")})},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Payload: []byte("b")}), Address: "configured-queue"},
	}

	_, _ = sender.SendBatch(context.Background(), msgs)
	if newBatchCalls == 0 {
		t.Fatal("expected dispatch to proceed past pre-validation (NewMessageBatch should be called)")
	}
}

// ---------------------------------------------------------------------------
// Receiver — no fallback for Envelope.Subject
// ---------------------------------------------------------------------------

// runReceiverOnce wires a Receiver around a mock that returns a single
// message on the first ReceiveMessages call and blocks afterwards.
func runReceiverOnce(t *testing.T, cfg ReceiverConfig, msg *azservicebus.ReceivedMessage) *messaging.Envelope {
	t.Helper()
	var calls int
	var mu sync.Mutex
	mc := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{msg}, nil
		},
	}
	cfg.Client = mc
	cfg.AutoExtend = boolPtr(false)

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

// Acceptance test: a queue message with no native Service Bus
// Subject yields an empty Envelope.Subject. The configured QueueName
// must NOT be promoted into Envelope.Subject.
func TestReceiver_NoNativeSubject_QueueYieldsEmptySubject(t *testing.T) {
	env := runReceiverOnce(t, ReceiverConfig{QueueName: "my-queue"}, &azservicebus.ReceivedMessage{
		MessageID: "m1",
		Body:      []byte("payload"),
	})
	if env.Subject() != "" {
		t.Fatalf("Envelope.Subject = %q, want empty (no fallback to queue name)", env.Subject())
	}
}

// Acceptance test: a topic/subscription message with no
// native Service Bus Subject yields an empty Envelope.Subject. The
// configured TopicName must NOT be promoted into Envelope.Subject.
func TestReceiver_NoNativeSubject_TopicYieldsEmptySubject(t *testing.T) {
	env := runReceiverOnce(t, ReceiverConfig{
		TopicName:        "my-topic",
		SubscriptionName: "my-sub",
	}, &azservicebus.ReceivedMessage{
		MessageID: "m-topic",
		Body:      []byte("payload"),
	})
	if env.Subject() != "" {
		t.Fatalf("Envelope.Subject = %q, want empty (no fallback to topic name)", env.Subject())
	}
}

// Verifies that a received message carrying a native Service Bus
// Subject populates Envelope.Subject from that field (canonical happy
// path).
func TestReceiver_NativeSubject_PopulatesEnvelopeSubject(t *testing.T) {
	env := runReceiverOnce(t, ReceiverConfig{QueueName: "my-queue"}, &azservicebus.ReceivedMessage{
		MessageID: "m1",
		Body:      []byte("payload"),
		Subject:   strPtr("evt.created"),
	})
	if env.Subject() != "evt.created" {
		t.Fatalf("Envelope.Subject = %q, want %q", env.Subject(), "evt.created")
	}
}
