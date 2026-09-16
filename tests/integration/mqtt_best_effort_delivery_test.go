package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func bestEffortMetric(t *testing.T, rec *ports.RecordingExporter, name string, want int64, reason string) {
	t.Helper()
	var count int64
	for _, entry := range rec.FindEntries(name) {
		count += entry.IValue
		if reason != "" {
			assert.Contains(t, entry.Tags, shared.Tag{Key: shared.TagKeyReason, Value: reason})
		}
	}
	assert.Equal(t, want, count, "%s", name)
}

func bestEffortEnvelope(id string, generated bool, qos byte) *messaging.Envelope {
	pub := &pahov5.Publish{Topic: "readings/one", Payload: []byte(id), QoS: qos}
	if !generated {
		pub.Properties = &pahov5.PublishProperties{
			User: pahov5.UserProperties{{Key: paho.HeaderMessageID, Value: id}},
		}
	}
	return paho.EnvelopeFromPublish(pub, clocktest.NewAt(time.Unix(1, 0)))
}

func TestMQTTBestEffortDelivery_TerminalOutcomes(t *testing.T) {
	for _, generated := range []bool{false, true} {
		for _, tc := range []struct {
			name      string
			sendError error
			store     bool
			storeFail bool
			drop      bool
		}{
			{name: "success"},
			{name: "DLQ", sendError: shared.ErrUnavailable, store: true},
			{name: "explicit drop", sendError: shared.ErrUnavailable, drop: true},
			{name: "failed DLQ", sendError: shared.ErrUnavailable, store: true, storeFail: true},
			{name: "permanent failed DLQ", sendError: shared.ErrInvalidPayload, store: true, storeFail: true},
		} {
			t.Run(fmt.Sprintf("generated=%t/%s", generated, tc.name), func(t *testing.T) {
				rec := &ports.RecordingExporter{}
				hook := &bestEffortHook{}
				clk := clocktest.NewAt(time.Unix(1, 0))
				store := &bestEffortDLQ{}
				storeFailure := errors.New("DLQ unavailable")
				if tc.storeFail {
					store.failure = storeFailure
				}
				var sink ports.DLQStore
				if tc.store {
					sink = store
				}
				policy := routing.RoutePolicy{
					DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 1, AllowRetryDrop: tc.drop,
				}
				if tc.drop {
					policy.OnPermanentFailure = routing.FailureDrop
					policy.OnExpired = routing.ExpiredDrop
				}
				rx := newFakeReceiver()
				var calls atomic.Int32
				global := make(chan struct{}, 1)
				runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
					RouteID: "readings", SourceTransport: "mqtt.paho", Policy: policy,
					Receiver: rx, GlobalSem: global, Clock: clk, Metrics: rec, Hook: hook,
					DLQ: dlq.NewFromConfig(dlq.Config{Store: sink, Clock: clk, Metrics: rec, WriteMaxAttempts: 1}),
					Sender: bestEffortSender(func(context.Context, ports.OutboundMessage) error {
						if calls.Add(1) == 1 {
							return tc.sendError
						}
						return nil
					}),
				})
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- runner.Run(ctx) }()
				t.Cleanup(func() {
					cancel()
					assert.ErrorIs(t, wait.RequireReceive(t, done, time.Second), context.Canceled)
				})
				first := paho.NewDelivery(bestEffortEnvelope("first", generated, 0))
				require.ErrorIs(t, first.Retry(ctx, 0, nil), shared.ErrNotSupported)
				require.NoError(t, rx.Emit(ctx, first), "asynchronous acceptance is not a receiver emit rejection")
				wait.Until(t, time.Second, "first delivery finished", func() bool { return runner.InFlight() == 0 })
				require.Len(t, hook.settled(), 1)
				assert.True(t, hook.settled()[0].Terminal)
				require.NoError(t, first.Retry(ctx, 0, nil), "terminal QoS 0 Ack must latch without recovery")
				assert.Empty(t, rec.FindEntries(paho.MetricMQTTReceiverEmitRejected))
				assert.Empty(t, rec.FindEntries(paho.MetricMQTTSessionRecoveryRecycle))
				var dropped, deadLettered, sent int64
				reason := ""
				switch {
				case tc.sendError == nil:
					sent = 1
				case tc.storeFail:
					dropped, reason = 1, "retry_unsupported_dlq_failed"
					assert.ErrorIs(t, hook.settled()[0].Err, storeFailure)
				case tc.store:
					deadLettered = 1
				default:
					dropped, reason = 1, "retry_unsupported"
					if generated {
						reason = "unstable_identity"
					}
				}
				bestEffortMetric(t, rec, shared.MetricMessagesDropped, dropped, reason)
				bestEffortMetric(t, rec, shared.MetricDLQEntries, deadLettered, "")
				bestEffortMetric(t, rec, shared.MetricMessagesSent, sent, "")
				if tc.storeFail {
					wantWrites := int32(1)
					if generated || tc.sendError == shared.ErrInvalidPayload {
						wantWrites = 2
					}
					assert.Equal(t, wantWrites, store.writes.Load())
					bestEffortMetric(t, rec, shared.MetricDLQWriteFailures, int64(wantWrites), "")
				}
				if deadLettered != 0 {
					category := "retry_unsupported"
					if generated {
						category = "unstable_identity"
					}
					assert.Contains(t, rec.FindEntries(shared.MetricDLQEntries)[0].Tags,
						shared.Tag{Key: shared.TagKeyCategory, Value: category})
				}
				require.Empty(t, global)
				require.NoError(t, rx.Emit(ctx, paho.NewDelivery(bestEffortEnvelope("following", false, 0))))
				wait.Until(t, time.Second, "following delivery finished", func() bool { return runner.InFlight() == 0 })
				assert.EqualValues(t, 2, calls.Load())
				bestEffortMetric(t, rec, shared.MetricMessagesSent, sent+1, "")
				assert.Len(t, hook.settled(), 2)
				assert.Empty(t, global)
			})
		}
	}
}

