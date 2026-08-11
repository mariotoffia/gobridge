package servicebus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// requireReport asserts a value arrives on ch before a failsafe deadline. The
// deadline is a test failsafe (mirrors testutil/wait.RequireReceive), NOT a
// synchronization sleep: the callback fires synchronously within the live op.
func requireReport(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("expected reactive-recovery callback, timed out")
		return nil
	}
}

func requireNoReport(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("expected NO reactive-recovery callback, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// a live Send that maps to shared.ErrNotAuthorized (SAS/AAD
// revocation) invokes the injected reactive-recovery callback, forcing an
// immediate re-resolve. Reverting the s.reportAuthFailure(err) call in
// Sender.Send makes this fail.
func TestSender_SendAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	mock := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			return errors.New("Unauthorized access: SAS token expired")
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	reported := make(chan error, 1)
	sender.SetAuthFailureCallback(func(err error) { reported <- err })

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("x")})
	sendErr := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if !errors.Is(sendErr, shared.ErrNotAuthorized) {
		t.Fatalf("Send err = %v, want ErrNotAuthorized", sendErr)
	}

	got := requireReport(t, reported)
	if !errors.Is(got, shared.ErrNotAuthorized) {
		t.Fatalf("callback err = %v, want ErrNotAuthorized", got)
	}
}

// a non-auth Send error must NOT trigger a reactive re-resolve.
func TestSender_SendNonAuthError_DoesNotReport(t *testing.T) {
	mock := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			return errors.New("server busy: too many requests")
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	reported := make(chan error, 1)
	sender.SetAuthFailureCallback(func(err error) { reported <- err })

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("x")})
	_ = sender.Send(context.Background(), ports.OutboundMessage{Envelope: env})

	requireNoReport(t, reported)
}

// a batch-ONLY sender (no single-Send, no receive path) must still
// fire the reactive-recovery report when a live SendBatch maps to
// shared.ErrNotAuthorized — the aggregated per-message results are its sole
// live-op signal of a credential revocation. Reverting the ErrNotAuthorized
// scan at the end of Sender.SendBatch makes this fail.
func TestSender_SendBatchAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			return nil, errors.New("Unauthorized access: SAS token expired")
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	// Buffered >1 so a duplicate report (per-message double-fire) would be
	// observable; the scan reports exactly once per batch.
	reported := make(chan error, 4)
	sender.SetAuthFailureCallback(func(err error) { reported <- err })

	envs := []*messaging.Envelope{
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b1", Payload: []byte("m1")}),
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b2", Payload: []byte("m2")}),
	}
	msgs := make([]ports.OutboundMessage, len(envs))
	for i, e := range envs {
		msgs[i] = ports.OutboundMessage{Envelope: e}
	}

	results, sendErr := sender.SendBatch(context.Background(), msgs)
	if sendErr != nil {
		t.Fatalf("batch dispatch failure is per-message, not whole-batch, got %v", sendErr)
	}
	if len(results) != 2 || !errors.Is(results[0].Err, shared.ErrNotAuthorized) {
		t.Fatalf("expected per-message ErrNotAuthorized, got %+v", results)
	}

	got := requireReport(t, reported)
	if !errors.Is(got, shared.ErrNotAuthorized) {
		t.Fatalf("callback err = %v, want ErrNotAuthorized", got)
	}
	// Exactly once per batch (NotifyAuthFailure is rate-limited anyway).
	requireNoReport(t, reported)
}

// a non-auth SendBatch error must NOT trigger a reactive re-resolve.
func TestSender_SendBatchNonAuthError_DoesNotReport(t *testing.T) {
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			return nil, errors.New("server busy: too many requests")
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "q", Client: mock})
	if err != nil {
		t.Fatal(err)
	}

	reported := make(chan error, 1)
	sender.SetAuthFailureCallback(func(err error) { reported <- err })

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b1", Payload: []byte("m1")})
	_, _ = sender.SendBatch(context.Background(), []ports.OutboundMessage{{Envelope: env}})

	requireNoReport(t, reported)
}

// the injected reactive-recovery callback. Reverting the r.reportAuthFailure(err)
// call in Receiver.pollLoop makes this fail.
func TestReceiver_PollAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	mock := &mockASBClient{
		ReceiveMessagesFn: func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return nil, errors.New("Unauthorized: token revoked")
		},
	}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	reported := make(chan error, 1)
	recv.SetAuthFailureCallback(func(err error) { reported <- err })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = recv.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
		close(done)
	}()

	got := requireReport(t, reported)
	if !errors.Is(got, shared.ErrNotAuthorized) {
		t.Fatalf("callback err = %v, want ErrNotAuthorized", got)
	}
	cancel()
	<-done
}

// a non-auth receive error must NOT trigger a reactive re-resolve.
func TestReceiver_PollNonAuthError_DoesNotReport(t *testing.T) {
	mock := &mockASBClient{
		ReceiveMessagesFn: func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return nil, errors.New("server busy: too many requests")
		},
	}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	reported := make(chan error, 1)
	recv.SetAuthFailureCallback(func(err error) { reported <- err })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = recv.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
		close(done)
	}()

	requireNoReport(t, reported)
	cancel()
	<-done
}
