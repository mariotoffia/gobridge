package paho_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// RES-MQTT-CONCSC: Concurrent Sender.Send and Session.Close
//
// The Sender captures a snapshot of the ConnectionManager pointer at the
// start of Send. If Close runs concurrently and tears the CM down (sets
// s.cm = nil and cancels cmCtx), the in-flight Send must:
//   - Not panic
//   - Return a classified BridgeError
//   - Complete in bounded time (no orphan goroutine leak)
//
// Failure modes this test guards against:
//   - panic on send-on-closed-channel (paho internal queue)
//   - hung Send (waits forever for PUBACK from disconnected cm)
//   - data race on s.cm (without snapshot under lock)
// ═══════════════════════════════════════════════════════════════════════════

// TestRes_ConcurrentSendAndClose_NoPanicOrHang publishes a stream of QoS 1
// messages from N goroutines while a coordinator goroutine closes the
// session mid-stream. All Send calls must complete, none may panic, and
// the test must terminate well before the test timeout.
func TestRes_ConcurrentSendAndClose_NoPanicOrHang(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("res-send-close"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainEvents(sess, 1, 3*time.Second)

	sender := paho.NewSender(sess, paho.SenderOptions{
		QoS:          1,
		Timeout:      2 * time.Second,
		DefaultTopic: "res/send-close",
	})

	const senders = 8
	const perSender = 50

	var (
		wg       sync.WaitGroup
		sendErrs atomic.Int64
		sendOK   atomic.Int64
		panics   atomic.Int64
	)

	wg.Add(senders)
	for s := 0; s < senders; s++ {
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
					t.Errorf("sender %d panicked: %v", id, r)
				}
			}()
			for i := 0; i < perSender; i++ {
				err := sender.Send(ctx, &domain.Envelope{
					Subject: "res/send-close",
					Payload: []byte(fmt.Sprintf("s%d-i%d", id, i)),
				})
				if err != nil {
					sendErrs.Add(1)
				} else {
					sendOK.Add(1)
				}
			}
		}(s)
	}

	// Trigger Close partway through. Use a short delay so we hit the
	// race window where Sends are mid-flight.
	go func() {
		time.Sleep(50 * time.Millisecond)
		closeCtx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = sess.Close(closeCtx)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("senders did not complete within timeout — Send is hung " +
			"after concurrent Close")
	}

	if got := panics.Load(); got != 0 {
		t.Fatalf("got %d panics from sender goroutines (must be 0)", got)
	}
	t.Logf("send results: ok=%d err=%d", sendOK.Load(), sendErrs.Load())
}

// TestRes_SendAfterClose_ReturnsErrorNoPanic verifies that calling Send
// AFTER Close returns a typed error (ErrUnavailable) and does not panic.
func TestRes_SendAfterClose_ReturnsErrorNoPanic(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("res-send-after-close"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainEvents(sess, 1, 3*time.Second)

	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 2 * time.Second})

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send after Close panicked: %v", r)
		}
	}()

	err := sender.Send(ctx, &domain.Envelope{
		Subject: "res/post-close",
		Payload: []byte("after-close"),
	})
	if err == nil {
		t.Fatal("Send after Close must return an error")
	}
	be, ok := err.(*domain.BridgeError)
	if !ok {
		t.Fatalf("Send after Close should return *domain.BridgeError, got %T: %v", err, err)
	}
	if be.Code != domain.ErrUnavailable.Code {
		t.Errorf("Send after Close: code=%s, want %s",
			be.Code, domain.ErrUnavailable.Code)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// RES-MQTT-CONCRC: Concurrent Reconcile and Close
//
// Reconcile takes a snapshot of cm under mu, then performs broker IO
// outside the mutex. Close concurrently nils s.cm and disconnects.
// In-flight Reconcile must complete (with or without error) — never hang
// past Close + cmCancel.
// ═══════════════════════════════════════════════════════════════════════════

// TestRes_ConcurrentReconcileAndClose_NoHang launches N reconcile loops
// against a real broker, then triggers Close. Every reconcile call must
// return within a bounded time after Close.
func TestRes_ConcurrentReconcileAndClose_NoHang(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("res-recon-close"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainEvents(sess, 1, 3*time.Second)

	const workers = 4
	const iters = 30

	var (
		wg     sync.WaitGroup
		panics atomic.Int64
	)

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
					t.Errorf("reconcile worker %d panicked: %v", id, r)
				}
			}()
			for i := 0; i < iters; i++ {
				_ = sess.Reconcile(ctx, domain.SessionPlan{
					Subscriptions: []domain.SubscriptionPlan{
						{Topic: fmt.Sprintf("res/recon/%d/%d", id, i), QoS: 1},
					},
				})
			}
		}(w)
	}

	go func() {
		time.Sleep(40 * time.Millisecond)
		closeCtx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = sess.Close(closeCtx)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("reconcile workers did not complete within timeout — " +
			"Reconcile is hung after concurrent Close")
	}

	if got := panics.Load(); got != 0 {
		t.Fatalf("got %d panics in reconcile workers (must be 0)", got)
	}
}

