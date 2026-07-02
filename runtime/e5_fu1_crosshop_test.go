package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// ═══════════════════════════════════════════════════════════════════════════
// E5-FU1: stale bridge-to-bridge redelivery count must be stripped at egress
//
// In a bridge A → bridge B topology the source transport's redelivery-count
// header is transport-namespaced (not x-bridge.* reserved), so nothing strips
// it at egress. If it rides the hop, bridge B's receiveCount() reads A's stale
// upstream value (first-match-wins) instead of B's own fresh count, corrupting
// MaxReplayAttempts / poison detection. Both egress chokepoints (direct-hold
// send and shared-outbox persist) must strip it from the OUTBOUND copy while
// leaving the SOURCE envelope intact (receiveCount re-reads the source on the
// retry/poison paths).
//
// "asb.delivery-count" is used as the literal wire string here because this is
// an EXTERNAL test package and cannot reference the unexported
// headerASBDeliveryCount const in runtime/route/dispatch.go — the literal
// mirrors that const by value.
// ═══════════════════════════════════════════════════════════════════════════

// asbDeliveryCountHeader mirrors the unexported headerASBDeliveryCount const in
// runtime/route/dispatch.go (an external test package cannot import it).
const asbDeliveryCountHeader = "asb.delivery-count"

// TestE5FU1_DirectHold_StripsStaleReceiveCountFromOutbound proves the
// direct-hold egress chokepoint strips the stale upstream count from the
// outbound clone while preserving benign headers and the source envelope.
func TestE5FU1_DirectHold_StripsStaleReceiveCountFromOutbound(t *testing.T) {
	const (
		bindingID   = "b1"
		destAddress = "destination/topic"
	)

	sender := newCaptureSender()

	bindings := []routing.DestinationBinding{{ID: bindingID, Address: destAddress}}
	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{{BindingID: bindingID}})
	resolver, err := runtime.NewRuleResolver(bindings, rules, "")
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}

	receiver := NewFakeReceiver()
	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:    "e5-direct-route",
		Policy:     routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver:   receiver,
		Sender:     sender,
		DLQ:        dlq.New(NewFakeDLQStore()),
		Resolver:   resolver,
		Bindings:   bindings,
		InstanceID: "bridge-B",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Source envelope arrives at bridge B still carrying bridge A's stale
	// upstream delivery count plus a benign app header that must survive.
	src := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e5-direct",
		Subject: "s",
		Payload: []byte("p"),
		Headers: map[string]any{asbDeliveryCountHeader: 9, "x-app.keep": "yes"},
	})
	del := NewFakeDelivery(src)

	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	select {
	case <-sender.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for send")
	}

	msg, ok := sender.last()
	if !ok {
		t.Fatal("no OutboundMessage captured")
	}
	if msg.Envelope == nil {
		t.Fatal("OutboundMessage.Envelope is nil")
	}

	// WHY: the stale upstream count must be stripped from the outbound clone so
	// the downstream bridge does not misread it as its own receiveCount (E5-FU1).
	if _, present := msg.Envelope.Headers()[asbDeliveryCountHeader]; present {
		t.Errorf("outbound envelope still carries %q; stale upstream count must be stripped", asbDeliveryCountHeader)
	}
	// WHY: the strip is surgical — benign application headers still ride the hop.
	if v, ok := msg.Envelope.Headers()["x-app.keep"]; !ok || v != "yes" {
		t.Errorf("outbound x-app.keep = %v (present=%v), want \"yes\" (only count headers are stripped)", v, ok)
	}

	// WHY: the SOURCE envelope must be untouched — receiveCount(env) is re-read
	// from it on the retry/poison paths, so its redelivery count must remain.
	if got, ok := del.Envelope().Headers()[asbDeliveryCountHeader]; !ok || got != 9 {
		t.Errorf("source %q = %v (present=%v), want 9 (source must not be mutated)", asbDeliveryCountHeader, got, ok)
	}
}

// TestE5FU1_SharedOutbox_StripsStaleReceiveCountFromPersisted proves the
// shared-outbox egress chokepoint strips the stale upstream count from the
// persisted record while preserving benign headers and the source envelope.
func TestE5FU1_SharedOutbox_StripsStaleReceiveCountFromPersisted(t *testing.T) {
	const (
		bindingID = "b1"
		sessionID = "e5-sess"
	)

	store := NewFakeOutboxStore()
	receiver := NewFakeReceiver()

	bindings := []routing.DestinationBinding{{ID: bindingID, SessionID: sessionID}}
	rules, _ := runtime.CompileMatchRules([]runtime.MatchRule{{BindingID: bindingID}})
	resolver, err := runtime.NewRuleResolver(bindings, rules, "")
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}

	runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
		RouteID:     "e5-outbox-route",
		Policy:      routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox, MaxOutboxDepth: 100}.WithDefaults(),
		Receiver:    receiver,
		Sender:      NewFakeSender(),
		OutboxStore: store,
		Resolver:    resolver,
		Bindings:    bindings,
		InstanceID:  "bridge-B",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	<-receiver.Ready()

	// Source envelope arrives at bridge B carrying bridge A's stale upstream
	// delivery count plus a benign app header that must be persisted.
	src := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e5-outbox",
		Subject: "s",
		Payload: []byte("p"),
		Headers: map[string]any{asbDeliveryCountHeader: 7, "x-app.keep": "yes"},
	})
	del := NewFakeDelivery(src)

	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitFor(t, time.Second, "acked", func() bool { return del.IsAcked() })

	// Locate the persisted record for this envelope.
	var rec *persistence.OutboxRecord
	for _, r := range store.Records() {
		if r.EnvelopeID() == "e5-outbox" {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatal("no outbox record persisted for envelope e5-outbox")
	}
	snap := rec.Snapshot()

	// WHY: a drained record forwarded to the next hop must not carry the stale
	// upstream count, or the downstream bridge would misread it (E5-FU1).
	if _, present := snap.Headers()[asbDeliveryCountHeader]; present {
		t.Errorf("persisted envelope still carries %q; stale upstream count must be stripped", asbDeliveryCountHeader)
	}
	// WHY: the strip is surgical — benign application headers are persisted.
	if v, ok := snap.Headers()["x-app.keep"]; !ok || v != "yes" {
		t.Errorf("persisted x-app.keep = %v (present=%v), want \"yes\"", v, ok)
	}

	// WHY: the SOURCE envelope must be untouched — receiveCount(env) is re-read
	// from it on the retry/poison paths.
	if got, ok := del.Envelope().Headers()[asbDeliveryCountHeader]; !ok || got != 7 {
		t.Errorf("source %q = %v (present=%v), want 7 (source must not be mutated)", asbDeliveryCountHeader, got, ok)
	}
}
