package paho

import (
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/domain"
)

func TestEnvelopeFromPublish_BasicFields(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "test/topic",
		Payload: []byte("hello"),
	}

	env := EnvelopeFromPublish(pub)

	if env.Subject != "test/topic" {
		t.Errorf("subject = %q, want %q", env.Subject, "test/topic")
	}
	if string(env.Payload) != "hello" {
		t.Errorf("payload = %q, want %q", env.Payload, "hello")
	}
	if env.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
}

func TestEnvelopeFromPublish_CorrelationAndContentType(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("corr-123"),
			ContentType:     "application/json",
		},
	}

	env := EnvelopeFromPublish(pub)

	v, ok := domain.GetHeaderString(env.Headers, domain.HeaderCorrelationID)
	if !ok || v != "corr-123" {
		t.Errorf("correlation = %q, want %q", v, "corr-123")
	}
	v, ok = domain.GetHeaderString(env.Headers, domain.HeaderContentType)
	if !ok || v != "application/json" {
		t.Errorf("content-type = %q, want %q", v, "application/json")
	}
}

func TestEnvelopeFromPublish_MessageExpiry(t *testing.T) {
	expiry := uint32(60)
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			MessageExpiry: &expiry,
		},
	}

	before := time.Now()
	env := EnvelopeFromPublish(pub)
	after := time.Now()

	if env.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt should be set")
	}
	earliest := before.Add(60 * time.Second)
	latest := after.Add(60 * time.Second)
	if env.ExpiresAt.Before(earliest) || env.ExpiresAt.After(latest) {
		t.Errorf("ExpiresAt = %v, want between %v and %v", env.ExpiresAt, earliest, latest)
	}
}

func TestEnvelopeFromPublish_UserProperties(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: "traceparent", Value: "00-abc-def-01"},
				{Key: "custom-key", Value: "custom-val"},
			},
		},
	}

	env := EnvelopeFromPublish(pub)

	if v, _ := domain.GetHeaderString(env.Headers, "traceparent"); v != "00-abc-def-01" {
		t.Errorf("traceparent = %q, want %q", v, "00-abc-def-01")
	}
	if v, _ := domain.GetHeaderString(env.Headers, "custom-key"); v != "custom-val" {
		t.Errorf("custom-key = %q, want %q", v, "custom-val")
	}
}

func TestEnvelopeFromPublish_StripsReservedHeaders(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: "x-bridge.route-id", Value: "injected"},
				{Key: "x-bridge.source-id", Value: "injected"},
				{Key: "safe-key", Value: "safe-val"},
			},
		},
	}

	env := EnvelopeFromPublish(pub)

	if _, ok := env.Headers["x-bridge.route-id"]; ok {
		t.Error("reserved header x-bridge.route-id should be stripped")
	}
	if _, ok := env.Headers["x-bridge.source-id"]; ok {
		t.Error("reserved header x-bridge.source-id should be stripped")
	}
	if v, _ := domain.GetHeaderString(env.Headers, "safe-key"); v != "safe-val" {
		t.Errorf("safe-key = %q, want %q", v, "safe-val")
	}
}

func TestEnvelopeFromPublish_ResponseTopic(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			ResponseTopic: "reply/to",
		},
	}

	env := EnvelopeFromPublish(pub)

	if v, _ := domain.GetHeaderString(env.Headers, "mqtt.response-topic"); v != "reply/to" {
		t.Errorf("response-topic = %q, want %q", v, "reply/to")
	}
}

func TestPublishFromEnvelope_BasicFields(t *testing.T) {
	env := &domain.Envelope{
		Subject: "out/topic",
		Payload: []byte("world"),
	}
	opts := SenderOptions{QoS: 1, Retain: true}

	pub := PublishFromEnvelope(env, opts)

	if pub.Topic != "out/topic" {
		t.Errorf("topic = %q, want %q", pub.Topic, "out/topic")
	}
	if pub.QoS != 1 {
		t.Errorf("QoS = %d, want 1", pub.QoS)
	}
	if !pub.Retain {
		t.Error("Retain should be true")
	}
	if string(pub.Payload) != "world" {
		t.Errorf("payload = %q, want %q", pub.Payload, "world")
	}
}

