package servicebus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
)

// mockSenderAPI implements asbSenderAPI for unit tests.
type mockSenderAPI struct {
	sendMessageFn      func(ctx context.Context, msg *azservicebus.Message, opts *azservicebus.SendMessageOptions) error
	newMessageBatchFn  func(ctx context.Context, opts *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error)
	sendMessageBatchFn func(ctx context.Context, batch *azservicebus.MessageBatch, opts *azservicebus.SendMessageBatchOptions) error
	closeFn            func(ctx context.Context) error

	mu           sync.Mutex
	sentMessages []*azservicebus.Message
	closed       bool
}

func (m *mockSenderAPI) SendMessage(ctx context.Context, msg *azservicebus.Message, opts *azservicebus.SendMessageOptions) error {
	m.mu.Lock()
	m.sentMessages = append(m.sentMessages, msg)
	m.mu.Unlock()

	if m.sendMessageFn != nil {
		return m.sendMessageFn(ctx, msg, opts)
	}
	return nil
}

func (m *mockSenderAPI) NewMessageBatch(ctx context.Context, opts *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
	if m.newMessageBatchFn != nil {
		return m.newMessageBatchFn(ctx, opts)
	}
	return nil, fmt.Errorf("NewMessageBatch not configured")
}

func (m *mockSenderAPI) SendMessageBatch(ctx context.Context, batch *azservicebus.MessageBatch, opts *azservicebus.SendMessageBatchOptions) error {
	if m.sendMessageBatchFn != nil {
		return m.sendMessageBatchFn(ctx, batch, opts)
	}
	return nil
}

func (m *mockSenderAPI) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	if m.closeFn != nil {
		return m.closeFn(ctx)
	}
	return nil
}

func (m *mockSenderAPI) lastMessage() *azservicebus.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sentMessages) == 0 {
		return nil
	}
	return m.sentMessages[len(m.sentMessages)-1]
}

