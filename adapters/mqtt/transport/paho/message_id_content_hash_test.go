package paho

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// A session set to message_id content_hash names a publish that carries no
// producer identity after its topic and payload, so a broker redelivery keeps
// its envelope ID and is not marked adapter-minted (ADR 0022). These tests pin
// that ingress rule and that the default stays a fresh, marked UUID.

// contentHashEnvelope runs one publish through the content-hash ingress rule
// and the envelope conversion the router applies after it.
func contentHashEnvelope(pub *pahov5.Publish) *messaging.Envelope {
	return EnvelopeFromPublish(publishWithIdentity(pub, true), nil)
}

func requireNotMarkedGenerated(t *testing.T, env *messaging.Envelope) {
	t.Helper()
	_, generated := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID)
	require.False(t, generated, "a content-hash id is stable across redelivery, so it must not be marked generated")
}

// inboundPublish builds a publish the way the SDK hands one to the router; it
// is the only way to set DUP.
func inboundPublish(p *packets.Publish) *pahov5.Publish {
	if p.Properties == nil {
		p.Properties = &packets.Properties{}
	}
	return pahov5.PublishFromPacketPublish(p)
}

func TestPublishWithIdentity_ContentHashIsTheDocumentedDigest(t *testing.T) {
	// SHA-256 over the topic length (big-endian uint64), the topic and the
	// payload, as unpadded base64url behind the mqtt-sha256: prefix.
	env := contentHashEnvelope(&pahov5.Publish{Topic: "t", Payload: []byte("p")})

	assert.Equal(t, "mqtt-sha256:KNCqR35t-GU1tF6r6uPvU7L1WaqZ7YbpHTbDjTfD1g0", env.ID())
	requireNotMarkedGenerated(t, env)
}

func TestPublishWithIdentity_ContentHashIsStableAcrossRedelivery(t *testing.T) {
	first := contentHashEnvelope(&pahov5.Publish{
		PacketID: 7, QoS: 1, Topic: "sensors/1", Payload: []byte(`{"t":21.5}`),
	})
	// The broker's redelivery carries a new packet id and DUP; a retained
	// replay of the same message carries RETAIN and possibly another QoS.
	for name, redelivery := range map[string]*packets.Publish{
		"dup redelivery":  {PacketID: 9, QoS: 1, Duplicate: true, Topic: "sensors/1", Payload: []byte(`{"t":21.5}`)},
		"retained replay": {PacketID: 3, QoS: 2, Retain: true, Topic: "sensors/1", Payload: []byte(`{"t":21.5}`)},
	} {
		t.Run(name, func(t *testing.T) {
			again := contentHashEnvelope(inboundPublish(redelivery))
			assert.Equal(t, first.ID(), again.ID())
			requireNotMarkedGenerated(t, again)
		})
	}
	requireNotMarkedGenerated(t, first)
	assert.True(t, strings.HasPrefix(first.ID(), contentHashIdentityPrefix), "id %q", first.ID())
}

func TestPublishWithIdentity_ContentHashSeparatesTopicAndPayload(t *testing.T) {
	cases := map[string]*pahov5.Publish{
		"base":                    {Topic: "a", Payload: []byte("bc")},
		"other topic":             {Topic: "a/x", Payload: []byte("bc")},
		"other payload":           {Topic: "a", Payload: []byte("bd")},
		"boundary shifted":        {Topic: "ab", Payload: []byte("c")},
		"empty payload":           {Topic: "abc"},
		"payload as topic":        {Topic: "bc", Payload: []byte("a")},
		"topic in payload":        {Topic: "", Payload: []byte("abc")},
		"length bytes in payload": {Topic: "", Payload: []byte("\x00\x00\x00\x00\x00\x00\x00\x01abc")},
	}
	seen := make(map[string]string, len(cases))
	for name, pub := range cases {
		id := contentHashEnvelope(pub).ID()
		if previous, collide := seen[id]; collide {
			t.Fatalf("%q and %q share envelope id %q", previous, name, id)
		}
		seen[id] = name
	}
}

