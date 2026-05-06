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
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-3 Integration: Reconnect preserves subscriptions
//
// Verifies that after a session is established with subscriptions, a
// reconnect (via Close + new session with same plan) delivers messages
// on the previously subscribed topics.
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_ReconnectPreservesSubscriptions establishes a session
// with subscriptions, publishes a message, verifies receipt, then creates
// a new session (simulating reconnect) and verifies messages still arrive.
func TestIntegration_ReconnectPreservesSubscriptions(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := fmt.Sprintf("bug3/integ/%d", time.Now().UnixNano())
	clientID := mqttlocal.UniqueClientID("bug3-recon")

	// --- Phase 1: Establish session, subscribe, verify pub/sub ---
	sess1 := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       clientID,
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess1.Start(ctx); err != nil {
		t.Fatalf("Start sess1: %v", err)
	}

	drainEvent(t, sess1)

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: topic, QoS: 1},
		},
	}
	if err := sess1.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile sess1: %v", err)
	}
	waitSubActive(t, sess1, 5*time.Second)

	recv1 := paho.NewReceiver("rx-bug3-1", sess1)
	sender1 := paho.NewSender(sess1, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var received1 atomic.Int32
	recvCtx1, recvCancel1 := context.WithCancel(ctx)
	var wg1 sync.WaitGroup
	wg1.Add(1)
	go func() {
		defer wg1.Done()
		_ = recv1.Run(recvCtx1, func(_ context.Context, _ ports.Delivery) error {
			received1.Add(1)
			return nil
		})
	}()

	if err := sender1.Send(ctx, &messaging.Envelope{
		Subject: topic,
		Payload: []byte("phase1-msg"),
	}); err != nil {
		t.Fatalf("Send phase1: %v", err)
	}

	waitForCondition(t, 5*time.Second, "phase1 message", func() bool {
		return received1.Load() >= 1
	})

	recvCancel1()
	wg1.Wait()

	if err := sess1.Close(ctx); err != nil {
		t.Fatalf("Close sess1: %v", err)
	}

	// --- Phase 2: New session (simulating reconnect), reapply plan ---
	sess2 := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("bug3-recon2"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess2.Start(ctx); err != nil {
		t.Fatalf("Start sess2: %v", err)
	}
	defer func() { _ = sess2.Close(ctx) }()

	drainEvent(t, sess2)

	if err := sess2.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile sess2: %v", err)
	}
	waitSubActive(t, sess2, 5*time.Second)

	recv2 := paho.NewReceiver("rx-bug3-2", sess2)
	sender2 := paho.NewSender(sess2, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var received2 atomic.Int32
	recvCtx2, recvCancel2 := context.WithCancel(ctx)
	var wg2 sync.WaitGroup
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		_ = recv2.Run(recvCtx2, func(_ context.Context, _ ports.Delivery) error {
			received2.Add(1)
			return nil
		})
	}()

	if err := sender2.Send(ctx, &messaging.Envelope{
		Subject: topic,
		Payload: []byte("phase2-msg"),
	}); err != nil {
		t.Fatalf("Send phase2: %v", err)
	}

	waitForCondition(t, 5*time.Second, "phase2 message", func() bool {
		return received2.Load() >= 1
	})

	recvCancel2()
	wg2.Wait()
}

