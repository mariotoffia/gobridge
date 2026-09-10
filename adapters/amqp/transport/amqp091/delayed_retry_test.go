// ═══════════════════════════════════════════════
// Production-readiness remediation tests: delayed-retry parity
// and poison-loop mitigation (c5-poison-loop).
//
// AMQP 0-9-1 has no native delayed redelivery. Retry(after>0) therefore
// honors the requested backoff CLIENT-SIDE: it holds the unacked delivery
// for `after` (via the injected clock, cancellable by ctx) BEFORE the
// nack/requeue, so a poison message is SPACED instead of hot-looping on a
// classic queue. These tests pin:
//
//   - MetricAMQP091DelayedRetryUnhonored counts EVERY delayed retry (the
//     broker cannot schedule the delay natively),
//   - the Warn fires ONCE per consumer channel (shared delayWarnOnce),
//     and mentions the x-delivery-limit / DLX poison guard,
//   - the requeue is HELD until the clock advances past `after` (the
//     hot-loop fix), then issued with requeue=true.
//
// All timing is driven by a clocktest.Fake — no real sleeps.
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

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
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

// awaitParkedRetry starts d.Retry(after) on a goroutine and blocks until the
// delivery has parked on the injected fake clock (its honored-delay timer is
// registered) but has NOT yet requeued. It returns the error channel; the
// caller advances fake past `after` to release the hold. `after` must be > 0.
func awaitParkedRetry(t *testing.T, d *Delivery, fake *clocktest.Fake, after time.Duration) <-chan error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- d.Retry(context.Background(), after, nil) }()
	require.Eventually(t, func() bool { return fake.TimerCount() >= 1 }, time.Second, time.Millisecond,
		"Retry must park on the injected clock before the requeue (honored client-side delay)")
	return errc
}

// TestDelivery_Retry_Delayed_EmitsMetricAndWarnsOncePerChannel pins the
// parity contract across two deliveries sharing one consumer channel.
// Serialized (one Retry at a time) so the shared recorder / warn dedup /
// nacks slice are mutated by a single goroutine — race-clean.
func TestDelivery_Retry_Delayed_EmitsMetricAndWarnsOncePerChannel(t *testing.T) {
	handler := &warnCountHandler{substr: "delayed retry honored client-side"}
	logger := slog.New(handler)
	rec := &ports.RecordingExporter{}
	fake := clocktest.New()

	// Both deliveries share the same consumer channel's warn dedup, as
	// forwardDeliveries wires it in production.
	var channelOnce sync.Once

	var nacks []bool
	nackFn := func(_ uint64, multiple, requeue bool) error {
		require.False(t, multiple)
		nacks = append(nacks, requeue)
		return nil
	}
	d1, a1 := newDelayedRetryDelivery(t, 1, logger, rec, &channelOnce)
	d1.clk = fake
	a1.NackFn = nackFn
	d2, a2 := newDelayedRetryDelivery(t, 2, logger, rec, &channelOnce)
	d2.clk = fake
	a2.NackFn = nackFn

	errc1 := awaitParkedRetry(t, d1, fake, 30*time.Second)
	fake.Advance(30 * time.Second)
	require.NoError(t, wait.RequireReceive(t, errc1, 2*time.Second))

	errc2 := awaitParkedRetry(t, d2, fake, 30*time.Second)
	fake.Advance(30 * time.Second)
	require.NoError(t, wait.RequireReceive(t, errc2, 2*time.Second))

	// The broker-side settlement is unchanged: requeue=true.
	require.Equal(t, []bool{true, true}, nacks)

	// Every delayed retry is metered ...
	require.Len(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored), 2)
	// ... but the Warn is deduplicated per consumer channel.
	require.EqualValues(t, 1, handler.count.Load(),
		"warn must fire once per consumer channel, not once per message")
}

// TestDelivery_Retry_Immediate_NoDelayedRetrySignal proves the metric and
// Warn only fire for after>0 — an immediate retry is fully honored and never
// parks on the clock.
func TestDelivery_Retry_Immediate_NoDelayedRetrySignal(t *testing.T) {
	handler := &warnCountHandler{substr: "delayed retry honored client-side"}
	logger := slog.New(handler)
	rec := &ports.RecordingExporter{}

	d, _ := newDelayedRetryDelivery(t, 3, logger, rec, &sync.Once{})
	require.NoError(t, d.Retry(context.Background(), 0, nil))

	require.Empty(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored))
	require.EqualValues(t, 0, handler.count.Load())
}

