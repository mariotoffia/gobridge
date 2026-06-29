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
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Analysis-driven integration tests against a real Mosquitto broker.
// These exercise behaviours that the existing suite either lacks or
// only covers via simulation (no real broker).
// ═══════════════════════════════════════════════════════════════════════════

// makeSession is a tiny helper to keep these tests focused.
func makeSession(t *testing.T, ctx context.Context, url, prefix string) *paho.Session {
	t.Helper()
	s := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID(prefix),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start (%s): %v", prefix, err)
	}
	// drain the SessionConnected event
	select {
	case <-s.Events():
	case <-time.After(3 * time.Second):
	}
	return s
}

// TestAnaIntg_StartAfterClose_RealBroker_DoesNotReconnect is the
// strongest signal for the BUG-SAC fix. With the bug, Start would
// connect to the real broker and install a zombie cm; with the fix,
// Start returns ErrUnavailable and no second connection is ever made.
func TestAnaIntg_StartAfterClose_RealBroker_DoesNotReconnect(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("ana-sac-real"),
		KeepAlive:      10,
		ConnectTimeout: 5 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !s.Health(ctx).Connected {
		t.Fatal("session should be connected after first Start")
	}

	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second Start MUST fail; with the bug it would silently install
	// a fresh ConnectionManager whose events are unobservable.
	err := s.Start(ctx)
	if err == nil {
		t.Fatal("BUG-SAC: second Start after Close must error")
	}
	be, ok := err.(*shared.BridgeError)
	if !ok || be.Code != shared.ErrUnavailable.Code {
		t.Fatalf("BUG-SAC: err = %v, want ErrUnavailable", err)
	}
	if s.ConnectionManager() != nil {
		t.Fatal("BUG-SAC: no zombie ConnectionManager allowed after Start-on-closed")
	}
	if s.Health(ctx).Connected {
		t.Fatal("BUG-SAC: Health.Connected must remain false")
	}
}

// TestAnaIntg_ReconcileEmptyPlan_DoesNotUnsubscribe verifies the
// documented no-op behaviour against a real broker. After applying a
// non-empty plan, calling Reconcile with an empty plan must NOT cause
// the previously-subscribed messages to be missed.
func TestAnaIntg_ReconcileEmptyPlan_DoesNotUnsubscribe(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := fmt.Sprintf("ana/intg/empty/%d", time.Now().UnixNano())

	sess := makeSession(t, ctx, url, "ana-empty-recon")
	defer func() { _ = sess.Close(context.Background()) }()

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("initial Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	// Empty plan must be a no-op.
	if err := sess.Reconcile(ctx, connectivity.SessionPlan{}); err != nil {
		t.Fatalf("empty Reconcile must succeed (no-op): %v", err)
	}

	recv := paho.NewReceiver("rx-empty", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var got atomic.Int32
	rctx, rcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(rctx, func(_ context.Context, _ ports.Delivery) error {
			got.Add(1)
			return nil
		})
	}()
	defer func() { rcancel(); wg.Wait() }()

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: topic, Payload: []byte("after-empty-reconcile"),
	}), Address: topic}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for got.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected message to arrive — empty Reconcile must NOT unsubscribe")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestAnaIntg_LargePayload_RoundTrip exercises payloads much larger
// than the MQTT default packet size to flush out length-handling and
// deep-copy correctness.
func TestAnaIntg_LargePayload_RoundTrip(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := fmt.Sprintf("ana/intg/large/%d", time.Now().UnixNano())
	sess := makeSession(t, ctx, url, "ana-large")
	defer func() { _ = sess.Close(context.Background()) }()

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-large", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})

	const sz = 128 * 1024
	payload := make([]byte, sz)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	var (
		mu      sync.Mutex
		recvBuf []byte
	)

	rctx, rcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(rctx, func(_ context.Context, del ports.Delivery) error {
			mu.Lock()
			recvBuf = make([]byte, len(del.Envelope().Payload()))
			copy(recvBuf, del.Envelope().Payload())
			mu.Unlock()
			rcancel() // we have what we need
			return nil
		})
	}()

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: topic, Payload: payload}), Address: topic}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(recvBuf) != sz {
		t.Fatalf("received %d bytes, want %d", len(recvBuf), sz)
	}
	for i, b := range recvBuf {
		if b != byte(i%251) {
			t.Fatalf("payload byte %d = 0x%02X, want 0x%02X", i, b, byte(i%251))
		}
	}
}

