package paho

import (
	"sync"
	"testing"

	"github.com/eclipse/paho.golang/packets"
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

	t.Run("hostile marker beside a producer identity is stripped at ingress", func(t *testing.T) {
		// A publisher that supplies BOTH a stable mqtt.message-id and the
		// adapter-internal marker would otherwise have its own stable identity
		// classified as adapter-minted: the runtime then treats the message as an
		// uncountable redelivery and terminalizes (DLQ/drop) its FIRST transient
		// failure instead of retrying it.
		raw := &pahov5.Publish{
			Topic:   "t",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				User: []pahov5.UserProperty{
					{Key: HeaderMessageID, Value: "producer-stable-1"},
					{Key: headerMQTTGeneratedID, Value: "1"},
				},
			},
		}

		env := EnvelopeFromPublish(publishWithIdentity(raw), nil)

		if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID); ok {
			t.Fatalf("publisher-supplied %q set adapter-minted provenance on a stable producer identity", headerMQTTGeneratedID)
		}
		if env.ID() != "producer-stable-1" {
			t.Fatalf("envelope id = %q, want producer-stable-1", env.ID())
		}
	})

	t.Run("hostile marker without a producer identity does not survive ingress", func(t *testing.T) {
		// Ingress must own the marker end to end: whatever the publisher sent is
		// removed before the router mints, so the marker on the dispatched publish
		// is always the router's own.
		raw := &pahov5.Publish{
			Topic:   "t",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				User: []pahov5.UserProperty{{Key: headerMQTTGeneratedID, Value: "publisher"}},
			},
		}

		sanitized := publishWithIdentity(raw)

		markers := 0
		for _, u := range sanitized.Properties.User {
			if u.Key == headerMQTTGeneratedID {
				markers++
				if u.Value == "publisher" {
					t.Fatalf("publisher-supplied %q value survived ingress", headerMQTTGeneratedID)
				}
			}
		}
		if markers != 1 {
			t.Fatalf("got %d %q markers on the dispatched publish, want exactly the router's own", markers, headerMQTTGeneratedID)
		}
		if _, ok := messaging.GetHeaderString(EnvelopeFromPublish(sanitized, nil).Headers(), messaging.HeaderGeneratedID); !ok {
			t.Fatal("a publish with no producer identity must still be marked adapter-generated")
		}
	})

	t.Run("hostile marker does not mutate the broker's packet", func(t *testing.T) {
		// The Paho callback packet is shared with the SDK's acknowledgement
		// tracker until settlement; sanitising must copy, never mutate in place.
		raw := &pahov5.Publish{
			Topic:   "t",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				User: []pahov5.UserProperty{
					{Key: HeaderMessageID, Value: "producer-stable-1"},
					{Key: headerMQTTGeneratedID, Value: "1"},
				},
			},
		}

		_ = publishWithIdentity(raw)

		if len(raw.Properties.User) != 2 || raw.Properties.User[1].Key != headerMQTTGeneratedID {
			t.Fatalf("ingress sanitising mutated the SDK-owned packet: %+v", raw.Properties.User)
		}
	})

	t.Run("hostile marker cannot reach a handler through the router", func(t *testing.T) {
		// End-to-end through the two production ingress entry points, so the
		// guarantee is pinned at the wiring rather than at the helper.
		hostile := func() *pahov5.Publish {
			return &pahov5.Publish{
				Topic:   "identity/hostile",
				Payload: []byte("p"),
				Properties: &pahov5.PublishProperties{
					User: []pahov5.UserProperty{
						{Key: HeaderMessageID, Value: "producer-stable-1"},
						{Key: headerMQTTGeneratedID, Value: "1"},
					},
				},
			}
		}

		r := newRouter(nil, nil)
		var mu sync.Mutex
		var delivered []*messaging.Envelope
		r.RegisterEnvelope("hostile-provenance", nil, nil, func(env *messaging.Envelope, _ func() error) {
			mu.Lock()
			delivered = append(delivered, env)
			mu.Unlock()
		})

		handled, err := r.onPublishReceived(pahov5.PublishReceived{Packet: hostile()})
		if err != nil || !handled {
			t.Fatalf("onPublishReceived = handled:%v err:%v", handled, err)
		}
		r.Route(&packets.Publish{
			Topic:   "identity/hostile",
			Payload: []byte("p"),
			Properties: &packets.Properties{User: []packets.User{
				{Key: HeaderMessageID, Value: "producer-stable-1"},
				{Key: headerMQTTGeneratedID, Value: "1"},
			}},
		})
		r.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(delivered) != 2 {
			t.Fatalf("delivered %d envelopes, want 2 (one per ingress entry point)", len(delivered))
		}
		for i, env := range delivered {
			if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID); ok {
				t.Fatalf("envelope %d: publisher-supplied provenance survived the router", i)
			}
			if env.ID() != "producer-stable-1" {
				t.Fatalf("envelope %d: id = %q, want producer-stable-1", i, env.ID())
			}
		}
	})

	t.Run("marker does not ride egress", func(t *testing.T) {
		env := EnvelopeFromPublish(publishWithIdentity(&pahov5.Publish{Topic: "t", Payload: []byte("p")}), nil)
		pub := mustPublishFromEnvelope(t, env, "out/topic", SenderOptions{}, nil)
		if pub.Properties != nil {
			for _, u := range pub.Properties.User {
				if u.Key == messaging.HeaderGeneratedID || u.Key == headerMQTTGeneratedID {
					t.Fatalf("generated-identity marker %q leaked to egress user properties", u.Key)
				}
			}
		}
	})
}
