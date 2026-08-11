package amqp10

import (
	"testing"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// amqp10SourceIdentity drives the production ingress conversion over messages
// carrying the given message-id. A released or modified AMQP 1.0 delivery is
// re-sent with the producer's own properties, so converting an identical
// Message models redelivery.
func amqp10SourceIdentity(messageID func(n int) any) transporttest.SourceIdentity {
	convert := func(n int) *messaging.Envelope {
		properties := &amqp.MessageProperties{}
		if id := messageID(n); id != nil {
			properties.MessageID = id
		}
		env, err := messageToEnvelope(&amqp.Message{
			Properties: properties,
			Data:       [][]byte{[]byte("p")},
		}, nil)
		if err != nil {
			panic("amqp10: conformance conversion failed: " + err.Error())
		}
		return env
	}
	return transporttest.SourceIdentity{
		Redeliver: func() *messaging.Envelope { return convert(0) },
		Distinct:  convert,
	}
}

// TestAMQP10SourceIdentityConformance runs the ports.Receiver envelope-identity
// contract over both AMQP 1.0 ingress paths: a producer-supplied message-id,
// and a message with none — where the adapter mints and must declare that the
// identity does not survive redelivery.
func TestAMQP10SourceIdentityConformance(t *testing.T) {
	t.Run("producer message-id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return amqp10SourceIdentity(func(n int) any { return string(rune('a' + n)) })
		})
	})

	t.Run("no producer message-id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return amqp10SourceIdentity(func(int) any { return nil })
		})
	})
}
