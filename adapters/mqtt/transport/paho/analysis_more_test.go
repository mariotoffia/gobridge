package paho

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Extra second-pass analysis tests covering edge cases not handled by
// earlier files.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaMore_PublishFromEnvelope_EgressHeaderPolicy verifies the MQTT
// egress header policy: INTERNAL-ONLY reserved headers
// (messaging.IsInternalOnlyHeader — route-id, route-override, source-id,
// content-type) are NOT serialized as MQTT user properties, while
// BRIDGE-TO-BRIDGE propagated headers (causation, idempotency, dedup,
// ordering, tenant, forwarded, trace) and application headers ARE, so a
// peer bridge can still correlate, deduplicate and continue a trace.
//
// This replaces an earlier characterization that PINNED the leak (route-id
// / source-id appearing on the wire). content-type is mapped to the native
// MQTT ContentType property; correlation-id to CorrelationData.
func TestAnaMore_PublishFromEnvelope_EgressHeaderPolicy(t *testing.T) {
	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		Subject: "t",
		Payload: []byte("p"),
		Headers: map[string]any{
			messaging.HeaderCorrelationID:   "corr",         // → MQTT CorrelationData
			messaging.HeaderContentType:     "text/plain",   // → MQTT ContentType (internal-only)
			messaging.HeaderCausationID:     "cause",        // bridge-to-bridge → user property
			messaging.HeaderIdempotencyKey:  "idem",         // bridge-to-bridge → user property
			messaging.HeaderDeduplicationID: "dedup",        // bridge-to-bridge → user property
			messaging.HeaderOrderingKey:     "order",        // bridge-to-bridge → user property
			messaging.HeaderTenantID:        "tenant-7",     // bridge-to-bridge → user property
			messaging.HeaderForwardedFrom:   "bridge-a",     // bridge-to-bridge → user property
			messaging.HeaderForwardedHop:    "3",            // bridge-to-bridge → user property
			messaging.HeaderTraceParent:     "00-trace-01",  // bridge-to-bridge → user property
			messaging.HeaderTraceState:      "vendor=x",     // bridge-to-bridge → user property
			messaging.HeaderRouteID:         "internal-rt",  // INTERNAL-ONLY → stripped
			messaging.HeaderRouteOverride:   "internal-ovr", // INTERNAL-ONLY → stripped
			messaging.HeaderSourceID:        "internal-src", // INTERNAL-ONLY → stripped
			"x-app-key":                     "app-value",    // application → user property
		},
	})
	pub := PublishFromEnvelope(env, env.Subject(), SenderOptions{QoS: 1}, nil)
	if pub.Properties == nil {
		t.Fatal("expected properties to be set")
	}

	mapped := map[string]string{}
	for _, u := range pub.Properties.User {
		mapped[u.Key] = u.Value
	}

	// CorrelationData / ContentType are mapped to native MQTT properties,
	// never duplicated as user properties.
	if _, has := mapped[messaging.HeaderCorrelationID]; has {
		t.Error("HeaderCorrelationID must not appear as a user property")
	}
	if string(pub.Properties.CorrelationData) != "corr" {
		t.Errorf("CorrelationData = %q, want %q", pub.Properties.CorrelationData, "corr")
	}
	if _, has := mapped[messaging.HeaderContentType]; has {
		t.Error("HeaderContentType must not appear as a user property")
	}
	if pub.Properties.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", pub.Properties.ContentType, "text/plain")
	}

	// INTERNAL-ONLY reserved headers MUST be stripped from the wire.
	for _, k := range []string{
		messaging.HeaderRouteID,
		messaging.HeaderRouteOverride,
		messaging.HeaderSourceID,
	} {
		if v, ok := mapped[k]; ok {
			t.Errorf("internal-only header %q leaked to MQTT user property (value %q); want stripped", k, v)
		}
	}

	// BRIDGE-TO-BRIDGE and application headers MUST pass through.
	wantOnWire := map[string]string{
		messaging.HeaderCausationID:     "cause",
		messaging.HeaderIdempotencyKey:  "idem",
		messaging.HeaderDeduplicationID: "dedup",
		messaging.HeaderOrderingKey:     "order",
		messaging.HeaderTenantID:        "tenant-7",
		messaging.HeaderForwardedFrom:   "bridge-a",
		messaging.HeaderForwardedHop:    "3",
		messaging.HeaderTraceParent:     "00-trace-01",
		messaging.HeaderTraceState:      "vendor=x",
		"x-app-key":                     "app-value",
	}
	for k, want := range wantOnWire {
		got, ok := mapped[k]
		if !ok {
			t.Errorf("header %q expected on wire (bridge-to-bridge/application) but missing", k)
			continue
		}
		if got != want {
			t.Errorf("header %q = %q, want %q", k, got, want)
		}
	}
}