// TestAnaIntg_MultipleReceivers_SameTopic_AllReceive verifies the
// router fan-out semantics on a real broker: two Receivers attached to
// the same Session both receive every message.
func TestAnaIntg_MultipleReceivers_SameTopic_AllReceive(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := fmt.Sprintf("ana/intg/fanout/%d", time.Now().UnixNano())
	sess := makeSession(t, ctx, url, "ana-fanout")
	defer func() { _ = sess.Close(context.Background()) }()

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv1 := paho.NewReceiver("rx-fan-1", sess)
	recv2 := paho.NewReceiver("rx-fan-2", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var got1, got2 atomic.Int32
	rctx, rcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = recv1.Run(rctx, func(_ context.Context, _ ports.Delivery) error { got1.Add(1); return nil })
	}()
	go func() {
		defer wg.Done()
		_ = recv2.Run(rctx, func(_ context.Context, _ ports.Delivery) error { got2.Add(1); return nil })
	}()
	defer func() { rcancel(); wg.Wait() }()

	const n = 5
	for i := 0; i < n; i++ {
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: topic, Payload: []byte(fmt.Sprintf("m-%d", i)),
		}), Address: topic}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	deadline := time.After(10 * time.Second)
	for got1.Load() < int32(n) || got2.Load() < int32(n) {
		select {
		case <-deadline:
			t.Fatalf("router fan-out incomplete: got1=%d got2=%d, want both ≥ %d",
				got1.Load(), got2.Load(), n)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestAnaIntg_HighConcurrencyPublish_NoLoss publishes from many
// goroutines simultaneously while a receiver is attached. All messages
// must arrive (per QoS 1 contract).
func TestAnaIntg_HighConcurrencyPublish_NoLoss(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	topic := fmt.Sprintf("ana/intg/conc/%d", time.Now().UnixNano())
	sess := makeSession(t, ctx, url, "ana-conc-pub")
	defer func() { _ = sess.Close(context.Background()) }()

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-conc", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})

	var got atomic.Int64
	rctx, rcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(rctx, func(_ context.Context, _ ports.Delivery) error {
			got.Add(1)
			return nil
		})
	}()

	const goroutines = 8
	const perG = 25
	var pwg sync.WaitGroup
	pwg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer pwg.Done()
			for i := 0; i < perG; i++ {
				if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
					Subject: topic, Payload: []byte(fmt.Sprintf("g%d-i%d", id, i)),
				}), Address: topic}); err != nil {
					t.Errorf("Send g%d i%d: %v", id, i, err)
					return
				}
			}
		}(g)
	}
	pwg.Wait()

	want := int64(goroutines * perG)
	deadline := time.After(20 * time.Second)
	for got.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("received %d / %d messages", got.Load(), want)
		case <-time.After(100 * time.Millisecond):
		}
	}
	rcancel()
	wg.Wait()
}

// TestAnaIntg_ReconcileSameTopicTwice_Idempotent verifies that
// re-reconciling the same plan is a cheap no-op (no broker error,
// no duplicate subscribe).
func TestAnaIntg_ReconcileSameTopicTwice_Idempotent(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := fmt.Sprintf("ana/intg/idemp/%d", time.Now().UnixNano())
	sess := makeSession(t, ctx, url, "ana-idemp")
	defer func() { _ = sess.Close(context.Background()) }()

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}
	for i := 0; i < 3; i++ {
		if err := sess.Reconcile(ctx, plan); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}

	// Verify subscription still works.
	recv := paho.NewReceiver("rx-idemp", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	var got atomic.Int32
	rctx, rcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(rctx, func(_ context.Context, _ ports.Delivery) error { got.Add(1); return nil })
	}()
	defer func() { rcancel(); wg.Wait() }()

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: topic, Payload: []byte("p")}), Address: topic}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for got.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected exactly 1 message; sub was not idempotent")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// NEGATIVE: assert no duplicate arrives from a (hypothetical) double-subscribe.
	<-time.After(300 * time.Millisecond)
	if n := got.Load(); n != 1 {
		t.Fatalf("got %d messages, want exactly 1 (idempotent reconcile)", n)
	}
}

// TestAnaIntg_HealthDuringTraffic_RemainsStable verifies that calling
// Health while traffic is flowing returns Connected=true and does not
// race (verify with -race).
func TestAnaIntg_HealthDuringTraffic_RemainsStable(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	topic := fmt.Sprintf("ana/intg/health/%d", time.Now().UnixNano())
	sess := makeSession(t, ctx, url, "ana-health-traffic")
	defer func() { _ = sess.Close(context.Background()) }()

	if err := sess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubActive(t, sess, 5*time.Second)

	recv := paho.NewReceiver("rx-health", sess)
	sender := paho.NewSender(sess, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})

	rctx, rcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(rctx, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	defer func() { rcancel(); wg.Wait() }()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 50; i++ {
			h := sess.Health(ctx)
			if !h.Connected {
				t.Errorf("Health.Connected = false at iteration %d", i)
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()

	for i := 0; i < 30; i++ {
		_ = sender.Send(ctx, ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: topic, Payload: []byte("p")}), Address: topic})
	}
	<-pollDone
}
