package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestRuntime(instanceID string, outbox *FakeOutboxStore, lease *FakeLeaseStore, dlq *FakeDLQStore, extra ...goruntime.Option) *goruntime.Runtime {
	opts := []goruntime.Option{
		goruntime.WithInstanceID(instanceID),
	}
	if outbox != nil {
		opts = append(opts, goruntime.WithOutboxStore(outbox))
	}
	if lease != nil {
		opts = append(opts, goruntime.WithLeaseStore(lease))
	}
	if dlq != nil {
		opts = append(opts, goruntime.WithDLQStore(dlq))
	} else {
		opts = append(opts, goruntime.WithDLQStore(NewFakeDLQStore()))
	}
	opts = append(opts, extra...)
	return goruntime.New(opts...)
}

func fastSessionConfig(sessionID string) session.Config {
	cfg := session.DefaultConfig(sessionID, true)
	cfg.LeaseTTL = 500 * time.Millisecond
	cfg.RenewInterval = 80 * time.Millisecond
	cfg.RenewJitter = 10 * time.Millisecond
	cfg.StepDownGrace = 100 * time.Millisecond
	cfg.DrainStrategy = persistence.NewFixedPoll(30 * time.Millisecond)
	cfg.DrainBatchSize = 50
	return cfg
}

// ---------------------------------------------------------------------------
// Basic shared outbox flow tests
// ---------------------------------------------------------------------------

// verifies end-to-end shared outbox flow: outbox persist after receive, source ack,
// drainer send to the resolved subject, and outbox completion.
func TestSharedOutbox_BasicFlow(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-basic", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-basic")

	cfg := goruntime.RouteConfig{
		ID: "basic-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "binding-1", Address: "devices/1/state"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-1", SessionID: "mqtt-sess-basic"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	// Wait for lease acquisition and sess reconciliation.
	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-basic-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Source should be acked after outbox persist.
	waitFor(t, time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})

	if outbox.RecordCount() == 0 {
		t.Fatal("expected outbox records after persist")
	}

	// Drainer should pick up the record and send it.
	waitFor(t, 2*time.Second, "message sent via drainer", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if sent[0].Subject() != "device.state.update" {
		t.Errorf("expected logical subject device.state.update preserved, got %q", sent[0].Subject())
	}
	outbound := sender.GetOutbound()
	if len(outbound) == 0 || outbound[0].Address != "devices/1/state" {
		t.Errorf("expected OutboundMessage.Address devices/1/state, got %+v", outbound)
	}

	// Outbox record should be completed.
	waitFor(t, time.Second, "outbox completed", func() bool {
		return outbox.CompletedCount() >= 1
	})
}

// TestSharedOutbox_BindingInheritsRouteSession is the regression test:
// a shared_outbox binding that omits its own SessionID must inherit the route
// session so the outbox record persists under the same SESSION#<id> partition
// the single drainer polls. Without the inheritance the record is orphaned and
// never drains, even though the source was acked after persist (silent loss).
func TestSharedOutbox_BindingInheritsRouteSession(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-a1", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-a1")

	cfg := goruntime.RouteConfig{
		ID: "a1-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "binding-1", Address: "devices/1/state"},
			},
		},
		Bindings: []routing.DestinationBinding{
			// SessionID intentionally omitted: it must inherit the route session.
			{ID: "binding-1"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-a1-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	waitFor(t, time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})

	// The record drains through the route session's sender only when the
	// binding inherited the route SessionID; otherwise it is orphaned.
	waitFor(t, 2*time.Second, "message drained via inherited session", func() bool {
		return sender.SentCount() >= 1
	})

	waitFor(t, time.Second, "outbox completed", func() bool {
		return outbox.CompletedCount() >= 1
	})
}

// TestSharedOutbox_OrphanBindingPlan_NotAckedOrLost is the regression test for
// the dispatch-time orphan guard. When a Resolver emits a plan whose BindingID
// is absent from the configured bindings, the binding resolves to no session;
// the outbox record would persist under a BINDING#<id> partition that no drainer
// ever polls, and the source would be ACKed after persist — silent loss. The
// guard must fail the delivery (retry/DLQ) before any persist+ack so the source
// message is preserved. This vector is not statically decidable (the Resolver is
// a runtime function), so the static validator cannot catch it.
func TestSharedOutbox_OrphanBindingPlan_NotAckedOrLost(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-orphan", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-orphan")

	cfg := goruntime.RouteConfig{
		ID: "orphan-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			// "ghost-binding" is NOT among the configured bindings, so it
			// resolves to no session at dispatch time.
			Plans: []routing.DispatchPlan{
				{BindingID: "ghost-binding", Address: "devices/1/state"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-1", SessionID: "mqtt-sess-orphan"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-orphan-1",
		Subject: "device.state.update",
		Payload: []byte("hello"),
	})
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// The orphan guard must fail the delivery (retry), never persist+ack.
	waitFor(t, time.Second, "delivery retried", func() bool {
		return del.IsRetried()
	})

	if del.IsAcked() {
		t.Fatal("source ACKed for an orphan dispatch plan — silent message loss")
	}
	if outbox.RecordCount() != 0 {
		t.Fatalf("orphan record persisted under a BINDING# partition no drainer polls: %d record(s)", outbox.RecordCount())
	}
}

