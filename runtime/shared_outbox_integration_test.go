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
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestRuntime(instanceID string, outbox *FakeOutboxStore, lease *FakeLeaseStore, dlq *FakeDLQStore) *goruntime.Runtime {
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
	return goruntime.New(opts...)
}

func fastSessionConfig(sessionID string) goruntime.SessionConfig {
	cfg := goruntime.DefaultSessionConfig(sessionID, true)
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
	session := NewFakeSession()

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

	if err := rt.AddRoute(cfg, receiver, sender, session, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	// Wait for lease acquisition and session reconciliation.
	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	env := &messaging.Envelope{
		ID:      "msg-basic-1",
		Payload: []byte("hello"),
	}
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
	if sent[0].Subject != "devices/1/state" {
		t.Errorf("expected subject devices/1/state, got %q", sent[0].Subject)
	}

	// Outbox record should be completed.
	waitFor(t, time.Second, "outbox completed", func() bool {
		return outbox.CompletedCount() >= 1
	})
}

// verifies route processors run in order and their mutations appear on envelopes the drainer sends.
func TestSharedOutbox_ProcessorChainRuns(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()

	rt := newTestRuntime("bridge-proc", outbox, lease, nil)

	receiver := NewFakeReceiver()
	sender := NewFakeSender()
	session := NewFakeSession()

	sessCfg := fastSessionConfig("mqtt-sess-proc")

	enricher := &FakeProcessor{
		NameVal: "enricher",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			env.Headers["enriched"] = true
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

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	env := &messaging.Envelope{ID: "msg-proc-1", Payload: []byte("data")}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "message sent", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if v, ok := sent[0].Headers["enriched"]; !ok || v != true {
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
	session := NewFakeSession()

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

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	env := &messaging.Envelope{ID: "msg-corr-1", Payload: []byte("x")}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "message sent", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if _, ok := sent[0].Headers[messaging.HeaderCorrelationID]; !ok {
		t.Error("expected correlation ID header")
	}
	if _, ok := sent[0].Headers[messaging.HeaderRouteID]; !ok {
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
	session := NewFakeSession()
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

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "session started", func() bool {
		return session.IsStarted()
	})

	env := &messaging.Envelope{
		ID:      "msg-hdr-1",
		Payload: []byte("x"),
		Headers: map[string]any{
			"x-bridge.spoofed": "evil",
			"safe-header":      "ok",
		},
	}
	del := NewFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 2*time.Second, "message sent", func() bool {
		return sender.SentCount() >= 1
	})

	sent := sender.GetSent()
	if _, ok := sent[0].Headers["x-bridge.spoofed"]; ok {
		t.Error("reserved header should have been stripped")
	}
	if v, ok := sent[0].Headers["safe-header"]; !ok || v != "ok" {
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

// TrackingSender wraps a FakeSender and records which session's sender
// was called, for multi-session fan-out assertions.
type TrackingSender struct {
	Tag string
	FakeSender
	mu      sync.Mutex
	SentIDs []string
}

func NewTrackingSender(tag string) *TrackingSender {
	return &TrackingSender{Tag: tag}
}

func (s *TrackingSender) Send(ctx context.Context, env *messaging.Envelope) error {
	s.mu.Lock()
	s.SentIDs = append(s.SentIDs, env.ID)
	s.mu.Unlock()
	return s.FakeSender.Send(ctx, env)
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
		env := &messaging.Envelope{
			ID:      fmt.Sprintf("%s-%d", prefix, i),
			Payload: []byte(fmt.Sprintf("payload-%d", i)),
		}
		del := NewFakeDelivery(env)
		dels[i] = del
		if err := receiver.Emit(ctx, del); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	return dels
}