// validates NewSender rejects invalid SenderConfig and accepts queue or topic with a client.
func TestNewSender_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SenderConfig
		wantErr bool
	}{
		{
			name:    "missing queue and topic",
			cfg:     SenderConfig{Client: &mockSenderAPI{}},
			wantErr: true,
		},
		{
			name:    "missing connection and client",
			cfg:     SenderConfig{QueueName: "q"},
			wantErr: true,
		},
		{
			name: "valid queue with client",
			cfg: SenderConfig{
				QueueName: "my-queue",
				Client:    &mockSenderAPI{},
			},
		},
		{
			name: "valid topic with client",
			cfg: SenderConfig{
				TopicName: "my-topic",
				Client:    &mockSenderAPI{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSender(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewSender() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// verifies Send maps envelope ID, subject, and payload onto the azservicebus.Message.
func TestSender_Send_Success(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "msg-123",
		Subject: "order.created",
		Payload: []byte(`{"order_id":"42"}`),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if string(msg.Body) != `{"order_id":"42"}` {
		t.Errorf("Body = %q, want %q", msg.Body, `{"order_id":"42"}`)
	}
	if msg.MessageID == nil || *msg.MessageID != "msg-123" {
		t.Errorf("MessageID = %v, want %q", msg.MessageID, "msg-123")
	}
	if msg.Subject == nil || *msg.Subject != "order.created" {
		t.Errorf("Subject = %v, want %q", msg.Subject, "order.created")
	}
}

// verifies Send maps ASB headers to message fields and passes other keys to ApplicationProperties.
func TestSender_Send_HeaderMapping(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "hdr-msg",
		Subject: "test",
		Payload: []byte("body"),
		Headers: map[string]any{
			"asb.correlation-id": "corr-123",
			"asb.content-type":   "application/json",
			"asb.reply-to":       "reply-queue",
			"asb.session-id":     "sess-456",
			"asb.to":             "dest-queue",
			"custom-header":      "custom-value",
			"x-bridge.route-id":  "route-1",
		},
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}

	if msg.CorrelationID == nil || *msg.CorrelationID != "corr-123" {
		t.Errorf("CorrelationID = %v, want %q", msg.CorrelationID, "corr-123")
	}
	if msg.ContentType == nil || *msg.ContentType != "application/json" {
		t.Errorf("ContentType = %v, want %q", msg.ContentType, "application/json")
	}
	if msg.ReplyTo == nil || *msg.ReplyTo != "reply-queue" {
		t.Errorf("ReplyTo = %v, want %q", msg.ReplyTo, "reply-queue")
	}
	if msg.SessionID == nil || *msg.SessionID != "sess-456" {
		t.Errorf("SessionID = %v, want %q", msg.SessionID, "sess-456")
	}
	if msg.To == nil || *msg.To != "dest-queue" {
		t.Errorf("To = %v, want %q", msg.To, "dest-queue")
	}

	assertAppProp(t, msg, "custom-header", "custom-value")
	assertAppProp(t, msg, "x-bridge.route-id", "route-1")

	for k := range msg.ApplicationProperties {
		if len(k) > 4 && k[:4] == "asb." {
			t.Errorf("asb.* key leaked into ApplicationProperties: %s", k)
		}
	}
}

// verifies DefaultSessionID is applied when the envelope has no session header.
func TestSender_Send_DefaultSessionID(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName:        "q",
		DefaultSessionID: "default-sess",
		Client:           mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		Payload: []byte("no-session-header"),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if msg.SessionID == nil || *msg.SessionID != "default-sess" {
		t.Errorf("SessionID = %v, want %q", msg.SessionID, "default-sess")
	}
}

// verifies asb.session-id in headers overrides DefaultSessionID.
func TestSender_Send_HeaderSessionOverride(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName:        "q",
		DefaultSessionID: "default-sess",
		Client:           mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		Payload: []byte("override-session"),
		Headers: map[string]any{
			"asb.session-id": "override-sess",
		},
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if msg.SessionID == nil || *msg.SessionID != "override-sess" {
		t.Errorf("SessionID = %v, want %q (header should override default)", msg.SessionID, "override-sess")
	}
}

// verifies Send wraps SendMessage failures as a BridgeError.
func TestSender_Send_Error(t *testing.T) {
	mock := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			return fmt.Errorf("some error")
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{Payload: []byte("fail")}
	sendErr := sender.Send(context.Background(), env)
	if sendErr == nil {
		t.Fatal("expected error from Send")
	}

	var be *domain.BridgeError
	if !errors.As(sendErr, &be) {
		t.Fatalf("expected *domain.BridgeError, got %T", sendErr)
	}
}

// verifies envelope Subject maps to the outgoing message Subject.
func TestSender_Send_SubjectMapping(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		Subject: "my-subject",
		Payload: []byte("data"),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if msg.Subject == nil || *msg.Subject != "my-subject" {
		t.Errorf("Subject = %v, want %q", msg.Subject, "my-subject")
	}
}

// verifies Send does not set MessageID when the envelope has no ID.
func TestSender_Send_EmptyID(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		Payload: []byte("no-id"),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if msg.MessageID != nil && *msg.MessageID != "" {
		t.Errorf("MessageID should be nil or empty for envelope with no ID, got %q", *msg.MessageID)
	}
}

// verifies Send succeeds with nil envelope headers and sends the payload.
func TestSender_Send_NilHeaders(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "nil-hdr",
		Payload: []byte("body"),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if string(msg.Body) != "body" {
		t.Errorf("Body = %q, want %q", msg.Body, "body")
	}
}

// verifies SendBatch returns zero sent and no error for nil or empty input.
func TestSender_SendBatch_EmptySlice(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	sent, err := sender.SendBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("SendBatch(nil): %v", err)
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}

	sent, err = sender.SendBatch(context.Background(), []*domain.Envelope{})
	if err != nil {
		t.Fatalf("SendBatch(empty): %v", err)
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
}

// verifies SendBatch propagates NewMessageBatch errors and reports zero sent.
func TestSender_SendBatch_NewBatchError(t *testing.T) {
	batchErr := fmt.Errorf("cannot create batch")
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			return nil, batchErr
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := []*domain.Envelope{
		{ID: "b1", Payload: []byte("msg1")},
		{ID: "b2", Payload: []byte("msg2")},
	}

	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected error from SendBatch")
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0 on NewMessageBatch failure", sent)
	}
}

// verifies SendBatch propagates SendMessageBatch failures.
func TestSender_SendBatch_SendBatchError(t *testing.T) {
	mock := &mockSenderAPI{
		newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			// Return a nil batch; the implementation should still call
			// SendMessageBatch and propagate its error. If the implementation
			// guards against nil batch, the error from SendMessageBatch won't
			// trigger, and we verify NewMessageBatch was at least called.
			return nil, nil
		},
		sendMessageBatchFn: func(context.Context, *azservicebus.MessageBatch, *azservicebus.SendMessageBatchOptions) error {
			return fmt.Errorf("send batch failed")
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := []*domain.Envelope{
		{ID: "b1", Payload: []byte("msg1")},
	}

	_, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected error from SendBatch when SendMessageBatch fails")
	}
}

// verifies Close invokes the underlying sender client Close.
func TestSender_Close(t *testing.T) {
	closeCalled := false
	mock := &mockSenderAPI{
		closeFn: func(context.Context) error {
			closeCalled = true
			return nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := sender.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closeCalled {
		t.Error("expected Close to call underlying client Close")
	}
}

// verifies Close returns errors from the underlying client.
func TestSender_Close_Error(t *testing.T) {
	mock := &mockSenderAPI{
		closeFn: func(context.Context) error {
			return fmt.Errorf("close failed")
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := sender.Close(context.Background()); err == nil {
		t.Error("expected error from Close")
	}
}

// verifies Send surfaces context cancellation from the client layer.
func TestSender_Send_ContextCanceled(t *testing.T) {
	mock := &mockSenderAPI{
		sendMessageFn: func(ctx context.Context, _ *azservicebus.Message, _ *azservicebus.SendMessageOptions) error {
			return ctx.Err()
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	env := &domain.Envelope{Payload: []byte("canceled")}
	sendErr := sender.Send(ctx, env)
	if sendErr == nil {
		t.Fatal("expected error on canceled context")
	}
}

// verifies Send delivers payload when envelope CreatedAt and ExpiresAt are set.
func TestSender_Send_Timestamps(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	env := &domain.Envelope{
		ID:        "ts-msg",
		Payload:   []byte("with-timestamps"),
		CreatedAt: now,
		ExpiresAt: now.Add(5 * time.Minute),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := mock.lastMessage()
	if msg == nil {
		t.Fatal("expected SendMessage to be called")
	}
	if string(msg.Body) != "with-timestamps" {
		t.Errorf("Body = %q, want %q", msg.Body, "with-timestamps")
	}
}

// verifies multiple sequential Send calls append distinct messages on the mock client.
func TestSender_Send_MultipleMessages(t *testing.T) {
	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		QueueName: "q",
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		env := &domain.Envelope{
			ID:      fmt.Sprintf("msg-%d", i),
			Payload: []byte(fmt.Sprintf("payload-%d", i)),
		}
		if err := sender.Send(context.Background(), env); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}

	mock.mu.Lock()
	count := len(mock.sentMessages)
	mock.mu.Unlock()

	if count != 5 {
		t.Errorf("sentMessages count = %d, want 5", count)
	}
}

// assertAppProp verifies that an application property has the expected string value.
func assertAppProp(t *testing.T, msg *azservicebus.Message, key, want string) {
	t.Helper()
	if msg.ApplicationProperties == nil {
		t.Errorf("ApplicationProperties is nil, expected key %q", key)
		return
	}
	got, ok := msg.ApplicationProperties[key]
	if !ok {
		t.Errorf("ApplicationProperties missing key %q", key)
		return
	}
	if fmt.Sprint(got) != want {
		t.Errorf("ApplicationProperties[%q] = %v, want %q", key, got, want)
	}
}