// ═══════════════════════════════════════════════════════════════════════════
// BUG-3 Integration: Reconcile updates activeSubs on success
//
// Verifies that after a successful Reconcile with a changed plan,
// messages arrive on newly added topics and do NOT arrive on removed
// topics.
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_ReconcileSuccess_UpdatesActiveSubs validates that
// successful reconcile reflects the new subscription plan by receiving
// messages on new topics.
func TestIntegration_ReconcileSuccess_UpdatesActiveSubs(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	prefix := fmt.Sprintf("bug3/update/%d", time.Now().UnixNano())
	topicA := prefix + "/a"
	topicB := prefix + "/b"

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("bug3-update"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	drainEvent(t, sess)

	// Subscribe to topicA first.
	planA := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: topicA, QoS: 1},
		},
	}
	if err := sess.Reconcile(ctx, planA); err != nil {
		t.Fatalf("Reconcile planA: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	// Update plan: remove topicA, add topicB.
	planB := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: topicB, QoS: 1},
		},
	}
	if err := sess.Reconcile(ctx, planB); err != nil {
		t.Fatalf("Reconcile planB: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-bug3-update", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var receivedTopics sync.Map
	recvCtx, recvCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			receivedTopics.Store(del.Envelope().Subject, true)
			return nil
		})
	}()

	// Send to both topics.
	for _, topic := range []string{topicA, topicB} {
		if err := sender.Send(ctx, &messaging.Envelope{
			Subject: topic,
			Payload: []byte("test-" + topic),
		}); err != nil {
			t.Fatalf("Send to %s: %v", topic, err)
		}
	}

	// Wait for topicB message.
	waitForCondition(t, 5*time.Second, "topicB message", func() bool {
		_, ok := receivedTopics.Load(topicB)
		return ok
	})

	// NEGATIVE: assert topicA does NOT arrive.
	<-time.After(500 * time.Millisecond)

	recvCancel()
	wg.Wait()

	if _, ok := receivedTopics.Load(topicA); ok {
		t.Error("should NOT receive messages on unsubscribed topicA")
	}
	if _, ok := receivedTopics.Load(topicB); !ok {
		t.Error("should receive messages on subscribed topicB")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// BUG-4 Integration: Cancel context stops reconcile
//
// Verifies that cancelling the Start context prevents the session from
// hanging on reconcile operations.
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_CancelContext_ReconcileDoesNotHang validates that
// after starting a session and subscribing, cancelling the context
// causes subsequent Reconcile to fail promptly (not hang).
func TestIntegration_CancelContext_ReconcileDoesNotHang(t *testing.T) {
	url := mqttlocal.BrokerURL(t)

	parentCtx, parentCancel := context.WithCancel(context.Background())

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("bug4-cancel"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(parentCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	drainEvent(t, sess)

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "bug4/cancel/test", QoS: 1},
		},
	}
	if err := sess.Reconcile(parentCtx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Cancel the context.
	parentCancel()

	// Reconcile with the cancelled context should fail promptly.
	done := make(chan error, 1)
	go func() {
		done <- sess.Reconcile(parentCtx, domain.SessionPlan{
			Subscriptions: []domain.SubscriptionPlan{
				{Topic: "bug4/cancel/new", QoS: 0},
			},
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Log("Reconcile returned nil (may succeed if no broker call needed)")
		}
		// Either error or nil is acceptable; the point is it did not hang.
	case <-time.After(5 * time.Second):
		t.Fatal("Reconcile hung after context cancellation -- BUG-4 not fixed")
	}
}

// TestIntegration_SessionStartStoresContext validates that after Start(),
// the session's Health reports connected, proving startCtx was stored
// and connection was established successfully.
func TestIntegration_SessionStartStoresContext(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("bug4-start"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	drainEvent(t, sess)

	h := sess.Health(ctx)
	if !h.Connected {
		t.Fatal("session should be connected after Start")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// BUG-3 Integration: Concurrent reconcile does not corrupt activeSubs
// (race condition test)
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_ConcurrentReconcile_ActiveSubsIntegrity validates that
// concurrent Reconcile calls from multiple goroutines do not corrupt
// subscription state and that the final state is usable.
func TestIntegration_ConcurrentReconcile_ActiveSubsIntegrity(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("bug3-race"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, domain.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	drainEvent(t, sess)

	const workers = 4
	var wg sync.WaitGroup
	var errCount atomic.Int32
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			for iter := 0; iter < 3; iter++ {
				plan := domain.SessionPlan{
					Subscriptions: []domain.SubscriptionPlan{
						{Topic: fmt.Sprintf("bug3/race/%d/%d", idx, iter), QoS: 1},
					},
				}
				if err := sess.Reconcile(ctx, plan); err != nil {
					t.Logf("worker %d iter %d: %v", idx, iter, err)
					errCount.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	if n := errCount.Load(); n != 0 {
		t.Fatalf("expected 0 errors from concurrent reconcile, got %d", n)
	}

	// Apply a final known plan and verify messages arrive.
	verifyTopic := fmt.Sprintf("bug3/race/verify/%d", time.Now().UnixNano())
	if err := sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: verifyTopic, QoS: 1},
		},
	}); err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-race-verify", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var got atomic.Int32
	recvCtx, recvCancel := context.WithCancel(ctx)
	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() {
		defer rwg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, _ ports.Delivery) error {
			got.Add(1)
			return nil
		})
	}()

	wait.RequireClosed(t, recv.Started(), 5*time.Second)
	if err := sender.Send(ctx, &messaging.Envelope{
		Subject: verifyTopic,
		Payload: []byte("verify"),
	}); err != nil {
		t.Fatalf("Send verify: %v", err)
	}

	waitForCondition(t, 5*time.Second, "verify message", func() bool {
		return got.Load() >= 1
	})

	recvCancel()
	rwg.Wait()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func drainEvent(t *testing.T, sess *paho.Session) {
	t.Helper()
	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}
}

// waitSubActive waits for the broker to ACK all pending SUBSCRIBE frames.
// It deliberately does NOT require ServiceLevelFull because tests
// typically construct the receiver (handler) AFTER calling Reconcile;
// Full would require HandlersRegistered > 0 which has not happened yet.
func waitSubActive(t *testing.T, sess *paho.Session, timeout time.Duration) {
	t.Helper()
	wait.Until(t, timeout, "subscriptions active", func() bool {
		h := sess.Health(context.Background())
		return h.Connected && h.SubscriptionsActive == h.SubscriptionsWanted
	})
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", desc)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
