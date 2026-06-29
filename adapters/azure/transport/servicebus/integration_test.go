package servicebus_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	servicebus "github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/asblocal"
)

func TestMain(m *testing.M) {
	flag.Parse()
	asblocal.Configure(asblocal.WithCleanOrphans(true))
	if err := warmupAMQP(); err != nil {
		fmt.Fprintf(os.Stderr, "asb warmup skipped: %v\n", err)
	}
	code := m.Run()
	asblocal.Shutdown()
	os.Exit(code)
}

// warmupAMQP exercises the Service Bus emulator's AMQP layer once
// before any test runs. The emulator's first AMQP roundtrip after
// container startup can exceed 30 s while the broker fully initializes
// after TCP becomes reachable, which would cause the first Send-using
// test to fail with "request timed out: context deadline exceeded".
// We retry a short Send + Receive cycle on TestQueue until one
// succeeds (clearing any leftover warmup message), or the total budget
// is exhausted. Subsequent tests then see a hot AMQP link.
func warmupAMQP() error {
	cs := os.Getenv("ASB_CONNECTION_STRING")
	if cs == "" {
		// Skipping warmup probe without a pre-resolved connection
		// string would require starting containers from TestMain,
		// which the existing helpers do lazily inside tests. Use
		// asblocal.WaitUntilReady-style flow via a transient *testing.T.
		probe := &warmupT{}
		cs = asblocal.ConnectionString(probe)
		if probe.skipped {
			return fmt.Errorf("emulator unavailable: %s", probe.skipReason)
		}
	}

	const (
		totalBudget    = 90 * time.Second
		attemptTimeout = 10 * time.Second
		backoff        = 1 * time.Second
	)

	deadline := time.Now().Add(totalBudget)
	var lastErr error
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		if err := warmupRoundtrip(cs, attemptTimeout); err == nil {
			return nil
		} else {
			lastErr = err
			fmt.Fprintf(os.Stderr, "asb warmup attempt %d failed: %v\n", attempt, err)
		}
		time.Sleep(backoff) // OTHER: external emulator warmup backoff between probes
	}
	if lastErr != nil {
		return fmt.Errorf("asb warmup exhausted budget %s: %w", totalBudget, lastErr)
	}
	return nil
}

func warmupRoundtrip(cs string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sender, err := servicebus.NewSender(servicebus.SenderConfig{
		QueueName:  asblocal.TestQueue,
		Connection: servicebus.ConnectionConfig{ConnectionString: shared.NewSecret(cs)},
		Timeout:    timeout,
	})
	if err != nil {
		return fmt.Errorf("warmup sender: %w", err)
	}
	defer sender.Close(context.Background()) //nolint:errcheck

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      fmt.Sprintf("warmup-%d", time.Now().UnixNano()),
		Subject: "asb-warmup",
		Payload: []byte(`{"warmup":true}`),
	})}); err != nil {
		return fmt.Errorf("warmup send: %w", err)
	}

	recv, err := servicebus.NewReceiver(servicebus.ReceiverConfig{
		QueueName:   asblocal.TestQueue,
		MaxMessages: 1,
		Connection:  servicebus.ConnectionConfig{ConnectionString: shared.NewSecret(cs)},
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		return fmt.Errorf("warmup receiver: %w", err)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), timeout)
	defer drainCancel()
	defer recv.Close(context.Background()) //nolint:errcheck

	runErr := recv.Run(drainCtx, func(ctx context.Context, del ports.Delivery) error {
		_ = del.Ack(ctx)
		drainCancel()
		return nil
	})
	if runErr != nil && drainCtx.Err() == nil {
		return fmt.Errorf("warmup receive: %w", runErr)
	}
	return nil
}

// warmupT is a minimal testing.TB that ConnectionString accepts. It
// captures Skip calls without aborting the warmup goroutine.
type warmupT struct {
	testing.TB
	skipped    bool
	skipReason string
}

