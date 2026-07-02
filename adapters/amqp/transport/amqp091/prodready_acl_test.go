// ═══════════════════════════════════════════════
// Production-readiness remediation tests: consume goroutine lifetime &
// topology arguments.
//
// Covers:
//   - Finding #7: the consume forwarding goroutine exits (and requeues
//     the in-flight delivery) on context cancellation instead of leaking.
//   - Finding #11: queue/exchange/binding arguments flow from typed config
//     into the declaration tables, enabling quorum queues, DLX, TTL, etc.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestForwardDeliveries_CtxCancel_NacksAndExits verifies the leak fix:
// with no reader on out, the forwarder blocks on the send; cancelling ctx
// must make it nack-requeue the in-flight delivery and return.
func TestForwardDeliveries_CtxCancel_NacksAndExits(t *testing.T) {
	ack := newMockAcknowledger()
	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- amqp.Delivery{Acknowledger: ack, Body: []byte("payload"), MessageId: "m1"}
	out := make(chan *Delivery) // unbuffered, never read

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		forwardDeliveries(ctx, deliveries, out, false, slog.Default(), &ports.NoopExporter{}, clock.System)
		close(done)
	}()

	cancel()
	wait.RequireClosed(t, done, 2*time.Second)

	if ack.NackCalls != 1 {
		t.Fatalf("NackCalls = %d, want 1 (in-flight delivery requeued on shutdown)", ack.NackCalls)
	}
}

// TestForwardDeliveries_AutoAck_NoNackOnCancel verifies that in auto-ack
// mode the forwarder does not attempt to nack on cancel (the broker
// already settled the delivery; nacking would error).
func TestForwardDeliveries_AutoAck_NoNackOnCancel(t *testing.T) {
	ack := newMockAcknowledger()
	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- amqp.Delivery{Acknowledger: ack, Body: []byte("payload"), MessageId: "m1"}
	out := make(chan *Delivery)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		forwardDeliveries(ctx, deliveries, out, true /*autoAck*/, slog.Default(), &ports.NoopExporter{}, clock.System)
		close(done)
	}()

	cancel()
	wait.RequireClosed(t, done, 2*time.Second)

	if ack.NackCalls != 0 {
		t.Fatalf("NackCalls = %d, want 0 in auto-ack mode", ack.NackCalls)
	}
}

// TestForwardDeliveries_HappyPath_ForwardsThenCloses verifies normal
// forwarding: each delivery is wrapped and emitted, and out is closed
// when the source closes (goroutine exits cleanly).
func TestForwardDeliveries_HappyPath_ForwardsThenCloses(t *testing.T) {
	deliveries := make(chan amqp.Delivery, 2)
	deliveries <- amqp.Delivery{Body: []byte("a"), MessageId: "1"}
	deliveries <- amqp.Delivery{Body: []byte("b"), MessageId: "2"}
	close(deliveries)
	out := make(chan *Delivery, 2)

	done := make(chan struct{})
	go func() {
		forwardDeliveries(context.Background(), deliveries, out, false, slog.Default(), &ports.NoopExporter{}, clock.System)
		close(done)
	}()

	if d := wait.RequireReceive(t, out, time.Second); d == nil {
		t.Fatal("first delivery was nil")
	}
	if d := wait.RequireReceive(t, out, time.Second); d == nil {
		t.Fatal("second delivery was nil")
	}
	wait.RequireClosed(t, out, time.Second)
	wait.RequireClosed(t, done, time.Second)
}