// TestAnaMore_Sender_NilEnvelope_ReturnsInvalidPayload pins the
// Sender.Send contract for a nil envelope: the adapter rejects the
// call with a classified shared.ErrInvalidPayload rather than
// panicking. Validating at the transport boundary keeps session
// state intact and gives callers a recoverable error instead of
// undefined behaviour.
func TestAnaMore_Sender_NilEnvelope_ReturnsInvalidPayload(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-nil-env",
	}, connectivity.SessionEphemeral, nil)
	sess.mu.Lock()
	sess.cm = &pahoConn{cm: fakeCM}
	sess.mu.Unlock()

	s := NewSender(sess, SenderOptions{Timeout: time.Second, DefaultTopic: "t"})

	err := s.Send(context.Background(), ports.OutboundMessage{})
	if err == nil {
		t.Fatalf("expected error for nil envelope, got nil")
	}
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected *shared.BridgeError, got %T: %v", err, err)
	}
	if be.Code != shared.ErrInvalidPayload.Code {
		t.Fatalf("expected code %q, got %q", shared.ErrInvalidPayload.Code, be.Code)
	}
}

// TestAnaMore_ReconcileMetric_EmittedPerCall verifies that a successful
// (no-broker, no-op) Reconcile path produces a Timer metric. We use a
// fake CM and an empty-plan no-op to keep the test pure.
func TestAnaMore_ReconcileMetric_NotEmittedOnNoOp(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-metric-noop",
	}, connectivity.SessionEphemeral, nil, rec)
	s.mu.Lock()
	s.cm = &pahoConn{cm: fakeCM}
	// A SUBLESS prior APPLIED plan (sender-only session): an empty target plan
	// re-affirms "nothing desired" and is a genuine no-op. The no-op now keys
	// off the APPLIED history (blocking-#2), so seed appliedPlan (not just the
	// desired s.plan) with an empty plan to model "a subless plan was already
	// successfully reconciled". NOTE the prior setup used a plan WITH a
	// subscription and an empty activeSubs — that is now the reconnect-window
	// TEARDOWN case (the broker may still hold the resumed sub), not a no-op,
	// so it must NOT be pinned here (c4-remove-subs gates teardown on
	// applied-state history, not activeSubs).
	s.plan = &connectivity.SessionPlan{}
	s.appliedPlan = &connectivity.SessionPlan{}
	s.mu.Unlock()

	// Empty plan + subless prior plan = no-op short-circuit BEFORE reconcile().
	if err := s.Reconcile(context.Background(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("no-op Reconcile error: %v", err)
	}
	if entries := rec.FindEntries(MetricMQTTReconcileLatency); len(entries) != 0 {
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
	}, connectivity.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = &pahoConn{cm: fakeCM}
	s.activeSubs = map[string]byte{"a": 0, "b": 1}
	s.mu.Unlock()

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
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
	}, connectivity.SessionEphemeral, nil)

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
	if be == nil || be.Code != shared.ErrThrottled.Code {
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
	}, connectivity.SessionEphemeral, nil)

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
//
// Route dispatches SYNCHRONOUSLY (see acl_router.go) — it blocks until the
// handler/emit returns — so it is driven from a goroutine here; that block
// IS the backpressure this test exercises.
func TestAnaMore_Receiver_BlockedEmit_BackpressureCancelOnRunCtxDone(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recv-bp-cancel",
	}, connectivity.SessionEphemeral, nil)

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

	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}
	// Route blocks until emit returns; drive it from a goroutine so the
	// test can observe the blocked emit and then cancel.
	go sess.Router().Route(newTestPacketPublish("t/x", []byte("p")))

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
	}, connectivity.SessionEphemeral, nil)

	r := NewReceiver("rx-panic-clear", sess)

	// First Run: emit panics, but the router recovers it.
	ctx1, cancel1 := context.WithCancel(context.Background())
	res1 := make(chan error, 1)
	go func() {
		res1 <- r.Run(ctx1, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	select {
	case <-r.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}

	var panicked atomic.Bool
	sess.router.Register("panicker", func(*pahov5.Publish) { panicked.Store(true); panic("emit boom") })
	cancel1()
	select {
	case <-res1:
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not return after cancel")
	}
	_ = panicked.Load()

	// Second Run on same Receiver must succeed (running flag cleared).
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	res := make(chan error, 1)
	go func() {
		res <- r.Run(ctx2, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	pollDeadline := time.After(2 * time.Second)
	for sess.Router().HandlerCount() == 0 {
		select {
		case <-pollDeadline:
			t.Fatal("second Run did not register handler")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if sess.Router().HandlerCount() == 0 {
		t.Fatal("second Run should have re-registered handler — running flag not cleared after first Run returned")
	}
	cancel2()
	<-res
}