// TestSharedOutbox_DrainPreservesLogicalSubject is the acceptance test for
// the drainer must dispatch using OutboxRecord.Address as the destination
// while leaving the persisted envelope's logical Subject untouched.
func TestSharedOutbox_DrainPreservesLogicalSubject(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-t04", outbox, lease, dlq)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-t04")

	cfg := goruntime.RouteConfig{
		ID: "t04-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "binding-1", Address: "topics/users/created"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "binding-1", SessionID: "mqtt-sess-t04"},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-t04-1",
		Subject: "evt.user.created",
		Payload: []byte("user-payload"),
	})
	del := NewFakeDelivery(env)
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Wait for outbox persistence (delivery is acked after persist).
	waitFor(t, time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})
	waitFor(t, time.Second, "record persisted", func() bool {
		return outbox.RecordCount() >= 1
	})

	// The record must store the logical Subject and the destination Address
	// separately — the drainer must NOT mutate Envelope.Subject to Address.
	recs := outbox.Records()
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 outbox record, got %d", len(recs))
	}
	if got := recs[0].Snapshot().Subject(); got != "evt.user.created" {
		t.Errorf("outbox record Envelope.Subject = %q, want %q (logical subject must be preserved)",
			got, "evt.user.created")
	}
	if got := recs[0].Address(); got != "topics/users/created" {
		t.Errorf("outbox record Address = %q, want %q", got, "topics/users/created")
	}

	// Drainer should send it.
	waitFor(t, 2*time.Second, "message sent via drainer", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if sent[0].Subject() != "evt.user.created" {
		t.Errorf("sender saw Envelope.Subject = %q, want %q (logical subject must be preserved on outbound)",
			sent[0].Subject(), "evt.user.created")
	}

	outbound := sender.GetOutbound()
	if len(outbound) == 0 {
		t.Fatal("expected at least one OutboundMessage observed by sender")
	}
	if outbound[0].Address != "topics/users/created" {
		t.Errorf("OutboundMessage.Address = %q, want %q (destination must travel via Address)",
			outbound[0].Address, "topics/users/created")
	}
}

// verifies route processors run in order and their mutations appear on envelopes the drainer sends.
func TestSharedOutbox_ProcessorChainRuns(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-proc", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-proc")

	enricher := &FakeProcessor{
		NameVal: "enricher",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			env.SetHeader("enriched", true)
			return next(ctx, env)
		},
	}

	cfg := goruntime.RouteConfig{
		ID: "proc-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Processors: []ports.Processor{enricher},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "b1", Address: "topic/proc"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-sess-proc"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-proc-1", Payload: []byte("data")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "message sent", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if v, ok := sent[0].Headers()["enriched"]; !ok || v != true {
		t.Error("expected enriched header on sent envelope")
	}
	if enricher.CalledCount() != 1 {
		t.Errorf("expected processor called once, got %d", enricher.CalledCount())
	}
}

// verifies correlation ID and route ID headers are injected on outbound envelopes.
func TestSharedOutbox_CorrelationIDInjected(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-corr", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-corr")

	cfg := goruntime.RouteConfig{
		ID: "corr-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-sess-corr"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-corr-1", Payload: []byte("x")})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "message sent", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if _, ok := sent[0].Headers()[messaging.HeaderCorrelationID]; !ok {
		t.Error("expected correlation ID header")
	}
	if _, ok := sent[0].Headers()[messaging.HeaderRouteID]; !ok {
		t.Error("expected route ID header")
	}
}

// verifies reserved x-bridge ingress headers are stripped and non-reserved headers are preserved on send.
func TestSharedOutbox_ReservedHeadersStripped(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-hdr", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-sess-hdr")

	cfg := goruntime.RouteConfig{
		ID: "hdr-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-sess-hdr"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, sess, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "msg-hdr-1",
		Payload: []byte("x"),
		Headers: map[string]any{
			"x-bridge.spoofed": "evil",
			"safe-header":      "ok",
		},
	})
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "message sent", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if _, ok := sent[0].Headers()["x-bridge.spoofed"]; ok {
		t.Error("reserved header should have been stripped")
	}
	if v, ok := sent[0].Headers()["safe-header"]; !ok || v != "ok" {
		t.Error("safe header should be preserved")
	}
}

// ---------------------------------------------------------------------------
// Helper extensions for thread-safe assertions
// ---------------------------------------------------------------------------

func (s *FakeSession) IsStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Started
}

// TrackingSender wraps a FakeSender and records which sess's sender
// was called, for multi-sess fan-out assertions.
type TrackingSender struct {
	Tag string
	FakeSender
	mu      sync.Mutex
	SentIDs []string
}

func NewTrackingSender(tag string) *TrackingSender {
	return &TrackingSender{Tag: tag}
}

func (s *TrackingSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	s.mu.Lock()
	s.SentIDs = append(s.SentIDs, env.ID())
	s.mu.Unlock()
	return s.FakeSender.Send(ctx, msg)
}

func (s *TrackingSender) SentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.SentIDs)
}

// ConcurrentCounter is a thread-safe counter for test synchronization.
type ConcurrentCounter struct {
	mu sync.Mutex
	n  int
}

func (c *ConcurrentCounter) Inc()     { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *ConcurrentCounter) Val() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// emitMessages sends n envelopes through the receiver and returns their deliveries.
func emitMessages(t *testing.T, ctx context.Context, receiver *FakeReceiver, prefix string, n int) []*FakeDelivery {
	t.Helper()
	dels := make([]*FakeDelivery, n)
	for i := 0; i < n; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("%s-%d", prefix, i),
			Payload: []byte(fmt.Sprintf("payload-%d", i)),
		})
		del := NewFakeDelivery(env)
		dels[i] = del
		if err := receiver.Emit(ctx, del); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	return dels
}
