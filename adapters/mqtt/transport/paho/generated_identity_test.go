package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestGeneratedIdentity_MarkedAndScoped proves marking:
//
//   - a publish WITHOUT a producer identity, run through publishWithIdentity (the
//     production ingress path), yields an envelope marked messaging.HeaderGeneratedID
//     so the runtime can terminate its uncountable replay loop;
//   - a publish WITH a real mqtt.message-id is NOT marked (its identity is stable);
//   - the marker never rides egress: PublishFromEnvelope drops it because it is
//     INTERNAL-ONLY, so a peer bridge never sees it.
func TestGeneratedIdentity_MarkedAndScoped(t *testing.T) {
	t.Run("count-less publish is marked generated", func(t *testing.T) {
		raw := &pahov5.Publish{Topic: "t", Payload: []byte("p")}
		env := EnvelopeFromPublish(publishWithIdentity(raw), nil)
		if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID); !ok {
			t.Fatalf("no-identity publish: HeaderGeneratedID not set; runtime cannot detect the uncountable case")
		}
		if env.ID() == "" {
			t.Fatalf("generated identity must still yield a non-empty envelope id")
		}
		// The adapter-internal marker property must be consumed, not surfaced as an
		// application header.
		if _, ok := env.Headers()[headerMQTTGeneratedID]; ok {
			t.Fatalf("adapter-internal marker %q leaked as an application header", headerMQTTGeneratedID)
		}
	})

	t.Run("producer identity is not marked", func(t *testing.T) {
		raw := &pahov5.Publish{
			Topic:   "t",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				User: []pahov5.UserProperty{{Key: HeaderMessageID, Value: "producer-stable-1"}},
			},
		}
		env := EnvelopeFromPublish(publishWithIdentity(raw), nil)
		if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID); ok {
			t.Fatalf("stable producer identity must NOT be marked generated")
		}
		if env.ID() != "producer-stable-1" {
			t.Fatalf("envelope id = %q, want producer-stable-1", env.ID())
		}
	})

	t.Run("marker does not ride egress", func(t *testing.T) {
		env := EnvelopeFromPublish(publishWithIdentity(&pahov5.Publish{Topic: "t", Payload: []byte("p")}), nil)
		pub := PublishFromEnvelope(env, "out/topic", SenderOptions{}, nil)
		if pub.Properties != nil {
			for _, u := range pub.Properties.User {
				if u.Key == messaging.HeaderGeneratedID || u.Key == headerMQTTGeneratedID {
					t.Fatalf("generated-identity marker %q leaked to egress user properties", u.Key)
				}
			}
		}
	})
}
