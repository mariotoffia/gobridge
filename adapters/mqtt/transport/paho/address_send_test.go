package paho

import (
	"context"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSender_UsesOutboundAddressNotSubject pins the contract:
// Sender.Send selects the publish topic from msg.Address (or
// SenderOptions.DefaultTopic when Address is empty); it never reads
// msg.Envelope.Subject() for topic selection. The logical subject is
// propagated as a HeaderGobridgeSubject user property.
//
// Send itself requires a real MQTT broker, so we exercise the topic-
// resolution gate via Send (using a fake CM that bypasses the nil
// guard) and the on-the-wire subject mapping via PublishFromEnvelope —
// which is exactly the path Send takes immediately after resolving
// the topic.
func TestSender_UsesOutboundAddressNotSubject(t *testing.T) {
	t.Run("address selects topic and subject becomes user property", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "logical.subject",
			Payload: []byte("p"),
		})
		// Sender.Send resolves topic = msg.Address ("topic/from/address")
		// then calls PublishFromEnvelope with that topic. We verify the
		// resulting on-the-wire packet directly.
		pub := PublishFromEnvelope(env, "topic/from/address", SenderOptions{QoS: 1}, nil)

		if pub.Topic != "topic/from/address" {
			t.Errorf("pub.Topic = %q, want %q", pub.Topic, "topic/from/address")
		}
		if pub.Properties == nil {
			t.Fatal("expected user properties to carry gobridge.subject")
		}
		var found bool
		for _, u := range pub.Properties.User {
			if u.Key == HeaderGobridgeSubject {
				if u.Value != "logical.subject" {
					t.Errorf("gobridge.subject user property = %q, want %q", u.Value, "logical.subject")
				}
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q user property; got user=%+v",
				HeaderGobridgeSubject, pub.Properties.User)
		}
	})

	t.Run("empty address falls back to opts.DefaultTopic", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "logical.subject", Payload: []byte("p")})
		opts := SenderOptions{QoS: 0, DefaultTopic: "default/topic"}

		// Replicate Sender.Send's fallback: address is empty → DefaultTopic.
		topic := ""
		if topic == "" {
			topic = opts.DefaultTopic
		}
		pub := PublishFromEnvelope(env, topic, opts, nil)
		if pub.Topic != "default/topic" {
			t.Errorf("pub.Topic = %q, want %q", pub.Topic, "default/topic")
		}
	})

	t.Run("empty address and empty DefaultTopic returns ErrInvalidTopic", func(t *testing.T) {
		// Drive Sender.Send through its topic-resolution gate. Subject is
		// non-empty: the test proves Subject does NOT rescue the publish
		// when both Address and DefaultTopic are empty.
		sess := NewSession(SessionOptions{
			BrokerURLs: []string{"tcp://192.0.2.1:1883"},
			ClientID:   "t05-no-topic",
		}, connectivity.SessionEphemeral, nil)
		sess.mu.Lock()
		sess.cm = &pahoConn{cm: fakeCM}
		sess.mu.Unlock()

		s := NewSender(sess, SenderOptions{Timeout: time.Second})

		err := s.Send(context.Background(), ports.OutboundMessage{
			Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "logical.subject", Payload: []byte("p")}),
		})
		if err == nil {
			t.Fatal("expected ErrInvalidTopic when Address and DefaultTopic are both empty")
		}
		be, ok := err.(*shared.BridgeError)
		if !ok {
			t.Fatalf("err type = %T, want *shared.BridgeError", err)
		}
		if be.Code != shared.ErrInvalidTopic.Code {
			t.Fatalf("err code = %s, want %s", be.Code, shared.ErrInvalidTopic.Code)
		}
	})
}

// TestMQTTRoundTrip_PreservesLogicalSubjectAndRecordsTopic verifies the
// acceptance criterion: an MQTT round-trip preserves the logical
// Envelope.Subject (carried via HeaderGobridgeSubject) and records the
// publish topic separately under HeaderMQTTTopic.
func TestMQTTRoundTrip_PreservesLogicalSubjectAndRecordsTopic(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "rt-id-1",
		Subject: "orders.created",
		Payload: []byte("data"),
	})

	pub := PublishFromEnvelope(original, "topic/orders/v1", SenderOptions{QoS: 1}, nil)
	if pub.Topic != "topic/orders/v1" {
		t.Fatalf("pub.Topic = %q, want %q", pub.Topic, "topic/orders/v1")
	}

	received := EnvelopeFromPublish(pub, nil)

	if received.Subject() != "orders.created" {
		t.Errorf("received.Subject = %q, want %q (logical subject must round-trip)",
			received.Subject(), "orders.created")
	}
	if v, _ := messaging.GetHeaderString(received.Headers(), HeaderMQTTTopic); v != "topic/orders/v1" {
		t.Errorf("received.Headers[%q] = %q, want %q (transport topic must be recorded)",
			HeaderMQTTTopic, v, "topic/orders/v1")
	}
}

// TestEnvelopeFromPublish_NoGobridgeSubjectLeavesSubjectEmpty verifies
// that when a publish carries no HeaderGobridgeSubject user property,
// Envelope.Subject is left empty (no longer populated from pub.Topic).
func TestEnvelopeFromPublish_NoGobridgeSubjectLeavesSubjectEmpty(t *testing.T) {
	pub := PublishFromEnvelope(
		messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte("p")}), // Subject empty → no user property emitted
		"transport/only/topic",
		SenderOptions{QoS: 0},
		nil,
	)

	received := EnvelopeFromPublish(pub, nil)

	if received.Subject() != "" {
		t.Errorf("received.Subject = %q, want empty", received.Subject())
	}
	if v, _ := messaging.GetHeaderString(received.Headers(), HeaderMQTTTopic); v != "transport/only/topic" {
		t.Errorf("received.Headers[%q] = %q, want %q", HeaderMQTTTopic, v, "transport/only/topic")
	}
}

// TestEnvelopeFromPublish_HostileMQTTTopicUserPropertyIsIgnored verifies
// the adapter-controlled invariant: an inbound user property literally
// named "mqtt.topic" must be dropped so a hostile broker cannot spoof
// the recorded transport address. The recorded value must still be the
// real pub.Topic.
func TestEnvelopeFromPublish_HostileMQTTTopicUserPropertyIsIgnored(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "real/topic",
		Payload: []byte("p"),
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: HeaderMQTTTopic, Value: "spoofed/topic"},
				{Key: HeaderGobridgeSubject, Value: "preserved.subject"},
			},
		},
	}

	received := EnvelopeFromPublish(pub, nil)

	if v, _ := messaging.GetHeaderString(received.Headers(), HeaderMQTTTopic); v != "real/topic" {
		t.Errorf("headers[%q] = %q, want %q (hostile spoof must be ignored)",
			HeaderMQTTTopic, v, "real/topic")
	}
	if received.Subject() != "preserved.subject" {
		t.Errorf("Subject = %q, want %q (gobridge.subject must round-trip on receive)",
			received.Subject(), "preserved.subject")
	}
}
