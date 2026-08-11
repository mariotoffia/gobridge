package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// mqttSourceIdentity drives the production ingress conversion — the same
// publishWithIdentity/EnvelopeFromPublish pair the router runs — over publishes
// that carry the given producer identity. A broker redelivery of one publish
// repeats the identical properties, so `same` models redelivery and `distinct`
// models separate messages from the same source.
func mqttSourceIdentity(same func(*pahov5.PublishProperties), distinct func(int, *pahov5.PublishProperties)) transporttest.SourceIdentity {
	convert := func(apply func(*pahov5.PublishProperties)) *messaging.Envelope {
		properties := &pahov5.PublishProperties{}
		apply(properties)
		return EnvelopeFromPublish(publishWithIdentity(&pahov5.Publish{
			Topic:      "sensors/1",
			Payload:    []byte("p"),
			Properties: properties,
		}), nil)
	}
	return transporttest.SourceIdentity{
		Redeliver: func() *messaging.Envelope { return convert(same) },
		Distinct: func(n int) *messaging.Envelope {
			return convert(func(p *pahov5.PublishProperties) { distinct(n, p) })
		},
	}
}

// TestMQTTSourceIdentityConformance runs the ports.Receiver envelope-identity
// contract over each way an MQTT publish can carry — or lack — a producer
// identity: a message-id user property, textual Correlation Data, binary
// Correlation Data, and nothing at all (adapter-minted, declared generated).
func TestMQTTSourceIdentityConformance(t *testing.T) {
	t.Run("producer message-id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return mqttSourceIdentity(
				func(p *pahov5.PublishProperties) {
					p.User = pahov5.UserProperties{{Key: HeaderMessageID, Value: "producer-1"}}
				},
				func(n int, p *pahov5.PublishProperties) {
					p.User = pahov5.UserProperties{{Key: HeaderMessageID, Value: string(rune('a' + n))}}
				},
			)
		})
	})

	t.Run("textual correlation data", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return mqttSourceIdentity(
				func(p *pahov5.PublishProperties) { p.CorrelationData = []byte("corr-1") },
				func(n int, p *pahov5.PublishProperties) { p.CorrelationData = []byte{'c', byte('a' + n)} },
			)
		})
	})

	t.Run("binary correlation data", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return mqttSourceIdentity(
				func(p *pahov5.PublishProperties) { p.CorrelationData = []byte{0x00, 0xff, 'a'} },
				func(n int, p *pahov5.PublishProperties) { p.CorrelationData = []byte{0x00, 0xff, byte(n)} },
			)
		})
	})

	t.Run("no producer identity", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return mqttSourceIdentity(
				func(*pahov5.PublishProperties) {},
				func(int, *pahov5.PublishProperties) {},
			)
		})
	})
}
