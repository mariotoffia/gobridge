package servicebus

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// asbSourceIdentity drives the production ingress conversion over messages
// carrying the given MessageID. Peek-lock redelivery (abandon, lock expiry)
// preserves both MessageID and SequenceNumber, so converting an identical
// ReceivedMessage models redelivery.
func asbSourceIdentity(messageID func(n int) string, withSequence bool) transporttest.SourceIdentity {
	convert := func(n int) *messaging.Envelope {
		received := &azservicebus.ReceivedMessage{MessageID: messageID(n), Body: []byte("p")}
		if withSequence {
			sequence := int64(n + 1)
			received.SequenceNumber = &sequence
		}
		env, err := receivedToEnvelope(received, nil, "orders")
		if err != nil {
			panic("servicebus: conformance conversion failed: " + err.Error())
		}
		return env
	}
	return transporttest.SourceIdentity{
		Redeliver: func() *messaging.Envelope { return convert(0) },
		Distinct:  convert,
	}
}

// TestServiceBusSourceIdentityConformance runs the ports.Receiver
// envelope-identity contract over all three Service Bus ingress paths: the
// producer's MessageID, the sequence-number fallback used when a message
// carries none, and the last-resort random identity for a message with neither.
func TestServiceBusSourceIdentityConformance(t *testing.T) {
	t.Run("producer message id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return asbSourceIdentity(func(n int) string { return string(rune('a' + n)) }, true)
		})
	})

	t.Run("sequence-number fallback", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return asbSourceIdentity(func(int) string { return "" }, true)
		})
	})

	t.Run("no message id and no sequence number", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return asbSourceIdentity(func(int) string { return "" }, false)
		})
	})
}