func (w *warmupT) Helper()                         {}
func (w *warmupT) Logf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
func (w *warmupT) Skip(args ...any)                { w.skipped = true; w.skipReason = fmt.Sprint(args...) }
func (w *warmupT) Skipf(format string, args ...any) {
	w.skipped = true
	w.skipReason = fmt.Sprintf(format, args...)
}
func (w *warmupT) SkipNow() { w.skipped = true }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newTestSender(t *testing.T, queueName string) *servicebus.Sender {
	t.Helper()
	cs := asblocal.ConnectionString(t)
	s, err := servicebus.NewSender(servicebus.SenderConfig{
		QueueName: queueName,
		Connection: servicebus.ConnectionConfig{
			ConnectionString: shared.NewSecret(cs),
		},
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

func newTestReceiver(t *testing.T, cfg servicebus.ReceiverConfig) *servicebus.Receiver {
	t.Helper()
	cs := asblocal.ConnectionString(t)
	if cfg.Connection.ConnectionString.IsZero() {
		cfg.Connection = servicebus.ConnectionConfig{ConnectionString: shared.NewSecret(cs)}
	}
	r, err := servicebus.NewReceiver(cfg, testLogger())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	return r
}

// collectMessages runs the receiver and collects deliveries until count
// messages are received or the timeout expires. It returns the collected
// deliveries (already acked).
func collectMessages(ctx context.Context, recv *servicebus.Receiver, count int, timeout time.Duration) ([]ports.Delivery, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer recv.Close(context.Background()) //nolint:errcheck

	var mu sync.Mutex
	var deliveries []ports.Delivery

	err := recv.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		mu.Lock()
		deliveries = append(deliveries, del)
		n := len(deliveries)
		mu.Unlock()

		if ackErr := del.Ack(ctx); ackErr != nil {
			return ackErr
		}
		if n >= count {
			cancel()
		}
		return nil
	})

	if err != nil && ctx.Err() != nil {
		err = nil
	}

	return deliveries, err
}

// ---------------------------------------------------------------------------
// Send / Receive
// ---------------------------------------------------------------------------

// validates end-to-end send and receive on a queue with payload, subject, and custom headers.
func TestIntegration_SendReceive(t *testing.T) {
	ctx := context.Background()
	sender := newTestSender(t, asblocal.TestQueue)
	defer sender.Close(ctx) //nolint:errcheck

	payload := []byte(`{"msg":"hello"}`)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      fmt.Sprintf("test-msg-%d", time.Now().UnixNano()),
		Subject: "test-subject",
		Payload: payload,
		Headers: map[string]any{
			"custom-key":       "custom-value",
			"asb.content-type": "application/json",
		},
		CreatedAt: time.Now(),
	})

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := newTestReceiver(t, servicebus.ReceiverConfig{
		QueueName: asblocal.TestQueue,
	})

	deliveries, err := collectMessages(ctx, recv, 1, 30*time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(deliveries) == 0 {
		t.Fatal("expected at least 1 delivery, got 0")
	}

	got := deliveries[0].Envelope()

	if string(got.Payload()) != string(payload) {
		t.Errorf("payload = %q, want %q", got.Payload(), payload)
	}
	if got.Subject() != "test-subject" {
		t.Errorf("subject = %q, want %q", got.Subject(), "test-subject")
	}
	if v, ok := got.Headers()["custom-key"]; !ok || v != "custom-value" {
		t.Errorf("custom-key header = %v, want %q", v, "custom-value")
	}
	if v, ok := got.Headers()["asb.content-type"]; !ok || v != "application/json" {
		t.Errorf("content-type = %v, want %q", v, "application/json")
	}
}

// ---------------------------------------------------------------------------
// Ack / Retry / Extend
// ---------------------------------------------------------------------------

