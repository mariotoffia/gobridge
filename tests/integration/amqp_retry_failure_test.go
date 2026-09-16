package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	amqp10sdk "github.com/Azure/go-amqp"
	amqp091sdk "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestAMQPRetryFailure_FailedDispositionDoesNotDrop(t *testing.T) {
	for _, transport := range []string{"amqp10", "amqp091"} {
		t.Run(transport, func(t *testing.T) {
			clk := clocktest.NewAt(time.Unix(1000, 0))
			env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "recoverable", Payload: []byte("message")})
			amqp10Failure := &amqp10sdk.Error{Condition: amqp10sdk.ErrCondNotImplemented, Description: "disposition refused"}
			amqp091Failure := &amqp091sdk.Error{Code: 540, Reason: "disposition refused"}
			settler := &failedAMQP10Settler{failure: amqp10Failure}
			acknowledger := &failedAMQP091Acknowledger{failure: amqp091Failure}
			delivery := &observedRetryDelivery{}
			var protocolFailure error = amqp10Failure
			if transport == "amqp10" {
				delivery.Delivery = amqp10.NewDelivery(env, &amqp10sdk.Message{}, settler, nil, nil, clk)
			} else {
				protocolFailure = amqp091Failure
				delivery.Delivery = amqp091.NewDelivery(env, amqp091sdk.Delivery{Acknowledger: acknowledger, DeliveryTag: 1}, nil, nil, clk)
			}
			rec := &ports.RecordingExporter{}
			hook := &bestEffortHook{}
			store := &bestEffortDLQ{failure: shared.ErrUnavailable}
			runner := route.NewRouteRunnerFromConfig(route.RouteRunnerConfig{
				RouteID: "recoverable", Clock: clk, Metrics: rec, Hook: hook,
				Policy: routing.RoutePolicy{
					DeliveryMode: routing.DeliveryDirectHold,
					Backoff: routing.BackoffPolicy{
						InitialInterval: time.Second, MaxInterval: time.Second,
						Multiplier: 1, JitterFactor: routing.JitterDisabled,
					},
				},
				DLQ: dlq.NewFromConfig(dlq.Config{Store: store, Clock: clk, Metrics: rec, WriteMaxAttempts: 1}),
				Sender: bestEffortSender(func(context.Context, ports.OutboundMessage) error {
					return shared.ErrInvalidPayload
				}),
			})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				done <- runner.HandleDelivery(ctx, delivery)
			}()
			t.Cleanup(func() {
				cancel()
				wait.RequireClosed(t, finished, time.Second)
			})
			if transport == "amqp091" {
				wait.Until(t, time.Second, "client-side retry delay armed", func() bool {
					return delivery.retries.Load() == 1 && clk.TimerCount() > 0
				})
				clk.Advance(time.Second)
			}
			err := wait.RequireReceive(t, done, time.Second)
			assert.ErrorIs(t, err, protocolFailure)
			assert.ErrorIs(t, err, shared.ErrProtocolError)
			assert.NotErrorIs(t, err, shared.ErrNotSupported)
			assert.Zero(t, delivery.acks.Load(), "failed protocol settlement must not invoke terminal Ack")
			assert.EqualValues(t, 1, delivery.retries.Load())
			assert.Empty(t, hook.settled())
			assert.Empty(t, rec.FindEntries(shared.MetricMessagesDropped))
			assert.Empty(t, rec.FindEntries(shared.MetricDLQEntries))
			assert.EqualValues(t, 1, store.writes.Load(), "the failed DLQ must not be retried as an unsupported operation")
			assert.Zero(t, settler.accepts.Load())
			assert.Zero(t, acknowledger.acks.Load())
			if transport == "amqp10" {
				assert.EqualValues(t, 1, settler.modifies.Load())
			} else {
				assert.EqualValues(t, 1, acknowledger.nacks.Load())
			}
			assert.ErrorIs(t, delivery.Delivery.Ack(t.Context()), shared.ErrUnavailable, "the broker-owned handle remains failed")
		})
	}
}

func TestAMQP10RetryFailure_ReleaseAndModifyPreserveProtocolError(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		for _, scoped := range []bool{false, true} {
			t.Run(fmt.Sprintf("delay=%s/scoped=%t", delay, scoped), func(t *testing.T) {
				protocolFailure := &amqp10sdk.Error{Condition: amqp10sdk.ErrCondNotImplemented, Description: "disposition refused"}
				var failure error = protocolFailure
				if scoped {
					failure = fmt.Errorf("settlement: %w", &amqp10sdk.LinkError{RemoteErr: protocolFailure})
				}
				settler := &failedAMQP10Settler{failure: failure}
				del := amqp10.NewDelivery(
					messaging.MustEnvelope(messaging.EnvelopeInput{ID: "unsettled"}),
					&amqp10sdk.Message{}, settler, nil, nil, clocktest.NewAt(time.Unix(1000, 0)))
				err := del.Retry(t.Context(), delay, shared.ErrUnavailable)
				require.ErrorIs(t, err, protocolFailure)
				assert.ErrorIs(t, err, shared.ErrProtocolError)
				assert.NotErrorIs(t, err, shared.ErrNotSupported)
				assert.ErrorIs(t, del.Ack(t.Context()), shared.ErrUnavailable)
				assert.Zero(t, settler.accepts.Load())
			})
		}
	}
}