func TestPublishWithIdentity_ContentHashKeepsAProducerMessageID(t *testing.T) {
	env := contentHashEnvelope(&pahov5.Publish{
		Topic:   "t",
		Payload: []byte("p"),
		Properties: &pahov5.PublishProperties{
			User: pahov5.UserProperties{{Key: HeaderMessageID, Value: "producer-stable-1"}},
		},
	})

	assert.Equal(t, "producer-stable-1", env.ID())
	requireNotMarkedGenerated(t, env)
}

func TestPublishWithIdentity_ContentHashStripsAPublisherGeneratedMarker(t *testing.T) {
	t.Run("without a producer identity", func(t *testing.T) {
		raw := &pahov5.Publish{
			Topic:   "t",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				User: pahov5.UserProperties{{Key: headerMQTTGeneratedID, Value: "1"}},
			},
		}

		sanitized := publishWithIdentity(raw, true)

		for _, property := range sanitized.Properties.User {
			assert.NotEqual(t, headerMQTTGeneratedID, property.Key, "a publisher-supplied marker survived ingress")
		}
		env := EnvelopeFromPublish(sanitized, nil)
		assert.Equal(t, contentHashEnvelope(&pahov5.Publish{Topic: "t", Payload: []byte("p")}).ID(), env.ID())
		requireNotMarkedGenerated(t, env)
		require.Len(t, raw.Properties.User, 1, "sanitising must copy, never mutate the SDK-owned packet")
	})

	t.Run("beside a producer identity", func(t *testing.T) {
		env := contentHashEnvelope(&pahov5.Publish{
			Topic:   "t",
			Payload: []byte("p"),
			Properties: &pahov5.PublishProperties{
				User: pahov5.UserProperties{
					{Key: HeaderMessageID, Value: "producer-stable-1"},
					{Key: headerMQTTGeneratedID, Value: "1"},
				},
			},
		})

		assert.Equal(t, "producer-stable-1", env.ID())
		requireNotMarkedGenerated(t, env)
	})
}

func TestPublishWithIdentity_RandomDefaultMintsAFreshMarkedID(t *testing.T) {
	newPublish := func() *pahov5.Publish { return &pahov5.Publish{QoS: 1, Topic: "t", Payload: []byte("p")} }

	first := EnvelopeFromPublish(publishWithIdentity(newPublish(), false), nil)
	second := EnvelopeFromPublish(publishWithIdentity(newPublish(), false), nil)

	assert.NotEqual(t, first.ID(), second.ID(), "the default mints a fresh id per delivery")
	assert.False(t, strings.HasPrefix(first.ID(), contentHashIdentityPrefix), "id %q", first.ID())
	for _, env := range []*messaging.Envelope{first, second} {
		_, generated := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID)
		assert.True(t, generated, "a minted id must be marked generated")
	}
}

// recordRouterEnvelopes registers a match-all envelope handler on r.
func recordRouterEnvelopes(r *router) func() []*messaging.Envelope {
	var mu sync.Mutex
	var delivered []*messaging.Envelope
	r.RegisterEnvelope("content-hash", nil, nil, func(env *messaging.Envelope, _ func() error) {
		mu.Lock()
		delivered = append(delivered, env)
		mu.Unlock()
	})
	return func() []*messaging.Envelope {
		r.Wait()
		mu.Lock()
		defer mu.Unlock()
		return append([]*messaging.Envelope(nil), delivered...)
	}
}

