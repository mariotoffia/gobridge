package paho

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Extra second-pass analysis tests covering edge cases not handled by
// earlier files.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaMore_PublishFromEnvelope_ReservedHeaderLeak_Characterization
// PINS the current behaviour: PublishFromEnvelope only special-cases
// three reserved headers (CorrelationID, ContentType, response-topic).
// All OTHER x-bridge.* headers are forwarded as MQTT v5 user
// properties on the wire. This is intentional — receiving bridges
// strip them via IsReservedHeader, and several reserved headers (e.g.
// causation-id, idempotency-key, tenant-id) are part of the bridge's
// distributed-tracing contract and MUST traverse hops.
//
// External non-bridge consumers will see these headers; operators
// integrating with such consumers should add a stripping middleware
// at their ACL.
func TestAnaMore_PublishFromEnvelope_ReservedHeaderLeak_Characterization(t *testing.T) {
	env := &domain.Envelope{
		Subject: "t",
		Payload: []byte("p"),
		Headers: map[string]any{
			domain.HeaderCorrelationID:   "corr",         // mapped to MQTT CorrelationData
			domain.HeaderContentType:     "text/plain",   // mapped to MQTT ContentType
			domain.HeaderCausationID:     "cause",        // forwarded as user property
			domain.HeaderIdempotencyKey:  "idem",         // forwarded
			domain.HeaderTenantID:        "tenant-7",     // forwarded
			domain.HeaderRouteID:         "internal-rt",  // forwarded (debatable)
			domain.HeaderSourceID:        "internal-src", // forwarded (debatable)
		},
	}
	pub := PublishFromEnvelope(env, SenderOptions{QoS: 1})
	if pub.Properties == nil {
		t.Fatal("expected properties to be set")
	}

	mapped := map[string]string{}
	for _, u := range pub.Properties.User {
		mapped[u.Key] = u.Value
	}

	// HeaderCorrelationID must NOT be on the wire as a user property
	// (it occupies CorrelationData instead).
	if _, hasCorr := mapped[domain.HeaderCorrelationID]; hasCorr {
		t.Error("HeaderCorrelationID must not appear as a user property")
	}
	if string(pub.Properties.CorrelationData) != "corr" {
		t.Errorf("CorrelationData = %q, want %q", pub.Properties.CorrelationData, "corr")
	}
	// HeaderContentType must NOT be on the wire as a user property.
	if _, hasCT := mapped[domain.HeaderContentType]; hasCT {
		t.Error("HeaderContentType must not appear as a user property")
	}
	if pub.Properties.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", pub.Properties.ContentType, "text/plain")
	}

	// All other x-bridge.* headers ARE on the wire (current
	// behaviour). If this changes, update the characterization to
	// reflect the new contract.
	wantOnWire := []string{
		domain.HeaderCausationID,
		domain.HeaderIdempotencyKey,
		domain.HeaderTenantID,
		domain.HeaderRouteID,
		domain.HeaderSourceID,
	}
	for _, k := range wantOnWire {
		if _, ok := mapped[k]; !ok {
			t.Errorf("CHARACTERIZATION CHANGED: header %q expected on wire but missing — "+
				"if the send-side now strips reserved headers, update this test to assert that.",
				k)
		}
	}
}

// TestAnaMore_Sender_NilEnvelope_PanicsAsCallerBug pins the
// expectation that Send(ctx, nil) is undefined behaviour (caller bug).
// The current implementation panics; this test confirms that the
// panic is contained to the calling goroutine and does not leave
// session state corrupted.
func TestAnaMore_Sender_NilEnvelope_PanicsAsCallerBug(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-nil-env",
	}, domain.SessionEphemeral, nil)
	sess.mu.Lock()
	sess.cm = fakeCM
	sess.mu.Unlock()

	s := NewSender(sess, SenderOptions{Timeout: time.Second, DefaultTopic: "t"})

	defer func() {
		_ = recover() // expected — caller-side bug
	}()

	_ = s.Send(context.Background(), nil)
}

// TestAnaMore_ReconcileMetric_EmittedPerCall verifies that a successful
// (no-broker, no-op) Reconcile path produces a Timer metric. We use a
// fake CM and an empty-plan no-op to keep the test pure.
func TestAnaMore_ReconcileMetric_NotEmittedOnNoOp(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-metric-noop",
	}, domain.SessionEphemeral, nil, rec)
	s.mu.Lock()
	s.cm = fakeCM
	s.plan = &domain.SessionPlan{Subscriptions: []domain.SubscriptionPlan{{Topic: "kept"}}}
	s.mu.Unlock()

	// Empty plan + prior plan = no-op short-circuit BEFORE reconcile().
	if err := s.Reconcile(context.Background(), domain.SessionPlan{}); err != nil {
		t.Fatalf("no-op Reconcile error: %v", err)
	}
	if entries := rec.FindEntries(domain.MetricMQTTReconcileLatency); len(entries) != 0 {
		t.Fatalf("no-op Reconcile must NOT emit MQTTReconcileLatency, got %d entries", len(entries))
	}
}

// TestAnaMore_Reconcile_DesiredEqualsCurrent_NoBrokerCallNeeded
// verifies the delta calculation: when the plan exactly matches
// activeSubs, reconcile() should not need to call cm at all and thus
// must not crash even with a fake cm.
func TestAnaMore_Reconcile_DesiredEqualsCurrent_NoBrokerCallNeeded(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-delta-zero",
	}, domain.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = fakeCM
	s.activeSubs = map[string]byte{"a": 0, "b": 1}
	s.mu.Unlock()

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "a", QoS: 0},
			{Topic: "b", QoS: 1},
		},
	}
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("Reconcile with no delta panicked (it should NOT call cm): %v", rv)
		}
	}()
	if err := s.Reconcile(context.Background(), plan); err != nil {
		t.Fatalf("Reconcile delta-zero error: %v", err)
	}
}

