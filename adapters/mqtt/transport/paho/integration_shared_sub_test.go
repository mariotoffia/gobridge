package paho_test

import (
	"context"
	"fmt"
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

// ═══════════════════════════════════════════════════════════════════
// S4: MQTT v5 Shared Subscription Integration Tests
//
// Validates that $share/group/topic subscriptions distribute messages
// across subscribers (competing consumer) while plain subscriptions
// deliver to ALL subscribers (N-fold duplication).
//
// Infrastructure:
// ───────────────────────────────────────────────────────────────
//   ┌──────────┐     ┌──────────────────┐     ┌──────────┐
//   │ Publisher│────▶│ Mosquitto broker │────▶│ Sub A    │
//   │ (sess-p) │     │ (Docker)         │────▶│ Sub B    │
//   └──────────┘     └──────────────────┘     └──────────┘
// ───────────────────────────────────────────────────────────────
//
// Teardown:
//   Container is shared across all paho integration tests and
//   stopped by TestMain via mqttlocal.Shutdown().
// ═══════════════════════════════════════════════════════════════════

// TestIntegration_SharedSubscription_CompetingConsumers validates that
// two clients subscribing to $share/group/topic each receive a subset
// of messages (competing consumer pattern), and the total equals the
// number published.
//
// Scenario:
// ───────────────────────────────────────────────────────────────
//
//	Publisher ──▶ topic "shared-sub/test"
//	Sub A subscribes: $share/testgroup/shared-sub/test
//	Sub B subscribes: $share/testgroup/shared-sub/test
//
//	Broker distributes each message to exactly one of {A, B}
//	Total received by A + B == total published
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Combined count from A and B equals msgCount
//   - Neither A nor B receives all messages alone (both got work)
func TestIntegration_SharedSubscription_CompetingConsumers(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const (
		baseTopic  = "shared-sub/test"
		shareTopic = "$share/testgroup/" + baseTopic
		msgCount   = 20
	)

	subA := startSubscriber(t, ctx, brokerURL, "sub-a", shareTopic)
	defer subA.stop()
	subB := startSubscriber(t, ctx, brokerURL, "sub-b", shareTopic)
	defer subB.stop()

	publisher := newPublisherSession(t, ctx, brokerURL, "pub")
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	for i := 0; i < msgCount; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: baseTopic,
			Payload: []byte(fmt.Sprintf("shared-msg-%d", i)),
		})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: env.Subject()}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	waitForTotal(t, 10*time.Second, msgCount, &subA.count, &subB.count)

	countA := subA.count.Load()
	countB := subB.count.Load()

	if countA+countB != msgCount {
		t.Errorf("total received = %d, want %d (A=%d, B=%d)",
			countA+countB, msgCount, countA, countB)
	}

	if countA == 0 || countB == 0 {
		t.Errorf("expected both subscribers to receive messages, got A=%d B=%d "+
			"(broker may not support $share/ or only one subscriber was active)",
			countA, countB)
	}
}

// TestIntegration_PlainSubscription_FanOut validates that two clients
// subscribing to a plain topic (without $share/) each receive ALL
// messages, demonstrating the N-fold duplication that S4 prevents.
//
// Scenario:
// ───────────────────────────────────────────────────────────────
//
//	Publisher ──▶ topic "fanout/test"
//	Sub A subscribes: fanout/test  (plain)
//	Sub B subscribes: fanout/test  (plain)
//
//	Broker delivers every message to BOTH A and B
//	Each receives msgCount messages → total = 2 * msgCount
//
// ───────────────────────────────────────────────────────────────
//
// Assertions:
//   - Both A and B each receive exactly msgCount
func TestIntegration_PlainSubscription_FanOut(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const (
		topic    = "fanout/test"
		msgCount = 10
	)

	subA := startSubscriber(t, ctx, brokerURL, "fanout-a", topic)
	defer subA.stop()
	subB := startSubscriber(t, ctx, brokerURL, "fanout-b", topic)
	defer subB.stop()

	publisher := newPublisherSession(t, ctx, brokerURL, "fanout-pub")
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	for i := 0; i < msgCount; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: topic,
			Payload: []byte(fmt.Sprintf("fanout-msg-%d", i)),
		})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: env.Subject()}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	waitForTotal(t, 10*time.Second, 2*msgCount, &subA.count, &subB.count)

	countA := subA.count.Load()
	countB := subB.count.Load()

	if countA != msgCount {
		t.Errorf("subscriber A received %d, want %d (fan-out should deliver to all)", countA, msgCount)
	}
	if countB != msgCount {
		t.Errorf("subscriber B received %d, want %d (fan-out should deliver to all)", countB, msgCount)
	}
}

