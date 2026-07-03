// ═══════════════════════════════════════════════
// Production-readiness remediation tests: delayed-retry parity (F7).
//
// AMQP 0-9-1 has no client-side redelivery delay: Retry(after>0) nacks
// with IMMEDIATE requeue, so a poison message hot-loops on a classic
// queue unless the queue carries an x-delivery-limit or a DLX. The
// amqp10 transport already surfaced this (metric + Warn); amqp091 only
// had a Debug log. These tests pin the parity fix:
//
//   - MetricAMQP091DelayedRetryUnhonored counts EVERY unhonored delay,
//   - the Warn fires ONCE per consumer channel (shared delayWarnOnce),
//     and mentions the x-delivery-limit / DLX poison guard,
//   - the nack still requeues immediately (behavior unchanged).
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// warnCountHandler counts Warn-level records whose message matches a
// substring; all other slog handling is discarded. It is safe for
// concurrent use.
type warnCountHandler struct {
	substr string
	count  atomic.Int64
}

func (h *warnCountHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *warnCountHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && strings.Contains(r.Message, h.substr) {
		h.count.Add(1)
	}
	return nil
}

func (h *warnCountHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCountHandler) WithGroup(string) slog.Handler      { return h }

func newDelayedRetryDelivery(t *testing.T, tag uint64, logger *slog.Logger, metrics ports.MetricsExporter, once *sync.Once) (*Delivery, *mockAcknowledger) {
	t.Helper()
	acker := newMockAcknowledger()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "f7"})
	raw := amqp.Delivery{
		Acknowledger: acker,
		DeliveryTag:  tag,
		RoutingKey:   "orders.bridged",
	}
	d := NewDelivery(env, raw, logger, metrics, nil)
	d.delayWarnOnce = once
	return d, acker
}

// TestDelivery_Retry_Delayed_EmitsMetricAndWarnsOncePerChannel pins the
// F7 parity contract across two deliveries sharing one consumer channel.
func TestDelivery_Retry_Delayed_EmitsMetricAndWarnsOncePerChannel(t *testing.T) {
	handler := &warnCountHandler{substr: "delayed retry not honored"}
	logger := slog.New(handler)
	rec := &ports.RecordingExporter{}

	// Both deliveries share the same consumer channel's warn dedup, as
	// forwardDeliveries wires it in production.
	var channelOnce sync.Once

	var nacks []bool
	d1, a1 := newDelayedRetryDelivery(t, 1, logger, rec, &channelOnce)
	a1.NackFn = func(_ uint64, multiple, requeue bool) error {
		require.False(t, multiple)
		nacks = append(nacks, requeue)
		return nil
	}
	d2, a2 := newDelayedRetryDelivery(t, 2, logger, rec, &channelOnce)
	a2.NackFn = a1.NackFn

	require.NoError(t, d1.Retry(context.Background(), 30*time.Second, nil))
	require.NoError(t, d2.Retry(context.Background(), 30*time.Second, nil))

	// The broker-side behavior is unchanged: immediate requeue.
	require.Equal(t, []bool{true, true}, nacks)

	// Every unhonored delay is metered ...
	require.Len(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored), 2)
	// ... but the Warn is deduplicated per consumer channel.
	require.EqualValues(t, 1, handler.count.Load(),
		"warn must fire once per consumer channel, not once per message")
}

// TestDelivery_Retry_Immediate_NoDelayedRetrySignal proves the metric and
// Warn only fire for after>0 — an immediate retry is fully honored.
func TestDelivery_Retry_Immediate_NoDelayedRetrySignal(t *testing.T) {
	handler := &warnCountHandler{substr: "delayed retry not honored"}
	logger := slog.New(handler)
	rec := &ports.RecordingExporter{}

	d, _ := newDelayedRetryDelivery(t, 3, logger, rec, &sync.Once{})
	require.NoError(t, d.Retry(context.Background(), 0, nil))

	require.Empty(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored))
	require.EqualValues(t, 0, handler.count.Load())
}

// TestDelivery_Retry_Delayed_NilOnce_WarnsEachCall pins the nil-guard:
// directly-constructed deliveries (no consume forwarder) warn per call
// rather than panicking or staying silent.
func TestDelivery_Retry_Delayed_NilOnce_WarnsEachCall(t *testing.T) {
	handler := &warnCountHandler{substr: "delayed retry not honored"}
	logger := slog.New(handler)
	rec := &ports.RecordingExporter{}

	d1, _ := newDelayedRetryDelivery(t, 4, logger, rec, nil)
	d2, _ := newDelayedRetryDelivery(t, 5, logger, rec, nil)
	require.NoError(t, d1.Retry(context.Background(), time.Second, nil))
	require.NoError(t, d2.Retry(context.Background(), time.Second, nil))

	require.Len(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored), 2)
	require.EqualValues(t, 2, handler.count.Load())
}