// TestToAMQPTable verifies the argument-table conversion: empty -> nil,
// scalars pass through, nested maps/slices recurse, and the result is a
// valid SDK table.
func TestToAMQPTable(t *testing.T) {
	if got := toAMQPTable(nil); got != nil {
		t.Fatalf("nil map -> %v, want nil Table", got)
	}
	if got := toAMQPTable(map[string]any{}); got != nil {
		t.Fatalf("empty map -> %v, want nil Table", got)
	}

	in := map[string]any{
		"x-queue-type":             "quorum",
		"x-message-ttl":            60000,
		"x-single-active-consumer": true,
		"nested":                   map[string]any{"inner": "v"},
		"list":                     []any{"a", 1},
	}
	got := toAMQPTable(in)

	if got["x-queue-type"] != "quorum" {
		t.Errorf("x-queue-type = %v", got["x-queue-type"])
	}
	if got["x-message-ttl"] != 60000 {
		t.Errorf("x-message-ttl = %v", got["x-message-ttl"])
	}
	if got["x-single-active-consumer"] != true {
		t.Errorf("x-single-active-consumer = %v", got["x-single-active-consumer"])
	}
	nested, ok := got["nested"].(amqp.Table)
	if !ok || nested["inner"] != "v" {
		t.Errorf("nested = %#v, want amqp.Table{inner:v}", got["nested"])
	}
	list, ok := got["list"].([]any)
	if !ok || len(list) != 2 {
		t.Errorf("list = %#v, want []any of len 2", got["list"])
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("produced table is not SDK-encodable: %v", err)
	}
}

// TestSubscriptionParams_Arguments verifies the per-subscription topology
// (including the new argument tables) is read from the typed config.
func TestSubscriptionParams_Arguments(t *testing.T) {
	cfg := Config{Subscription: SubscriptionParams{
		Exchange:          "ex",
		RoutingKey:        "rk",
		ExchangeType:      "topic",
		Durable:           true,
		QueueArguments:    map[string]any{"x-queue-type": "quorum"},
		ExchangeArguments: map[string]any{"alternate-exchange": "ae"},
		BindingArguments:  map[string]any{"x-match": "all"},
	}}
	decl := subscriptionParams(connectivity.SubscriptionPlan{Topic: "q", Config: cfg})

	if decl.exchange != "ex" || decl.routingKey != "rk" || decl.exchangeType != "topic" || !decl.durable {
		t.Fatalf("unexpected decl identity: %#v", decl)
	}
	if decl.queueArgs["x-queue-type"] != "quorum" {
		t.Errorf("queueArgs = %#v", decl.queueArgs)
	}
	if decl.exchangeArgs["alternate-exchange"] != "ae" {
		t.Errorf("exchangeArgs = %#v", decl.exchangeArgs)
	}
	if decl.bindArgs["x-match"] != "all" {
		t.Errorf("bindArgs = %#v", decl.bindArgs)
	}
}

// TestSubscriptionParams_Defaults verifies a plan with no config yields a
// sane default (direct exchange, nil argument tables).
func TestSubscriptionParams_Defaults(t *testing.T) {
	decl := subscriptionParams(connectivity.SubscriptionPlan{Topic: "q"})
	if decl.exchangeType != "direct" {
		t.Errorf("exchangeType = %q, want direct", decl.exchangeType)
	}
	if decl.queueArgs != nil || decl.exchangeArgs != nil || decl.bindArgs != nil {
		t.Errorf("expected nil argument tables, got %#v", decl)
	}
}

// TestPublisherParams_Arguments verifies the publisher topology and
// exchange arguments are read from the typed config.
func TestPublisherParams_Arguments(t *testing.T) {
	cfg := Config{Publisher: PublisherParams{
		ExchangeType:      "fanout",
		Durable:           true,
		ExchangeArguments: map[string]any{"alternate-exchange": "ae"},
	}}
	decl := publisherParams(connectivity.PublisherPlan{Topic: "ex", Config: cfg})

	if decl.exchangeType != "fanout" || !decl.durable {
		t.Fatalf("unexpected decl: %#v", decl)
	}
	if decl.exchangeArgs["alternate-exchange"] != "ae" {
		t.Errorf("exchangeArgs = %#v", decl.exchangeArgs)
	}
}
