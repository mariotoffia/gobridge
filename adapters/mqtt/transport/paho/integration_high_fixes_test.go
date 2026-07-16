package paho_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// T4 Integration: Concurrent reconcile serialization
//
// Verifies that concurrent Reconcile calls on a live broker are
// serialized by reloadGate, preventing interleaved subscribe/unsubscribe
// operations from corrupting activeSubs state.
//
//   Goroutine A ──▶ Reconcile(planA) ──▶ reloadGate.Lock ──▶ sub/unsub ──▶ unlock
//   Goroutine B ──▶ Reconcile(planB) ──▶ reloadGate.Lock ──▶ (waits) ──▶ sub/unsub
// ═══════════════════════════════════════════════════════════════════════════

// TestIntegration_ConcurrentReconcile_NoCorruption validates that
// concurrent Reconcile calls against a real broker do not produce errors
// or corrupt subscription state.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	5 goroutines each call Reconcile with different topic sets
//	All must succeed without errors
//	Final Reconcile with known plan must succeed
//
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - 5 concurrent reconcilers
//   - Each with a unique topic set
//
// Assertions:
//   - No errors from any Reconcile call
//   - Final Reconcile with definitive plan succeeds
//   - No panics or race conditions
func TestIntegration_ConcurrentReconcile_NoCorruption(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("conc-reconcile"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
	}

	const goroutines = 5
	var wg sync.WaitGroup
	var errCount atomic.Int32

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			for iter := 0; iter < 3; iter++ {
				plan := connectivity.SessionPlan{
					Subscriptions: []connectivity.SubscriptionPlan{
						{Topic: "conc/shared", QoS: 1},
						{Topic: topicForGoroutine(idx, iter), QoS: 0},
					},
				}
				if err := sess.Reconcile(ctx, plan); err != nil {
					t.Logf("goroutine %d iter %d: Reconcile error: %v", idx, iter, err)
					errCount.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	if n := errCount.Load(); n != 0 {
		t.Fatalf("expected 0 reconcile errors, got %d", n)
	}

	// Apply a definitive final plan and verify subscription state by
	// publishing a message to the subscribed topic and confirming receipt.
	verifyTopic := "conc/verify/" + mqttlocal.UniqueClientID("v")
	finalPlan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: verifyTopic, QoS: 1},
		},
	}
	if err := sess.Reconcile(ctx, finalPlan); err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("verify-rx", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var received atomic.Int32
	recvCtx, recvCancel := context.WithCancel(ctx)
	var recvWg sync.WaitGroup
	recvWg.Add(1)
	go func() {
		defer recvWg.Done()
		_ = recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
			received.Add(1)
			return del.Ack(ctx) // settle like the runtime does
		})
	}()

	select {
	case <-recv.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not start")
	}
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: verifyTopic,
		Payload: []byte("verify-state"),
	}), Address: verifyTopic}); err != nil {
		t.Fatalf("Send to verify topic: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for received.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for message on verify topic — activeSubs may be corrupted")
		case <-time.After(50 * time.Millisecond):
		}
	}

	recvCancel()
	recvWg.Wait()
}

func topicForGoroutine(goroutine, iter int) string {
	return "conc/" + string(rune('a'+goroutine)) + "/" + string(rune('0'+iter))
}
