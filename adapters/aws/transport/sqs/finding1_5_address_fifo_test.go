package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Finding 1 — queue-name binding address.
//
// Scenario configs bind `address: <queue-name>` while the sender resolves
// a fully-qualified queue URL. The sender previously rejected any non-empty
// address that did not equal the queue URL, so those scenarios failed. The
// in-adapter fix accepts an address that refers to the bound queue,
// including the queue NAME embedded as the URL's last path segment.

func TestSend_AddressEqualsQueueName_Accepted(t *testing.T) {
	mock := &mockSQSClient{}
	s, err := NewSender(SenderConfig{
		// Scenario 02 shape: full URL configured, name used as address.
		QueueURL: "https://sqs.us-west-1.amazonaws.com/123456789/processing-events",
		Client:   mock,
	})
	require.NoError(t, err)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("{}")})
	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: "processing-events"})
	require.NoError(t, err, "binding address equal to the queue name must be accepted")

	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Len(t, mock.SendCalls, 1)
}

func TestSend_AddressMismatch_RejectedWithoutSDK(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageFn: func(context.Context, *awssqs.SendMessageInput, ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			t.Fatal("SendMessage must not be called on address mismatch")
			return nil, nil
		},
	}
	s, err := NewSender(SenderConfig{
		QueueURL: "https://sqs.us-west-1.amazonaws.com/123456789/processing-events",
		Client:   mock,
	})
	require.NoError(t, err)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("{}")})
	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: "some-other-queue"})
	require.Error(t, err)
	require.True(t, errors.Is(err, shared.ErrInvalidTopic), "want ErrInvalidTopic, got %v", err)
}

// Finding 5 — FIFO per-MessageGroupId ordering.
//
// The route runner dispatches deliveries concurrently, so a ReceiveMessage
// that returned several messages of one group could reorder them. Forcing
// MaxMessages=1 makes SQS hand back at most one message per group (a FIFO
// group is locked to its in-flight message until that message is deleted),
// preserving per-group order with no shared-runner change. Detected from
// the `.fifo` suffix at applyDefaults and re-checked against the resolved
// URL in Run.

func TestReceiverConfig_FIFO_ForcesMaxMessages1(t *testing.T) {
	cases := []struct {
		name string
		cfg  ReceiverConfig
	}{
		{"fifo_url", ReceiverConfig{QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders.fifo", MaxMessages: 10}},
		{"fifo_name", ReceiverConfig{QueueName: "orders.fifo", MaxMessages: 5}},
		{"fifo_default_maxmessages", ReceiverConfig{QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders.fifo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.applyDefaults()
			assert.Equal(t, int32(1), cfg.MaxMessages, "FIFO source must clamp MaxMessages to 1")
		})
	}
}

func TestReceiverConfig_Standard_KeepsMaxMessages(t *testing.T) {
	cfg := ReceiverConfig{QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders", MaxMessages: 7}
	cfg.applyDefaults()
	assert.Equal(t, int32(7), cfg.MaxMessages)
}

func TestIsFIFOQueue(t *testing.T) {
	assert.True(t, isFIFOQueue("orders.fifo"))
	assert.True(t, isFIFOQueue("https://sqs.us-east-1.amazonaws.com/123/orders.fifo"))
	assert.False(t, isFIFOQueue("orders"))
	assert.False(t, isFIFOQueue("https://sqs.us-east-1.amazonaws.com/123/orders"))
	assert.False(t, isFIFOQueue(""))
}

// TestReceiver_Run_FIFO_RequestsSingleMessage asserts the end-to-end
// effect: a FIFO receiver issues ReceiveMessage with
// MaxNumberOfMessages=1, which is what actually preserves ordering at the
// SQS API boundary.
func TestReceiver_Run_FIFO_RequestsSingleMessage(t *testing.T) {
	var gotMax int32 = -1
	mock := &mockSQSClient{
		ReceiveMessageFn: func(ctx context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			gotMax = in.MaxNumberOfMessages
			<-ctx.Done() // block until the test cancels
			return nil, ctx.Err()
		},
	}
	noAuto := false
	r, err := NewReceiver(ReceiverConfig{
		QueueURL:   "https://sqs.us-east-1.amazonaws.com/123/orders.fifo",
		AutoExtend: &noAuto,
		Client:     mock,
	}, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	// Wait until the poll loop is live (Started signals readiness), then
	// cancel; no sleeps.
	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("receiver did not start")
	}
	cancel()
	<-done

	assert.Equal(t, int32(1), gotMax, "FIFO receiver must request MaxNumberOfMessages=1")
	assert.Equal(t, int32(1), r.cfg.MaxMessages)
}

// Finding 1 (build-time) — ValidateAddress mirrors the send-time queue
// match so a misconfigured static binding address fails when the bridge is
// built, not at first send. It is deliberately more lenient than the
// send-time check for QueueName-only senders, whose URL is resolved lazily
// (s.queueURL empty at build time): a full URL ending in the configured
// name is accepted so it is not falsely rejected before resolution.
func TestSender_ValidateAddress(t *testing.T) {
	const url = "https://sqs.us-west-1.amazonaws.com/123456789/orders"

	cases := []struct {
		name    string
		cfg     SenderConfig
		address string
		wantErr bool
	}{
		{"url-cfg/empty-address", SenderConfig{QueueURL: url}, "", false},
		{"url-cfg/name-address", SenderConfig{QueueURL: url}, "orders", false},
		{"url-cfg/url-address", SenderConfig{QueueURL: url}, url, false},
		{"url-cfg/wrong-address", SenderConfig{QueueURL: url}, "other-queue", true},
		{"name-cfg/name-address", SenderConfig{QueueName: "orders"}, "orders", false},
		{"name-cfg/full-url-ending-in-name", SenderConfig{QueueName: "orders"}, url, false},
		{"name-cfg/wrong-full-url", SenderConfig{QueueName: "orders"}, "https://sqs.us-west-1.amazonaws.com/123456789/wrong", true},
		{"name-cfg/wrong-name", SenderConfig{QueueName: "orders"}, "payments", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Client = &mockSQSClient{}
			s, err := NewSender(tc.cfg)
			require.NoError(t, err)

			err = s.ValidateAddress(tc.address)
			if tc.wantErr {
				require.Error(t, err)
				require.True(t, errors.Is(err, shared.ErrInvalidTopic), "want ErrInvalidTopic, got %v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