// TestIntegration_SharedSubscription_PayloadIntegrity validates that
// messages delivered via $share/ subscriptions arrive with correct
// payload and headers intact.
//
// Assertions:
//   - Received payload matches sent payload
//   - Custom headers round-trip through $share/ subscription
func TestIntegration_SharedSubscription_PayloadIntegrity(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const (
		baseTopic  = "shared-payload/test"
		shareTopic = "$share/integrity/" + baseTopic
	)

	sess := newSessionWithEvents(t, ctx, brokerURL, "payload-sub")
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: shareTopic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-payload", sess)
	var received []*messaging.Envelope
	var mu sync.Mutex

	recvCtx, recvCancel := context.WithCancel(ctx)
	defer recvCancel()
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
	t.Cleanup(func() { recvCancel(); wg.Wait() })

	publisher := newPublisherSession(t, ctx, brokerURL, "payload-pub")
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: baseTopic,
		Payload: []byte("integrity-check"),
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "corr-shared",
			messaging.HeaderContentType:   "application/json",
			"x-custom":                    "shared-value",
		},
	})

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: env.Subject()}); err != nil {
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
			t.Fatal("timed out waiting for shared subscription message")
		case <-time.After(50 * time.Millisecond):
		}
	}

	recvCancel()
	wg.Wait()

	mu.Lock()
	msg := received[0]
	mu.Unlock()

	if string(msg.Payload) != "integrity-check" {
		t.Errorf("payload = %q, want %q", msg.Payload, "integrity-check")
	}
	if v, _ := messaging.GetHeaderString(msg.Headers(), messaging.HeaderCorrelationID); v != "corr-shared" {
		t.Errorf("correlation = %q, want %q", v, "corr-shared")
	}
	if v, _ := messaging.GetHeaderString(msg.Headers(), messaging.HeaderContentType); v != "application/json" {
		t.Errorf("content-type = %q, want %q", v, "application/json")
	}
	if v, _ := messaging.GetHeaderString(msg.Headers(), "x-custom"); v != "shared-value" {
		t.Errorf("x-custom = %q, want %q", v, "shared-value")
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type subscriber struct {
	count  atomic.Int64
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *subscriber) stop() {
	s.cancel()
	s.wg.Wait()
}

func startSubscriber(t *testing.T, ctx context.Context, brokerURL, prefix, topic string) *subscriber {
	t.Helper()
	sess := newSessionWithEvents(t, ctx, brokerURL, prefix)

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		_ = sess.Close(context.Background())
		t.Fatalf("Reconcile (%s): %v", prefix, err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-"+prefix, sess)
	sub := &subscriber{}
	recvCtx, recvCancel := context.WithCancel(ctx)
	sub.cancel = func() {
		recvCancel()
		_ = sess.Close(context.Background())
	}

	sub.wg.Add(1)
	go func() {
		defer sub.wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, _ ports.Delivery) error {
			sub.count.Add(1)
			return nil
		})
	}()

	return sub
}

func newSessionWithEvents(t *testing.T, ctx context.Context, brokerURL, prefix string) *paho.Session {
	t.Helper()
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{brokerURL},
		ClientID:       mqttlocal.UniqueClientID(prefix),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start (%s): %v", prefix, err)
	}

	select {
	case <-sess.Events():
	case <-time.After(3 * time.Second):
		_ = sess.Close(context.Background())
		t.Fatalf("timed out waiting for connect event (%s)", prefix)
	}

	return sess
}

func newPublisherSession(t *testing.T, ctx context.Context, brokerURL, prefix string) *paho.Session {
	t.Helper()
	return newSessionWithEvents(t, ctx, brokerURL, prefix)
}

func waitForTotal(t *testing.T, timeout time.Duration, want int64, counters ...*atomic.Int64) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		var total int64
		for _, c := range counters {
			total += c.Load()
		}
		if total >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d messages (got %d)", want, total)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