func TestRouter_ContentHashMessageIDIsStableAtEveryIngressEntryPoint(t *testing.T) {
	r := newRouter(nil, nil, withContentHashMessageID(true))
	delivered := recordRouterEnvelopes(r)

	handled, err := r.onPublishReceived(pahov5.PublishReceived{
		Packet: &pahov5.Publish{PacketID: 1, QoS: 1, Topic: "identity/hash", Payload: []byte("p")},
	})
	require.NoError(t, err)
	require.True(t, handled)
	handled, err = r.onPublishReceived(pahov5.PublishReceived{
		Packet: inboundPublish(&packets.Publish{
			PacketID: 2, QoS: 1, Duplicate: true, Topic: "identity/hash", Payload: []byte("p"),
		}),
	})
	require.NoError(t, err)
	require.True(t, handled)
	r.Route(&packets.Publish{
		PacketID: 3, QoS: 1, Topic: "identity/hash", Payload: []byte("p"), Properties: &packets.Properties{},
	})

	envelopes := delivered()
	require.Len(t, envelopes, 3)
	for i, env := range envelopes {
		assert.Equal(t, envelopes[0].ID(), env.ID(), "delivery %d", i)
		requireNotMarkedGenerated(t, env)
	}
	assert.True(t, strings.HasPrefix(envelopes[0].ID(), contentHashIdentityPrefix), "id %q", envelopes[0].ID())
}

func TestRouter_DefaultMessageIDIsFreshAndMarked(t *testing.T) {
	r := newRouter(nil, nil)
	delivered := recordRouterEnvelopes(r)

	for packetID := range uint16(2) {
		_, err := r.onPublishReceived(pahov5.PublishReceived{
			Packet: &pahov5.Publish{PacketID: packetID + 1, QoS: 1, Topic: "identity/random", Payload: []byte("p")},
		})
		require.NoError(t, err)
	}

	envelopes := delivered()
	require.Len(t, envelopes, 2)
	assert.NotEqual(t, envelopes[0].ID(), envelopes[1].ID())
	for _, env := range envelopes {
		_, generated := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID)
		assert.True(t, generated)
	}
}

func TestNewSession_ContentHashMessageIDAppliesOnlyToMQTT311(t *testing.T) {
	cases := []struct {
		name       string
		opts       SessionOptions
		wantHashed bool
	}{
		{"v3.1.1 content_hash", SessionOptions{ProtocolVersion: ProtocolVersion311, MessageID: MessageIDContentHash}, true},
		{"v3.1.1 random", SessionOptions{ProtocolVersion: ProtocolVersion311, MessageID: MessageIDRandom}, false},
		{"v3.1.1 default", SessionOptions{ProtocolVersion: ProtocolVersion311}, false},
		{"v5 default", SessionOptions{}, false},
		// Refused by Start; the router still never derives an id from content.
		{"v5 content_hash", SessionOptions{MessageID: MessageIDContentHash}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.ClientID = "content-hash"
			s := NewSession(tc.opts, connectivity.SessionPersistent, nil)
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			delivered := recordRouterEnvelopes(s.Router())

			_, err := s.Router().onPublishReceived(pahov5.PublishReceived{
				Packet: &pahov5.Publish{PacketID: 1, QoS: 1, Topic: "t", Payload: []byte("p")},
			})
			require.NoError(t, err)

			envelopes := delivered()
			require.Len(t, envelopes, 1)
			_, generated := messaging.GetHeaderString(envelopes[0].Headers(), messaging.HeaderGeneratedID)
			assert.Equal(t, tc.wantHashed, strings.HasPrefix(envelopes[0].ID(), contentHashIdentityPrefix),
				"id %q", envelopes[0].ID())
			assert.Equal(t, !tc.wantHashed, generated)
		})
	}
}

func TestNewSession_StartRefusesContentHashMessageIDOnV5(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"ssl://192.0.2.1:8883"},
		ClientID:   "refused",
		MessageID:  MessageIDContentHash,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	var dials atomic.Int32
	countingDial(s, &dials, nil)

	require.ErrorIs(t, s.Start(t.Context()), shared.ErrInvalidConfig)
	assert.Zero(t, dials.Load(), "a refused session never dials")
}
