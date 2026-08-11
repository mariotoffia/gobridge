package amqp091

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// amqp091SourceIdentity drives the production ingress conversion over
// deliveries carrying the given MessageId. A requeued AMQP 0-9-1 delivery
// repeats the publisher's own properties, so converting an identical Delivery
// models redelivery.
func amqp091SourceIdentity(messageID func(n int) string) transporttest.SourceIdentity {
	convert := func(n int) *messaging.Envelope {
		env, err := deliveryToEnvelope(amqp.Delivery{
			MessageId:   messageID(n),
			RoutingKey:  "orders.created",
			Body:        []byte("p"),
			DeliveryTag: uint64(n + 1), //nolint:gosec // small test index, no overflow
		}, nil)
		if err != nil {
			panic("amqp091: conformance conversion failed: " + err.Error())
		}
		return env
	}
	return transporttest.SourceIdentity{
		Redeliver: func() *messaging.Envelope { return convert(0) },
		Distinct:  convert,
	}
}

// TestAMQP091SourceIdentityConformance runs the ports.Receiver envelope-identity
// contract over both AMQP 0-9-1 ingress paths: a publisher-supplied message-id,
// and a delivery with none — where the adapter mints and must declare that the
// identity does not survive a requeue.
func TestAMQP091SourceIdentityConformance(t *testing.T) {
	t.Run("publisher message-id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return amqp091SourceIdentity(func(n int) string { return string(rune('a' + n)) })
		})
	})

	t.Run("no publisher message-id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return amqp091SourceIdentity(func(int) string { return "" })
		})
	})
}
