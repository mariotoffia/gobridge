package paho_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestIntegration_PersistentSessionQueuedQoS1AndQoS2 proves broker-side
// Session Present functionally. A persistent subscription is established,
// its client goes offline, QoS 1 and QoS 2 messages are accepted and queued,
// Mosquitto persists them across a restart, and a new Session using the same
// effective ClientID receives both without creating a different broker session.
func TestIntegration_PersistentSessionQueuedQoS1AndQoS2(t *testing.T) {
	if testing.Short() {
		t.Skip("persistent broker restart integration test")
	}
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	brokerURL := broker.URL()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	clientID := mqttlocal.UniqueClientID("task14-persistent")
	topicPrefix := "task14/session-present/" + mqttlocal.UniqueClientID("topic")
	topicQoS1 := topicPrefix + "/qos1"
	topicQoS2 := topicPrefix + "/qos2"
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: topicQoS1, QoS: 1},
			{Topic: topicQoS2, QoS: 2},
		},
	}
	expected := []string{"producer-qos1", "producer-qos2"}
	accountant, err := prodid.New(expected, false)
	if err != nil {
		t.Fatalf("new producer accountant: %v", err)
	}

	first := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              clientID,
		KeepAlive:             5,
		ConnectTimeout:        10 * time.Second,
		CleanStart:            false,
		SessionExpiryInterval: 300,
	}, connectivity.SessionPersistent, nil)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first persistent session: %v", err)
	}
	if err := first.Reconcile(ctx, plan); err != nil {
		t.Fatalf("reconcile first persistent session: %v", err)
	}
	waitSubActive(t, first, 10*time.Second)
	if err := first.Close(ctx); err != nil {
		t.Fatalf("take first persistent session offline: %v", err)
	}

	publishQueuedQoS(t, ctx, brokerURL, topicQoS1, topicQoS2, expected)
	broker.StopGraceful()
	broker.RestartGraceful()

	second := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              clientID,
		KeepAlive:             5,
		ConnectTimeout:        10 * time.Second,
		CleanStart:            false,
		SessionExpiryInterval: 300,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	if err := second.Start(ctx); err != nil {
		t.Fatalf("resume persistent session: %v", err)
	}
	if err := second.Reconcile(ctx, plan); err != nil {
		t.Fatalf("reconcile resumed persistent session: %v", err)
	}

	receiver := paho.NewReceiver("task14-resumed-receiver", second)
	runCtx, stopReceiver := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(ctx context.Context, delivery ports.Delivery) error {
			envelope := delivery.Envelope()
			key, _ := messaging.GetHeaderString(envelope.Headers(), paho.HeaderMessageID)
			accountant.ObserveOutput(key, envelope.ID())
			return delivery.Ack(ctx)
		})
	}()
	wait.RequireClosed(t, receiver.Started(), 5*time.Second)
	wait.Until(t, 15*time.Second, "queued QoS 1/2 delivery from resumed broker session", func() bool {
		return len(accountant.Reconcile().Missing) == 0
	})

	health := second.Health(ctx)
	if !health.Connected || health.SubscriptionsSatisfied == nil || !*health.SubscriptionsSatisfied {
		t.Fatalf("resumed session health does not show current reconcile evidence: %+v", health)
	}
	if health.ServiceLevel != ports.ServiceLevelFull {
		t.Fatalf("resumed session service level = %s, want %s", health.ServiceLevel, ports.ServiceLevelFull)
	}
	report := accountant.Reconcile()
	if !report.Exact() || len(report.DLQ) != 0 || len(report.IntentionallyDropped) != 0 {
		t.Fatalf("queued persistent-session accounting failed: %s", report.String())
	}

	stopReceiver()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("resumed receiver stopped with error: %v", err)
	}
}

func publishQueuedQoS(
	t *testing.T,
	ctx context.Context,
	brokerURL, topicQoS1, topicQoS2 string,
	producerKeys []string,
) {
	t.Helper()
	endpoint, err := url.Parse(brokerURL)
	if err != nil {
		t.Fatalf("parse broker URL: %v", err)
	}
	publisher, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{endpoint},
		KeepAlive:                     10,
		CleanStartOnInitialConnection: true,
		ClientConfig: pahov5.ClientConfig{
			ClientID: mqttlocal.UniqueClientID("task14-offline-publisher"),
		},
	})
	if err != nil {
		t.Fatalf("create offline-queue publisher: %v", err)
	}
	defer func() { _ = publisher.Disconnect(context.Background()) }()
	if err := publisher.AwaitConnection(ctx); err != nil {
		t.Fatalf("connect offline-queue publisher: %v", err)
	}
	for i, spec := range []struct {
		topic string
		qos   byte
		key   string
	}{
		{topic: topicQoS1, qos: 1, key: producerKeys[0]},
		{topic: topicQoS2, qos: 2, key: producerKeys[1]},
	} {
		response, err := publisher.Publish(ctx, &pahov5.Publish{
			Topic:   spec.topic,
			QoS:     spec.qos,
			Payload: []byte("queued-" + spec.key),
			Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{
				{Key: paho.HeaderMessageID, Value: spec.key},
			}},
		})
		if err != nil {
			t.Fatalf("publish queued message %d at QoS %d: %v", i, spec.qos, err)
		}
		if response == nil || response.ReasonCode >= 0x80 {
			t.Fatalf("publish queued message %d PUBACK/PUBREC = %#v", i, response)
		}
	}
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
	}, connectivity.SessionEphemeral, nil)

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	drainEvent(t, sess)

	// Subscribe to topicA first.
	planA := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: topicA, QoS: 1},
		},
	}
	if err := sess.Reconcile(ctx, planA); err != nil {
		t.Fatalf("Reconcile planA: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	// Update plan: remove topicA, add topicB.
	planB := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
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
		_ = recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
			receivedTopics.Store(del.Envelope().Subject(), true)
			return del.Ack(ctx) // settle like the runtime does
		})
	}()

	// Send to both topics.
	for _, topic := range []string{topicA, topicB} {
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: topic,
			Payload: []byte("test-" + topic),
		}), Address: topic}); err != nil {
			t.Fatalf("Send to %s: %v", topic, err)
		}
	}

	// Wait for topicB message.
	waitForCondition(t, 5*time.Second, "topicB message", func() bool {
		_, ok := receivedTopics.Load(topicB)
		return ok
	})

	// Both QoS 1 publishes use one connection. MQTT preserves their packet
	// order, so observing topicB is the barrier for the earlier topicA publish;
	// no negative-delay window is needed.

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
	}, connectivity.SessionEphemeral, nil)

	if err := sess.Start(parentCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	drainEvent(t, sess)

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
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
		done <- sess.Reconcile(parentCtx, connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{
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
// the session's Health reports connected — proving the connection was
// established successfully.
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
	}, connectivity.SessionEphemeral, nil)

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
	}, connectivity.SessionEphemeral, nil)

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
				plan := connectivity.SessionPlan{
					Subscriptions: []connectivity.SubscriptionPlan{
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
	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
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
		_ = recv.Run(recvCtx, func(ctx context.Context, del ports.Delivery) error {
			got.Add(1)
			return del.Ack(ctx) // settle like the runtime does
		})
	}()

	wait.RequireClosed(t, recv.Started(), 5*time.Second)
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: verifyTopic,
		Payload: []byte("verify"),
	}), Address: verifyTopic}); err != nil {
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
	wait.Until(t, timeout, desc, fn)
}