// TestRes_ReconcileAfterClose_NoPanicReturnsError verifies that
// Reconcile invoked on a closed session returns ErrUnavailable promptly
// without panic.
func TestRes_ReconcileAfterClose_NoPanicReturnsError(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("res-recon-after-close"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainEvents(sess, 1, 3*time.Second)

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Reconcile after Close panicked: %v", r)
		}
	}()

	err := sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "res/post-close", QoS: 1}},
	})
	if err == nil {
		t.Fatal("Reconcile after Close must return an error")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// RES-MQTT-OUTAGE: Broker outage and recovery
//
// Stops the broker mid-flight, restarts it, and verifies that the session
// re-establishes connectivity, re-subscribes to its plan via OnConnectionUp,
// and resumes message delivery. This exercises:
//   - autopaho reconnection loop (KeepAlive timeout / disconnect)
//   - SessionDisconnected and SessionConnected event emission
//   - OnConnectionUp's reconcile that re-applies activeSubs
// ═══════════════════════════════════════════════════════════════════════════

// TestRes_BrokerOutage_ReconnectResubscribesAndDelivers stops the broker,
// restarts it, then publishes again to verify the subscription survived
// the outage.
func TestRes_BrokerOutage_ReconnectResubscribesAndDelivers(t *testing.T) {
	if testing.Short() {
		t.Skip("broker restart test")
	}

	broker := mqttlocal.NewBrokerInstance(t)
	url := broker.URL()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	topic := fmt.Sprintf("res/outage/%d", time.Now().UnixNano())

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{url},
		ClientID:         mqttlocal.UniqueClientID("res-outage"),
		KeepAlive:        2,
		ConnectTimeout:   10 * time.Second,
		ReconnectDelay:   500 * time.Millisecond,
		ReconnectTimeout: 10 * time.Second,
		CleanStart:       true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close(context.Background())

	drainEvents(sess, 1, 3*time.Second)

	if err := sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Wait for the SessionReconciled event so we know the subscription
	// is in place before we exercise the outage.
	waitForEventType(t, sess, ports.SessionReconciled, 5*time.Second)

	recv := paho.NewReceiver("res-outage-rx", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var received atomic.Int64
	recvCtx, recvCancel := context.WithCancel(ctx)
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, _ ports.Delivery) error {
			received.Add(1)
			return nil
		})
	}()
	defer func() { recvCancel(); rwg.Wait() }()

	// Phase 1: send + receive against live broker.
	if err := sender.Send(ctx, &domain.Envelope{
		Subject: topic,
		Payload: []byte("phase-1"),
	}); err != nil {
		t.Fatalf("Send phase 1: %v", err)
	}
	waitForCount(t, &received, 1, 5*time.Second, "phase 1 message")

	// Phase 2: kill broker, wait for SessionDisconnected.
	broker.Stop()
	waitForEventType(t, sess, ports.SessionDisconnected, 15*time.Second)

	// Phase 3: bring broker back; expect SessionConnected then SessionReconciled.
	broker.Restart()
	waitForEventType(t, sess, ports.SessionConnected, 30*time.Second)
	waitForEventType(t, sess, ports.SessionReconciled, 15*time.Second)

	// Allow paho a moment to settle the new subscription.
	time.Sleep(300 * time.Millisecond)

	// Phase 4: subscription must have been restored — sending again
	// should be received.
	if err := sender.Send(ctx, &domain.Envelope{
		Subject: topic,
		Payload: []byte("phase-2-after-recovery"),
	}); err != nil {
		t.Fatalf("Send phase 2 after recovery: %v", err)
	}
	waitForCount(t, &received, 2, 10*time.Second, "phase 2 message after recovery")
}

// ---------------------------------------------------------------------------
// helpers (kept package-local to avoid colliding with other test helpers)
// ---------------------------------------------------------------------------

func drainEvents(sess *paho.Session, n int, timeout time.Duration) {
	deadline := time.After(timeout)
	for i := 0; i < n; i++ {
		select {
		case <-sess.Events():
		case <-deadline:
			return
		}
	}
}

func waitForEventType(t *testing.T, sess *paho.Session, want ports.SessionEventType, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("events channel closed while waiting for %v", want)
			}
			if ev.Type == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %v", want)
		}
	}
}

func waitForCount(t *testing.T, c *atomic.Int64, want int64, timeout time.Duration, desc string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if c.Load() >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s (got %d, want %d)", desc, c.Load(), want)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
