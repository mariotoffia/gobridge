package paho

import (
	"strconv"
	"testing"

	"github.com/eclipse/paho.golang/packets"
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
		}, false), nil)
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
// Correlation Data, nothing at all (adapter-minted, declared generated), and
// nothing on a session that derives the id from content.
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

	// On an MQTT 3.1.1 session set to message_id content_hash a publish carries
	// no producer identity, so the id is derived from what the broker redelivers
	// unchanged: the topic and the payload. A redelivery gets a new packet id
	// and DUP; a distinct message differs in its payload.
	t.Run("content hash", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			convert := func(packetID uint16, duplicate bool, payload string) *messaging.Envelope {
				return EnvelopeFromPublish(publishWithIdentity(inboundPublish(&packets.Publish{
					PacketID:  packetID,
					QoS:       1,
					Duplicate: duplicate,
					Topic:     "sensors/1",
					Payload:   []byte(payload),
				}), true), nil)
			}
			var redeliveries uint16
			return transporttest.SourceIdentity{
				Redeliver: func() *messaging.Envelope {
					redeliveries++
					return convert(redeliveries, redeliveries > 1, "p")
				},
				Distinct: func(n int) *messaging.Envelope {
					return convert(1, false, "p-"+strconv.Itoa(n))
				},
			}
		})
	})
}