func TestMQTTBestEffortDelivery_DurableRetryPreserved(t *testing.T) {
	for _, qos := range []byte{1, 2} {
		for _, generated := range []bool{false, true} {
			t.Run(fmt.Sprintf("qos=%d/generated=%t", qos, generated), func(t *testing.T) {
				clk := clocktest.NewAt(time.Unix(1, 0))
				rec := &ports.RecordingExporter{}
				hook := &bestEffortHook{}
				store := &bestEffortDLQ{failure: shared.ErrUnavailable}
				var acks, retries atomic.Int32
				del := paho.NewDelivery(bestEffortEnvelope("durable", generated, qos),
					paho.WithAckFunc(func() error { acks.Add(1); return nil }),
					paho.WithRetryFunc(func(context.Context) error { retries.Add(1); return nil }))
				runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
					RouteID: "durable", SourceTransport: "mqtt.paho", Clock: clk, Metrics: rec, Hook: hook,
					Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
					DLQ:    dlq.NewFromConfig(dlq.Config{Store: store, Clock: clk, Metrics: rec, WriteMaxAttempts: 1}),
					Sender: bestEffortSender(func(context.Context, ports.OutboundMessage) error { return shared.ErrInvalidPayload }),
				})
				require.NoError(t, runner.HandleDelivery(t.Context(), del))
				assert.EqualValues(t, 1, retries.Load())
				assert.Zero(t, acks.Load())
				assert.Empty(t, hook.settled())
				bestEffortMetric(t, rec, shared.MetricMessagesDropped, 0, "")
				bestEffortMetric(t, rec, shared.MetricDLQEntries, 0, "")
			})
		}
	}
}

func TestMQTTBestEffortDelivery_CancellationIsNotLoss(t *testing.T) {
	for _, generated := range []bool{false, true} {
		for _, phase := range []string{"before", "send", "retry", "DLQ"} {
			t.Run(fmt.Sprintf("generated=%t/%s", generated, phase), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				rec := &ports.RecordingExporter{}
				hook := &bestEffortHook{}
				clk := clocktest.NewAt(time.Unix(1, 0))
				store := &bestEffortDLQ{failure: shared.ErrUnavailable}
				if phase == "DLQ" {
					store.before = cancel
				}
				var acks atomic.Int32
				opts := []paho.DeliveryOption{paho.WithAckFunc(func() error { acks.Add(1); return nil })}
				if phase == "retry" {
					opts = append(opts, paho.WithRetryFunc(func(context.Context) error {
						cancel()
						return shared.ErrNotSupported
					}))
				}
				del := paho.NewDelivery(bestEffortEnvelope("canceled", generated, 0), opts...)
				runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
					RouteID: "canceled", SourceTransport: "mqtt.paho", Clock: clk, Metrics: rec, Hook: hook,
					Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
					DLQ:    dlq.NewFromConfig(dlq.Config{Store: store, Clock: clk, Metrics: rec, WriteMaxAttempts: 1}),
					Sender: bestEffortSender(func(context.Context, ports.OutboundMessage) error {
						if phase == "send" {
							cancel()
							return ctx.Err()
						}
						return shared.ErrUnavailable
					}),
				})
				if phase == "before" {
					cancel()
				}
				require.ErrorIs(t, runner.HandleDelivery(ctx, del), context.Canceled)
				assert.Zero(t, acks.Load())
				assert.Empty(t, hook.settled())
				assert.Zero(t, runner.InFlight())
				bestEffortMetric(t, rec, shared.MetricMessagesDropped, 0, "")
				bestEffortMetric(t, rec, shared.MetricMessagesSent, 0, "")
				bestEffortMetric(t, rec, shared.MetricDLQEntries, 0, "")
			})
		}
	}
}