func TestPublishFromEnvelope_DefaultTopic(t *testing.T) {
	env := &domain.Envelope{Payload: []byte("x")}
	opts := SenderOptions{DefaultTopic: "fallback/topic", QoS: 0}

	pub := PublishFromEnvelope(env, opts)

	if pub.Topic != "fallback/topic" {
		t.Errorf("topic = %q, want %q", pub.Topic, "fallback/topic")
	}
}

func TestPublishFromEnvelope_Headers(t *testing.T) {
	env := &domain.Envelope{
		Subject: "t",
		Payload: []byte("x"),
		Headers: map[string]any{
			domain.HeaderCorrelationID: "corr-456",
			domain.HeaderContentType:   "text/plain",
			"traceparent":              "00-xyz",
			"mqtt.response-topic":      "reply",
		},
	}
	opts := SenderOptions{QoS: 1}

	pub := PublishFromEnvelope(env, opts)

	if pub.Properties == nil {
		t.Fatal("properties should be set")
	}
	if string(pub.Properties.CorrelationData) != "corr-456" {
		t.Errorf("CorrelationData = %q, want %q", pub.Properties.CorrelationData, "corr-456")
	}
	if pub.Properties.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", pub.Properties.ContentType, "text/plain")
	}
	if pub.Properties.ResponseTopic != "reply" {
		t.Errorf("ResponseTopic = %q, want %q", pub.Properties.ResponseTopic, "reply")
	}

	found := false
	for _, u := range pub.Properties.User {
		if u.Key == "traceparent" && u.Value == "00-xyz" {
			found = true
		}
	}
	if !found {
		t.Error("traceparent should be in user properties")
	}
}

func TestPublishFromEnvelope_MessageExpiry(t *testing.T) {
	env := &domain.Envelope{
		Subject:   "t",
		Payload:   []byte("x"),
		ExpiresAt: time.Now().Add(120 * time.Second),
	}
	opts := SenderOptions{QoS: 1}

	pub := PublishFromEnvelope(env, opts)

	if pub.Properties == nil || pub.Properties.MessageExpiry == nil {
		t.Fatal("MessageExpiry should be set")
	}
	if *pub.Properties.MessageExpiry < 118 || *pub.Properties.MessageExpiry > 121 {
		t.Errorf("MessageExpiry = %d, want ~120", *pub.Properties.MessageExpiry)
	}
}

func TestPublishFromEnvelope_NoProperties(t *testing.T) {
	env := &domain.Envelope{
		Subject: "t",
		Payload: []byte("x"),
	}
	opts := SenderOptions{QoS: 0}

	pub := PublishFromEnvelope(env, opts)

	if pub.Properties != nil {
		for _, u := range pub.Properties.User {
			t.Errorf("unexpected user property: %s=%s", u.Key, u.Value)
		}
	}
}

func TestRoundTrip_EnvelopePublishEnvelope(t *testing.T) {
	original := &domain.Envelope{
		Subject: "round/trip",
		Payload: []byte("data"),
		Headers: map[string]any{
			domain.HeaderCorrelationID: "rt-id",
			domain.HeaderContentType:   "application/octet-stream",
			"custom":                   "value",
		},
	}

	opts := SenderOptions{QoS: 1}
	pub := PublishFromEnvelope(original, opts)
	restored := EnvelopeFromPublish(pub)

	if restored.Subject != original.Subject {
		t.Errorf("subject = %q, want %q", restored.Subject, original.Subject)
	}
	if string(restored.Payload) != string(original.Payload) {
		t.Errorf("payload = %q, want %q", restored.Payload, original.Payload)
	}
	if v, _ := domain.GetHeaderString(restored.Headers, domain.HeaderCorrelationID); v != "rt-id" {
		t.Errorf("correlation = %q, want %q", v, "rt-id")
	}
	if v, _ := domain.GetHeaderString(restored.Headers, domain.HeaderContentType); v != "application/octet-stream" {
		t.Errorf("content-type = %q, want %q", v, "application/octet-stream")
	}
	if v, _ := domain.GetHeaderString(restored.Headers, "custom"); v != "value" {
		t.Errorf("custom = %q, want %q", v, "value")
	}
}
