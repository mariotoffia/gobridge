// ═══════════════════════════════════════════════
// Production-readiness remediation test: metric tag cardinality (MEDIUM).
//
// The Ack/Retry delivery metrics used to tag TagKeyEntity with the AMQP
// RoutingKey, which is caller-controlled and unbounded (per-entity keys like
// "orders.evt.<uuid>") — a time-series explosion in Prometheus. The fix tags
// with the BOUNDED queue name the consumer reads from (mirroring the
// ConsumeLatency tag in receiver.go).
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestDelivery_MetricTag_UsesBoundedQueueName pins the cardinality fix: the
// Ack metric must be tagged with the bounded queue name the forwarder stamps
// on the delivery, NOT the unbounded routing key.
//
// Counterfactual (tag with d.raw.RoutingKey): the recorded tag is the dynamic
// routing key, the cardinality-explosion path.
func TestDelivery_MetricTag_UsesBoundedQueueName(t *testing.T) {
	const queue = "orders.inbound"
	const dynamicRK = "orders.evt.deadbeef-1234"

	ack := newMockAcknowledger()
	deliveries := make(chan amqp.Delivery, 1)
	deliveries <- amqp.Delivery{Acknowledger: ack, Body: []byte("x"), MessageId: "m1", RoutingKey: dynamicRK}
	close(deliveries)
	out := make(chan *Delivery, 1)
	rec := &ports.RecordingExporter{}

	done := make(chan struct{})
	go func() {
		forwardDeliveries(context.Background(), queue, deliveries, out, false, slog.Default(), rec, clock.System)
		close(done)
	}()

	d := wait.RequireReceive(t, out, time.Second)
	require.NotNil(t, d)
	require.NoError(t, d.Ack(context.Background()))
	wait.RequireClosed(t, done, time.Second)

	entries := rec.FindEntries(MetricAMQP091AckLatency)
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Tags, 1)
	require.Equal(t, shared.TagKeyEntity, entries[0].Tags[0].Key)
	require.Equal(t, queue, entries[0].Tags[0].Value,
		"Ack metric must be tagged with the bounded queue name")
	require.NotEqual(t, dynamicRK, entries[0].Tags[0].Value,
		"Ack metric tagged with the unbounded routing key (cardinality explosion)")
}