// TestDelivery_Retry_Delayed_HoldsRequeueUntilClockAdvances is the core
// poison-loop mutation catcher: it proves the requeue is HELD for the full
// requested delay instead of being issued immediately. On the unfixed code
// (immediate Nack, delay only logged) the nack fires before any clock advance
// — the "silent before advance" assertions below fail.
func TestDelivery_Retry_Delayed_HoldsRequeueUntilClockAdvances(t *testing.T) {
	rec := &ports.RecordingExporter{}
	fake := clocktest.New()

	d, acker := newDelayedRetryDelivery(t, 7, slog.Default(), rec, &sync.Once{})
	d.clk = fake

	// Observe the requeue via a channel so the assertions are synchronized
	// (no unsynchronized read of the mock counters across goroutines).
	nacked := make(chan bool, 1)
	acker.NackFn = func(_ uint64, _ bool, requeue bool) error {
		nacked <- requeue
		return nil
	}

	errc := awaitParkedRetry(t, d, fake, 5*time.Second)

	// Parked on the honored delay: no requeue yet, and Retry has not returned.
	select {
	case <-nacked:
		t.Fatal("requeue issued immediately — honored delay not enforced (poison hot-loop)")
	case err := <-errc:
		t.Fatalf("Retry returned before the honored delay elapsed: %v", err)
	default:
	}

	// Short of the deadline: still held.
	fake.Advance(4 * time.Second)
	select {
	case <-nacked:
		t.Fatal("requeue issued before the full delay elapsed")
	default:
	}

	// Crossing the deadline releases the requeue.
	fake.Advance(1 * time.Second)
	require.NoError(t, wait.RequireReceive(t, errc, 2*time.Second))
	require.True(t, wait.RequireReceive(t, nacked, 2*time.Second), "requeue must be true")
	require.Len(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored), 1)
}

// TestDelivery_Retry_Delayed_CtxCancelRequeuesImmediately proves the honored
// hold is cancellable: a cancelled ctx requeues at once so settlement never
// blocks shutdown, AND the one-shot timer is Stop()ped on that path so it does
// not linger until `after` elapses (resource-lifecycle hardening).
func TestDelivery_Retry_Delayed_CtxCancelRequeuesImmediately(t *testing.T) {
	rec := &ports.RecordingExporter{}
	fake := clocktest.New()

	d, acker := newDelayedRetryDelivery(t, 8, slog.Default(), rec, &sync.Once{})
	d.clk = fake
	nacked := make(chan bool, 1)
	acker.NackFn = func(_ uint64, _ bool, requeue bool) error {
		nacked <- requeue
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the hold must not block

	require.NoError(t, d.Retry(ctx, time.Hour, nil))
	require.True(t, wait.RequireReceive(t, nacked, 2*time.Second),
		"cancelled ctx must requeue immediately without waiting out the delay")
	// The delay is still metered even when cancelled short.
	require.Len(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored), 1)
	// Resource lifecycle: the ctx-cancel path must Stop() the one-shot timer so
	// it is released immediately rather than lingering for the full `after`.
	// Mutation: drop `t.Stop()` (or revert to clk.After) and the never-fired
	// hour-long timer stays registered → TimerCount() == 1 here.
	require.Zero(t, fake.TimerCount(),
		"the honored-delay timer must be stopped when the hold is cancelled")
}

// TestDelivery_Retry_Delayed_NilOnce_WarnsEachCall pins the nil-guard:
// directly-constructed deliveries (no consume forwarder) warn per call
// rather than panicking or staying silent. Serialized + fake-clock driven.
func TestDelivery_Retry_Delayed_NilOnce_WarnsEachCall(t *testing.T) {
	handler := &warnCountHandler{substr: "delayed retry honored client-side"}
	logger := slog.New(handler)
	rec := &ports.RecordingExporter{}
	fake := clocktest.New()

	d1, _ := newDelayedRetryDelivery(t, 4, logger, rec, nil)
	d1.clk = fake
	d2, _ := newDelayedRetryDelivery(t, 5, logger, rec, nil)
	d2.clk = fake

	errc1 := awaitParkedRetry(t, d1, fake, time.Second)
	fake.Advance(time.Second)
	require.NoError(t, wait.RequireReceive(t, errc1, 2*time.Second))

	errc2 := awaitParkedRetry(t, d2, fake, time.Second)
	fake.Advance(time.Second)
	require.NoError(t, wait.RequireReceive(t, errc2, 2*time.Second))

	require.Len(t, rec.FindEntries(MetricAMQP091DelayedRetryUnhonored), 2)
	require.EqualValues(t, 2, handler.count.Load())
}