// validates Extend, Retry (abandon), and re-receive of the same message on a queue.
func TestIntegration_AckRetryExtend(t *testing.T) {
	ctx := context.Background()
	sender := newTestSender(t, asblocal.TestQueue)
	defer sender.Close(ctx) //nolint:errcheck

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        fmt.Sprintf("ack-test-%d", time.Now().UnixNano()),
		Subject:   "ack-test",
		Payload:   []byte("ack-retry-extend-test"),
		CreatedAt: time.Now(),
	})

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	t.Run("Retry_then_re-receive", func(t *testing.T) {
		recv := newTestReceiver(t, servicebus.ReceiverConfig{
			QueueName: asblocal.TestQueue,
		})

		recvCtx, recvCancel := context.WithTimeout(ctx, 30*time.Second)
		defer recvCancel()

		var firstDelivery ports.Delivery
		err := recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
			firstDelivery = del
			recvCancel()
			return nil
		})
		if err != nil && recvCtx.Err() != nil {
			err = nil
		}
		if err != nil {
			t.Fatalf("first receive: %v", err)
		}
		if firstDelivery == nil {
			t.Fatal("no message received on first attempt")
		}

		// Extend the lock before abandoning.
		if err := firstDelivery.Extend(ctx, time.Time{}); err != nil {
			t.Fatalf("Extend: %v", err)
		}

		// Abandon (retry) to make the message available again.
		if err := firstDelivery.Retry(ctx, 0, nil); err != nil {
			t.Fatalf("Retry: %v", err)
		}

		// Close the first receiver after all deliveries are settled.
		recv.Close(context.Background()) //nolint:errcheck

		// Re-receive the same message.
		recv2 := newTestReceiver(t, servicebus.ReceiverConfig{
			QueueName: asblocal.TestQueue,
		})
		deliveries, err := collectMessages(ctx, recv2, 1, 30*time.Second)
		if err != nil {
			t.Fatalf("re-receive: %v", err)
		}
		if len(deliveries) == 0 {
			t.Fatal("expected re-delivery after retry, got 0")
		}

		got := deliveries[0].Envelope()
		if string(got.Payload()) != "ack-retry-extend-test" {
			t.Errorf("re-delivered payload = %q", got.Payload())
		}
	})
}

// ---------------------------------------------------------------------------
// Batch Send
// ---------------------------------------------------------------------------

// validates SendBatch against a queue and receiving all batched messages.
func TestIntegration_BatchSend(t *testing.T) {
	ctx := context.Background()
	sender := newTestSender(t, asblocal.TestQueue)
	defer sender.Close(ctx) //nolint:errcheck

	const batchSize = 5
	envs := make([]*messaging.Envelope, batchSize)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:        fmt.Sprintf("batch-%d-%d", i, time.Now().UnixNano()),
			Subject:   "batch-test",
			Payload:   []byte(fmt.Sprintf("batch-message-%d", i)),
			CreatedAt: time.Now(),
		})
	}

	results, err := sender.SendBatch(ctx, func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != batchSize {
		t.Fatalf("sent = %d, want %d", sent, batchSize)
	}

	recv := newTestReceiver(t, servicebus.ReceiverConfig{
		QueueName: asblocal.TestQueue,
	})

	deliveries, err := collectMessages(ctx, recv, batchSize, 60*time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(deliveries) < batchSize {
		t.Fatalf("received %d messages, want %d", len(deliveries), batchSize)
	}

	rxBodies := make(map[string]bool, batchSize)
	for _, d := range deliveries {
		rxBodies[string(d.Envelope().Payload())] = true
	}
	for i := 0; i < batchSize; i++ {
		want := fmt.Sprintf("batch-message-%d", i)
		if !rxBodies[want] {
			t.Errorf("missing payload %q in received messages", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Error Mapping
// ---------------------------------------------------------------------------

// validates send failures map to a domain BridgeError when the queue does not exist.
func TestIntegration_ErrorMapping(t *testing.T) {
	cs := asblocal.ConnectionString(t)

	// Attempt to send to a non-existent queue.
	s, err := servicebus.NewSender(servicebus.SenderConfig{
		QueueName: "nonexistent-queue-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		Connection: servicebus.ConnectionConfig{
			ConnectionString: shared.NewSecret(cs),
		},
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer s.Close(context.Background()) //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sendErr := s.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "error-test",
		Subject:   "error-test",
		Payload:   []byte("error-test"),
		CreatedAt: time.Now(),
	})})

	if sendErr == nil {
		t.Fatal("expected error when sending to non-existent queue")
	}

	var bridgeErr *shared.BridgeError
	if !errors.As(sendErr, &bridgeErr) {
		t.Fatalf("expected *shared.BridgeError, got %T: %v", sendErr, sendErr)
	}

	t.Logf("error class = %s, code = %s, message = %s",
		bridgeErr.Class, bridgeErr.Code, bridgeErr.Message)
}

// ---------------------------------------------------------------------------
// Auto-Extend Lock
// ---------------------------------------------------------------------------

// validates lock auto-renewal allows Ack after holding a message longer than the lock duration.
func TestIntegration_AutoExtend(t *testing.T) {
	ctx := context.Background()
	sender := newTestSender(t, asblocal.TestQueue)
	defer sender.Close(ctx) //nolint:errcheck

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        fmt.Sprintf("autoextend-%d", time.Now().UnixNano()),
		Subject:   "auto-extend-test",
		Payload:   []byte("auto-extend-payload"),
		CreatedAt: time.Now(),
	})

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	autoExtend := true
	recv := newTestReceiver(t, servicebus.ReceiverConfig{
		QueueName:    asblocal.TestQueue,
		LockDuration: 15 * time.Second,
		AutoExtend:   &autoExtend,
	})
	defer recv.Close(context.Background()) //nolint:errcheck

	recvCtx, recvCancel := context.WithTimeout(ctx, 60*time.Second)
	defer recvCancel()

	var ackErr error
	err := recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
		// Hold the message longer than the lock duration. The auto-extend
		// goroutine should renew the lock at 50% of LockDuration (7.5s).
		t.Log("holding message for 20s to test auto-extend...")
		select {
		case <-time.After(20 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}

		ackErr = del.Ack(ctx)
		recvCancel()
		return nil
	})

	if err != nil && recvCtx.Err() != nil {
		err = nil
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ackErr != nil {
		t.Fatalf("Ack after auto-extend: %v (lock may have been lost)", ackErr)
	}
}