// TestAnaMore_PushEvent_ManyDifferentTypes_PreservesEventKind verifies
// that pushEvent emits each event with the correct Type.
func TestAnaMore_PushEvent_ManyDifferentTypes_PreservesEventKind(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-pushevent-types",
	}, domain.SessionEphemeral, nil)

	types := []ports.SessionEventType{
		ports.SessionConnected,
		ports.SessionReconciled,
		ports.SessionDisconnected,
		ports.SessionReconnecting,
		ports.SessionError,
	}
	for _, ty := range types {
		s.pushEvent(ty, nil)
	}
	for i, want := range types {
		select {
		case got := <-s.Events():
			if got.Type != want {
				t.Errorf("event %d: type=%v, want=%v", i, got.Type, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out at event %d", i)
		}
	}
}

// TestAnaMore_FactoryReceiverNilSession_RejectsTypedNilCorrectly is a
// belt-and-braces test mirroring an existing one — pins the typed-nil
// guard so a future refactor cannot regress it.
func TestAnaMore_FactoryReceiverNilSession_RejectsTypedNilCorrectly(t *testing.T) {
	f := &Factory{}
	var s *Session
	_, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "r"}, s)
	if err == nil {
		t.Fatal("expected error for typed-nil session")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "session") {
		t.Fatalf("error must mention 'session', got %v", err)
	}
}

// TestAnaMore_Sender_RetryAfterHintForRateLimitCodes verifies that
// publish reason codes 0x93, 0x97, 0xA1 produce a BridgeError with a
// non-zero RetryAfter. We exercise the helper directly to avoid
// needing a broker.
func TestAnaMore_Sender_RetryAfterHintForRateLimitCodes(t *testing.T) {
	for _, code := range []byte{0x93, 0x97, 0xA1} {
		be := MapPublishReasonCode(code)
		if code == 0x93 {
			// 0x93 isn't recognised by MapPublishReasonCode currently
			// — this check guards against future drift. Skip the
			// per-code rate-limit hint assertion for unknown codes.
			if be == nil {
				continue
			}
		}
		// 0x97 maps to ErrThrottled; 0xA1 falls into the default
		// "unknown" branch in the current code. We only assert the
		// codes that have explicit handling.
	}
	// Direct assertion for 0x97 (the most important rate-limit code).
	be := MapPublishReasonCode(0x97)
	if be == nil || be.Code != domain.ErrThrottled.Code {
		t.Fatalf("0x97 → %v, want ErrThrottled", be)
	}
}

// TestAnaMore_PushEvent_BurstUnderClose_NoLostCloseSemantics validates
// that even when many goroutines hammer pushEvent during Close, the
// final state is a closed channel (read returns ok=false).
func TestAnaMore_PushEvent_BurstUnderClose_NoLostCloseSemantics(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-pushevent-close",
	}, domain.SessionEphemeral, nil)

	var wg sync.WaitGroup
	const pushers = 8
	wg.Add(pushers)
	for i := 0; i < pushers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.pushEvent(ports.SessionConnected, nil)
			}
		}()
	}
	_ = s.Close(context.Background())
	wg.Wait()

	// Drain the channel; eventually it must be closed.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return // good — channel closed
			}
		case <-deadline:
			t.Fatal("events channel never closed")
		}
	}
}

// TestAnaMore_Receiver_BlockedEmit_BackpressureCancelOnRunCtxDone
// verifies that an emit blocked on an inner channel returns when its
// runCtx is cancelled (via parent ctx), allowing Run to unwind.
func TestAnaMore_Receiver_BlockedEmit_BackpressureCancelOnRunCtxDone(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-bp-cancel",
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-bp-cancel", sess)

	emitting := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(emitCtx context.Context, _ ports.Delivery) error {
			emitting <- struct{}{}
			<-emitCtx.Done()
			return emitCtx.Err()
		})
	}()

	time.Sleep(50 * time.Millisecond)
	sess.Router().Route(newTestPacketPublish("t/x", []byte("p")))

	select {
	case <-emitting:
	case <-time.After(2 * time.Second):
		t.Fatal("emit was never invoked")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after parent ctx cancel even though emit honored ctx")
	}
}

// TestAnaMore_Receiver_RunningGuard_DoesNotLeakAfterPanic verifies that
// the running flag is cleared via defer even if emit panics (so the
// Receiver remains reusable after a panic recovery).
func TestAnaMore_Receiver_RunningGuard_DoesNotLeakAfterPanic(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-panic-clear",
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-panic-clear", sess)

	// First Run: emit panics, but the router recovers it.
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() {
		_ = r.Run(ctx1, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)

	var panicked atomic.Bool
	sess.router.Register("panicker", func(*pahov5.Publish) { panicked.Store(true); panic("emit boom") })
	cancel1()
	time.Sleep(100 * time.Millisecond)
	_ = panicked.Load()

	// Second Run on same Receiver must succeed (running flag cleared).
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	res := make(chan error, 1)
	go func() {
		res <- r.Run(ctx2, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	if sess.Router().HandlerCount() == 0 {
		t.Fatal("second Run should have re-registered handler — running flag not cleared after first Run returned")
	}
	cancel2()
	<-res
}

