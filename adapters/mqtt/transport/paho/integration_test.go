package paho_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

func TestMain(m *testing.M) {
	code := m.Run()
	mqttlocal.Shutdown()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Session lifecycle tests
// ---------------------------------------------------------------------------

// TestIntegration_SessionStartAndClose validates that a session can connect
// to Mosquitto and disconnect cleanly.
func TestIntegration_SessionStartAndClose(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx := context.Background()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("lifecycle"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h := sess.Health(ctx)
	if !h.Connected {
		t.Error("session should be connected")
	}

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestIntegration_SessionCloseIdempotent validates that Close can be
// called multiple times without error.
func TestIntegration_SessionCloseIdempotent(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx := context.Background()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("close-idem"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
}

// TestIntegration_SessionEvents validates that a SessionConnected event
// is emitted after successful connection.
func TestIntegration_SessionEvents(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx := context.Background()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("events"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(ctx)

	select {
	case ev := <-sess.Events():
		if ev.Type != ports.SessionConnected {
			t.Errorf("event type = %v, want SessionConnected", ev.Type)
		}
	case <-time.After(3 * time.Second):
		t.Error("timed out waiting for SessionConnected event")
	}
}

// TestIntegration_SessionReconcile validates that Reconcile subscribes
// and unsubscribes topics correctly, including updates.
func TestIntegration_SessionReconcile(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx := context.Background()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("reconcile"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(ctx)

	// Drain the connected event
	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "test/a", QoS: 1},
			{Topic: "test/b", QoS: 0},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Update: remove test/a, add test/c, change test/b QoS
	plan2 := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "test/b", QoS: 1},
			{Topic: "test/c", QoS: 1},
		},
	}
	if err := sess.Reconcile(ctx, plan2); err != nil {
		t.Fatalf("Reconcile (update): %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pub/Sub round-trip tests
// ---------------------------------------------------------------------------

// TestIntegration_PubSubRoundTrip validates end-to-end publish/subscribe
// through Session, Receiver, and Sender with header round-tripping.
func TestIntegration_PubSubRoundTrip(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("pubsub"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(ctx)

	// Drain connected event
	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	if err := sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "roundtrip/test", QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	recv := paho.NewReceiver("rx1", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var received []*domain.Envelope
	var mu sync.Mutex

	recvCtx, recvCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			received = append(received, del.Envelope())
			mu.Unlock()
			return nil
		})
	}()

	env := &domain.Envelope{
		Subject: "roundtrip/test",
		Payload: []byte("hello-roundtrip"),
		Headers: map[string]any{
			domain.HeaderCorrelationID: "test-corr",
			domain.HeaderContentType:   "text/plain",
			"custom-key":               "custom-val",
		},
	}

	if err := sender.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for message")
		case <-time.After(50 * time.Millisecond):
		}
	}

	recvCancel()
	wg.Wait()

	mu.Lock()
	msg := received[0]
	mu.Unlock()

	if msg.Subject != "roundtrip/test" {
		t.Errorf("subject = %q, want %q", msg.Subject, "roundtrip/test")
	}
	if string(msg.Payload) != "hello-roundtrip" {
		t.Errorf("payload = %q, want %q", msg.Payload, "hello-roundtrip")
	}
	if v, _ := domain.GetHeaderString(msg.Headers, domain.HeaderCorrelationID); v != "test-corr" {
		t.Errorf("correlation = %q, want %q", v, "test-corr")
	}
	if v, _ := domain.GetHeaderString(msg.Headers, domain.HeaderContentType); v != "text/plain" {
		t.Errorf("content-type = %q, want %q", v, "text/plain")
	}
	if v, _ := domain.GetHeaderString(msg.Headers, "custom-key"); v != "custom-val" {
		t.Errorf("custom-key = %q, want %q", v, "custom-val")
	}
}

// ---------------------------------------------------------------------------
// Backpressure test
// ---------------------------------------------------------------------------

// TestIntegration_BackpressureNoDrops validates that when the emit
// callback is slow, messages are buffered (backpressure) rather than
// dropped.
func TestIntegration_BackpressureNoDrops(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("backpressure"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(ctx)

	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	if err := sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "bp/test", QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	recv := paho.NewReceiver("rx-bp", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	const msgCount = 20
	var receivedCount atomic.Int64

	recvCtx, recvCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			time.Sleep(10 * time.Millisecond)
			receivedCount.Add(1)
			return nil
		})
	}()

	for i := 0; i < msgCount; i++ {
		env := &domain.Envelope{
			Subject: "bp/test",
			Payload: []byte(fmt.Sprintf("msg-%d", i)),
		}
		if err := sender.Send(ctx, env); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	deadline := time.After(15 * time.Second)
	for {
		n := receivedCount.Load()
		if n >= msgCount {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: received %d of %d messages", n, msgCount)
		case <-time.After(100 * time.Millisecond):
		}
	}

	recvCancel()
	wg.Wait()

	got := receivedCount.Load()
	if got != msgCount {
		t.Errorf("received %d messages, want %d (messages were dropped!)", got, msgCount)
	}
}

// ---------------------------------------------------------------------------
// QoS completion test
// ---------------------------------------------------------------------------

// TestIntegration_QoS1Completion validates that QoS 1 Send blocks until
// PUBACK (broker confirmation).
func TestIntegration_QoS1Completion(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("qos1"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(ctx)

	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	sender := paho.NewSender(sess, paho.SenderOptions{
		DefaultTopic: "qos1/test",
		QoS:          1,
		Timeout:      5 * time.Second,
	})

	if err := sender.Send(ctx, &domain.Envelope{
		Payload: []byte("qos1-message"),
	}); err != nil {
		t.Fatalf("QoS 1 Send: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Factory tests
// ---------------------------------------------------------------------------

// TestIntegration_Factory validates that the Factory correctly creates
// Session, Receiver, and Sender from port specs.
func TestIntegration_Factory(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx := context.Background()

	factory := &paho.Factory{}

	sess, err := factory.NewSession(ctx, ports.SessionSpec{
		ID:          "factory-sess",
		Transport:   "mqtt",
		SessionMode: domain.SessionEphemeral,
		Options: map[string]any{
			"broker_urls": []string{url},
			"client_id":   mqttlocal.UniqueClientID("factory"),
			"clean_start": true,
		},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(ctx)

	recv, err := factory.NewReceiver(ctx, ports.ReceiverSpec{
		ID:        "factory-rx",
		SessionID: "factory-sess",
	}, sess)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	_ = recv

	send, err := factory.NewSender(ctx, ports.SenderSpec{
		ID:        "factory-tx",
		SessionID: "factory-sess",
		Options: map[string]any{
			"qos":           1,
			"default_topic": "factory/test",
		},
	}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	_ = send
}
