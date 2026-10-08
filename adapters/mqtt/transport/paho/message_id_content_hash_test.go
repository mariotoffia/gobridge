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
	"github.com/mariotoffia/gobridge/ports"
)

// A session set to message_id content_hash names a publish that carries no
// producer identity after its broker session, topic and payload, so a broker
// redelivery keeps its envelope ID and is not marked adapter-minted (ADR 0022).
// These tests pin that ingress rule and that the default stays a fresh, marked
// UUID.

// contentHashEnvelope runs one publish through the content-hash ingress rule of
// a session connecting as "c" to tcp://broker:1883, and the envelope conversion
// the router applies after it.
func contentHashEnvelope(pub *pahov5.Publish) *messaging.Envelope {
	scope := appendContentHashField(appendContentHashField(nil, "c"), "tcp://broker:1883")
	return EnvelopeFromPublish(publishWithIdentity(pub, scope), nil)
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

// contentHashSession builds an MQTT 3.1.1 session set to message_id
// content_hash that connects as clientID to brokerURLs.
func contentHashSession(t *testing.T, clientID string, brokerURLs ...string) *Session {
	t.Helper()
	s := NewSession(SessionOptions{
		BrokerURLs:      brokerURLs,
		ClientID:        clientID,
		ProtocolVersion: ProtocolVersion311,
		MessageID:       MessageIDContentHash,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// routedEnvelopeID routes one inbound publish through s's router and returns
// the envelope ID its handler saw.
func routedEnvelopeID(t *testing.T, s *Session, topic, payload string) string {
	t.Helper()
	delivered := recordRouterEnvelopes(s.Router())
	_, err := s.Router().onPublishReceived(pahov5.PublishReceived{
		Packet: &pahov5.Publish{PacketID: 1, QoS: 1, Topic: topic, Payload: []byte(payload)},
	})
	require.NoError(t, err)
	envelopes := delivered()
	require.Len(t, envelopes, 1)
	requireNotMarkedGenerated(t, envelopes[0])
	return envelopes[0].ID()
}

func TestContentHashMessageID_IsTheDocumentedDigest(t *testing.T) {
	// SHA-256 over the client_id, the canonical broker URLs joined by "\n" and
	// the topic, each behind its byte length as a big-endian uint64, then the
	// payload; unpadded base64url behind the mqtt-sha256: prefix. Every case
	// hashes client_id "c", topic "t" and payload "p". The expected ids were
	// computed outside Go from those inputs and the canonical broker field.
	cases := []struct {
		name       string
		brokerURLs []string
		canonical  string
		want       string
	}{
		{
			name:       "one broker",
			brokerURLs: []string{"tcp://broker:1883"},
			canonical:  "tcp://broker:1883",
			want:       "mqtt-sha256:J9sTAITCmYSboGvwPskEehxc2_vzajBMr1WEj0MBI1k",
		},
		{
			name:       "broker list in configured order",
			brokerURLs: []string{"MQTT://Broker-A", "ssl://bridge@broker-b:8883"},
			canonical:  "tcp://broker-a:1883\nssl://broker-b:8883",
			want:       "mqtt-sha256:6bwIAW51a1tHqgLx-2FJXB4QouW3srgIufR_1kfiQ1A",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brokers, err := canonicalBrokerSet(tc.brokerURLs, "")
			require.NoError(t, err)
			require.Equal(t, tc.canonical, strings.Join(brokers, "\n"))

			assert.Equal(t, tc.want, routedEnvelopeID(t, contentHashSession(t, "c", tc.brokerURLs...), "t", "p"))
		})
	}
}

func TestContentHashMessageID_IsScopedToTheBrokerSession(t *testing.T) {
	// Two sessions feeding one shared binding may receive the same topic and
	// payload; a broker redelivers only on the session that received it.
	base := routedEnvelopeID(t, contentHashSession(t, "bridge-a", "tcp://broker:1883"), "t", "p")
	cases := []struct {
		name     string
		clientID string
		broker   string
		same     bool
	}{
		{"another client_id", "bridge-b", "tcp://broker:1883", false},
		{"another broker", "bridge-a", "tcp://other-broker:1883", false},
		{"the same broker spelled differently", "bridge-a", "mqtt://BROKER", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := routedEnvelopeID(t, contentHashSession(t, tc.clientID, tc.broker), "t", "p")
			assert.Equal(t, tc.same, id == base, "id %q, base %q", id, base)
		})
	}
}

func TestFactoryNewSession_ContentHashMessageIDHashesTheEffectiveClientID(t *testing.T) {
	cfg := Config{Session: SessionOptions{
		BrokerURLs:      []string{"tcp://broker:1883"},
		ClientID:        "c",
		ClientIDSuffix:  ClientIDSuffixNonce,
		ProtocolVersion: ProtocolVersion311,
		MessageID:       MessageIDContentHash,
	}}
	// The nonce suffix hex-encodes these 16 bytes, so the session connects as
	// "c-30313233343536373839616263646566".
	cfg.clientIDSuffixIdentity = &clientIDSuffixProcessIdentity{random: strings.NewReader("0123456789abcdef")}
	built, err := NewFactory(nil).NewSession(t.Context(), ports.SessionSpec{
		ID: "content-hash", SessionMode: connectivity.SessionEphemeral, Config: cfg,
	})
	require.NoError(t, err)
	s, ok := built.(*Session)
	require.True(t, ok)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	got := routedEnvelopeID(t, s, "t", "p")

	effective := contentHashSession(t, "c-30313233343536373839616263646566", "tcp://broker:1883")
	assert.Equal(t, routedEnvelopeID(t, effective, "t", "p"), got)
	assert.NotEqual(t, routedEnvelopeID(t, contentHashSession(t, "c", "tcp://broker:1883"), "t", "p"), got,
		"the configured client_id without its suffix names no broker session")
}

func TestNewSession_StartRefusesContentHashMessageIDWithoutACanonicalBroker(t *testing.T) {
	s := contentHashSession(t, "refused", "tcp://:1883")
	var dials atomic.Int32
	countingDial(s, &dials, nil)

	require.ErrorIs(t, s.Start(t.Context()), shared.ErrInvalidConfig)
	assert.Zero(t, dials.Load(), "a session whose broker session cannot be named never dials")
}

func TestNewSession_StartRefusesContentHashMessageIDWithoutAClientID(t *testing.T) {
	s := contentHashSession(t, "", "tcp://broker:1883")
	var dials atomic.Int32
	countingDial(s, &dials, nil)

	require.ErrorIs(t, s.Start(t.Context()), shared.ErrInvalidConfig)
	assert.Zero(t, dials.Load(), "without a client id two sessions would share one hash scope")
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

func TestPublishWithIdentity_RandomDefaultMintsAFreshMarkedID(t *testing.T) {
	newPublish := func() *pahov5.Publish { return &pahov5.Publish{QoS: 1, Topic: "t", Payload: []byte("p")} }

	first := EnvelopeFromPublish(publishWithIdentity(newPublish(), nil), nil)
	second := EnvelopeFromPublish(publishWithIdentity(newPublish(), nil), nil)

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
	r := newRouter(nil, nil, withContentHashMessageID(appendContentHashField(nil, "c")))
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