// ---------------------------------------------------------------------------
// Topic + Subscription
// ---------------------------------------------------------------------------

// validates topic publish delivery to catch-all and SQL-filtered subscriptions.
func TestIntegration_TopicSubscription(t *testing.T) {
	ctx := context.Background()
	cs := asblocal.ConnectionString(t)

	topicSender, err := servicebus.NewSender(servicebus.SenderConfig{
		TopicName: asblocal.TestTopic,
		Connection: servicebus.ConnectionConfig{
			ConnectionString: shared.NewSecret(cs),
		},
	})
	if err != nil {
		t.Fatalf("NewSender (topic): %v", err)
	}
	defer topicSender.Close(ctx) //nolint:errcheck

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      fmt.Sprintf("topic-msg-%d", time.Now().UnixNano()),
		Subject: "topic-test",
		Payload: []byte("topic-payload"),
		Headers: map[string]any{
			"env": "test",
		},
		CreatedAt: time.Now(),
	})

	if err := topicSender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send to topic: %v", err)
	}

	t.Run("sub-all receives message", func(t *testing.T) {
		recv := newTestReceiver(t, servicebus.ReceiverConfig{
			TopicName:        asblocal.TestTopic,
			SubscriptionName: asblocal.TestSubscriptionAll,
		})

		deliveries, err := collectMessages(ctx, recv, 1, 30*time.Second)
		if err != nil {
			t.Fatalf("Receive sub-all: %v", err)
		}
		if len(deliveries) == 0 {
			t.Fatal("sub-all: expected 1 delivery, got 0")
		}
		got := deliveries[0].Envelope()
		if string(got.Payload()) != "topic-payload" {
			t.Errorf("payload = %q, want %q", got.Payload(), "topic-payload")
		}
	})

	t.Run("sub-filtered receives matching message", func(t *testing.T) {
		recv := newTestReceiver(t, servicebus.ReceiverConfig{
			TopicName:        asblocal.TestTopic,
			SubscriptionName: asblocal.TestSubscriptionFilt,
		})

		deliveries, err := collectMessages(ctx, recv, 1, 30*time.Second)
		if err != nil {
			t.Fatalf("Receive sub-filtered: %v", err)
		}
		if len(deliveries) == 0 {
			t.Fatal("sub-filtered: expected 1 delivery, got 0")
		}
		got := deliveries[0].Envelope()
		if string(got.Payload()) != "topic-payload" {
			t.Errorf("payload = %q, want %q", got.Payload(), "topic-payload")
		}
	})
}

// batchSent counts the successful (nil-Err) entries in a SendBatch result.
func batchSent(results []ports.BatchResult) int {
	n := 0
	for _, r := range results {
		if r.Err == nil {
			n++
		}
	}
	return n
}
