package paho

import (
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// verifies EnvelopeFromPublish records the publish topic under
// HeaderMQTTTopic and leaves env.Subject() empty when no
// HeaderGobridgeSubject user property is present.
func TestEnvelopeFromPublish_BasicFields(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "test/topic",
		Payload: []byte("hello"),
	}

	env := EnvelopeFromPublish(pub, nil)

	if env.Subject() != "" {
		t.Errorf("subject = %q, want empty (no gobridge.subject user property)", env.Subject())
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), HeaderMQTTTopic); v != "test/topic" {
		t.Errorf("headers[%q] = %q, want %q", HeaderMQTTTopic, v, "test/topic")
	}
	if string(env.Payload()) != "hello" {
		t.Errorf("payload = %q, want %q", env.Payload(), "hello")
	}
	if env.CreatedAt().IsZero() {
		t.Error("CreatedAt should be set")
	}
}

// verifies EnvelopeFromPublish maps correlation data and content type into headers.
func TestEnvelopeFromPublish_CorrelationAndContentType(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("corr-123"),
			ContentType:     "application/json",
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID)
	if !ok || v != "corr-123" {
		t.Errorf("correlation = %q, want %q", v, "corr-123")
	}
	v, ok = messaging.GetHeaderString(env.Headers(), messaging.HeaderContentType)
	if !ok || v != "application/json" {
		t.Errorf("content-type = %q, want %q", v, "application/json")
	}
}

// verifies EnvelopeFromPublish sets ExpiresAt from MessageExpiry seconds.
func TestEnvelopeFromPublish_MessageExpiry(t *testing.T) {
	expiry := uint32(60)
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			MessageExpiry: &expiry,
		},
	}

	before := time.Now()
	env := EnvelopeFromPublish(pub, nil)
	after := time.Now()

	if env.ExpiresAt().IsZero() {
		t.Fatal("ExpiresAt should be set")
	}
	earliest := before.Add(60 * time.Second)
	latest := after.Add(60 * time.Second)
	if env.ExpiresAt().Before(earliest) || env.ExpiresAt().After(latest) {
		t.Errorf("ExpiresAt = %v, want between %v and %v", env.ExpiresAt(), earliest, latest)
	}
}

// verifies EnvelopeFromPublish copies MQTT user properties into envelope headers.
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

	env := EnvelopeFromPublish(pub, nil)

	if v, _ := messaging.GetHeaderString(env.Headers(), "traceparent"); v != "00-abc-def-01" {
		t.Errorf("traceparent = %q, want %q", v, "00-abc-def-01")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), "custom-key"); v != "custom-val" {
		t.Errorf("custom-key = %q, want %q", v, "custom-val")
	}
}

// verifies EnvelopeFromPublish strips injected bridge route and source headers.
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

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()["x-bridge.route-id"]; ok {
		t.Error("reserved header x-bridge.route-id should be stripped")
	}
	if _, ok := env.Headers()["x-bridge.source-id"]; ok {
		t.Error("reserved header x-bridge.source-id should be stripped")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), "safe-key"); v != "safe-val" {
		t.Errorf("safe-key = %q, want %q", v, "safe-val")
	}
}

// verifies EnvelopeFromPublish maps ResponseTopic to mqtt.response-topic.
func TestEnvelopeFromPublish_ResponseTopic(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			ResponseTopic: "reply/to",
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if v, _ := messaging.GetHeaderString(env.Headers(), "mqtt.response-topic"); v != "reply/to" {
		t.Errorf("response-topic = %q, want %q", v, "reply/to")
	}
}

// verifies PublishFromEnvelope maps subject, payload, QoS, and retain.
func TestPublishFromEnvelope_BasicFields(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "out/topic",
		Payload: []byte("world"),
	})
	opts := SenderOptions{QoS: 1, Retain: true}

	pub := PublishFromEnvelope(env, env.Subject(), opts, nil)

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

// verifies PublishFromEnvelope uses the supplied topic argument verbatim,
// even when the envelope subject is empty (the topic is the transport
// destination — independent from the logical subject).
func TestPublishFromEnvelope_DefaultTopic(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte("x")})
	opts := SenderOptions{DefaultTopic: "fallback/topic", QoS: 0}

	pub := PublishFromEnvelope(env, opts.DefaultTopic, opts, nil)

	if pub.Topic != "fallback/topic" {
		t.Errorf("topic = %q, want %q", pub.Topic, "fallback/topic")
	}
}

// verifies PublishFromEnvelope maps correlation, content-type, response topic, and user props.
func TestPublishFromEnvelope_Headers(t *testing.T) {
	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		Subject: "t",
		Payload: []byte("x"),
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "corr-456",
			messaging.HeaderContentType:   "text/plain",
			"traceparent":                 "00-xyz",
			"mqtt.response-topic":         "reply",
		},
	})
	opts := SenderOptions{QoS: 1}

	pub := PublishFromEnvelope(env, env.Subject(), opts, nil)

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

// verifies PublishFromEnvelope sets MessageExpiry from envelope ExpiresAt.
func TestPublishFromEnvelope_MessageExpiry(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject:   "t",
		Payload:   []byte("x"),
		ExpiresAt: time.Now().Add(120 * time.Second),
	})
	opts := SenderOptions{QoS: 1}

	pub := PublishFromEnvelope(env, env.Subject(), opts, nil)

	if pub.Properties == nil || pub.Properties.MessageExpiry == nil {
		t.Fatal("MessageExpiry should be set")
	}
	if *pub.Properties.MessageExpiry < 118 || *pub.Properties.MessageExpiry > 121 {
		t.Errorf("MessageExpiry = %d, want ~120", *pub.Properties.MessageExpiry)
	}
}

// verifies PublishFromEnvelope omits Properties when no header-derived fields are needed.
func TestPublishFromEnvelope_NoProperties(t *testing.T) {
	// Note: env.Subject() is intentionally empty so PublishFromEnvelope
	// does NOT emit a HeaderGobridgeSubject user property.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte("x")})
	opts := SenderOptions{QoS: 0}

	pub := PublishFromEnvelope(env, "t", opts, nil)

	if pub.Properties != nil {
		for _, u := range pub.Properties.User {
			// MustEnvelope auto-assigns an ID, so mqtt.message-id is expected; all others are unexpected.
			if u.Key != "mqtt.message-id" {
				t.Errorf("unexpected user property: %s=%s", u.Key, u.Value)
			}
		}
	}
}

// TestEnvelopeFromPublish_RejectsNonPrintableCorrelationData validates that
// non-printable ASCII in CorrelationData is dropped to prevent log injection.
func TestEnvelopeFromPublish_RejectsNonPrintableCorrelationData(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("corr\x00id\nnewline"),
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; ok {
		t.Error("non-printable correlation data should be rejected")
	}
}

// TestEnvelopeFromPublish_RejectsOversizedCorrelationData validates that
// correlation data exceeding maxHeaderValueLen is dropped.
func TestEnvelopeFromPublish_RejectsOversizedCorrelationData(t *testing.T) {
	longCorr := make([]byte, maxHeaderValueLen+1)
	for i := range longCorr {
		longCorr[i] = 'a'
	}

	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: longCorr,
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; ok {
		t.Error("oversized correlation data should be rejected")
	}
}

// TestEnvelopeFromPublish_RejectsNonPrintableContentType validates that
// non-printable ASCII in ContentType is dropped.
func TestEnvelopeFromPublish_RejectsNonPrintableContentType(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			ContentType: "text/plain\x00; charset=utf-8",
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()[messaging.HeaderContentType]; ok {
		t.Error("non-printable content type should be rejected")
	}
}

// TestEnvelopeFromPublish_AcceptsValidCorrelationData validates that
// valid printable ASCII CorrelationData within size limits is accepted.
func TestEnvelopeFromPublish_AcceptsValidCorrelationData(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("550e8400-e29b-41d4-a716-446655440000"),
			ContentType:     "application/json",
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID)
	if !ok || v != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("valid correlation data should be accepted, got %q", v)
	}
	v, ok = messaging.GetHeaderString(env.Headers(), messaging.HeaderContentType)
	if !ok || v != "application/json" {
		t.Errorf("valid content type should be accepted, got %q", v)
	}
}

// TestEnvelopeFromPublish_TimeConsistency validates that CreatedAt and
// ExpiresAt are computed from the same time base (no drift).
func TestEnvelopeFromPublish_TimeConsistency(t *testing.T) {
	expiry := uint32(300)
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			MessageExpiry: &expiry,
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	diff := env.ExpiresAt().Sub(env.CreatedAt())
	if diff != 300*time.Second {
		t.Errorf("ExpiresAt - CreatedAt = %v, want exactly 300s (same time base)", diff)
	}
}

// TestEnvelopeFromPublish_NilProperties validates that a publish with
// nil properties produces an envelope carrying only the adapter-controlled
// transport headers (recorded topic, retained flag, QoS) plus the
// generated-identity marker: with no producer identity the id is adapter-minted,
// which the adapter records so the runtime can bound the uncountable replay case
// (MQTT-CORE-1).
func TestEnvelopeFromPublish_NilProperties(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "t",
		Payload: []byte("p"),
	}

	env := EnvelopeFromPublish(pub, nil)

	if got := len(env.Headers()); got != 4 {
		t.Errorf("expected exactly 4 headers (mqtt.topic, mqtt.retained, mqtt.qos, x-bridge.generated-id) for nil properties, got %d: %v", got, env.Headers())
	}
	if _, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderGeneratedID); !ok {
		t.Errorf("no-identity publish: expected HeaderGeneratedID marker (MQTT-CORE-1)")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), HeaderMQTTTopic); v != "t" {
		t.Errorf("headers[%q] = %q, want %q", HeaderMQTTTopic, v, "t")
	}
	if v, ok := env.Headers()[HeaderMQTTRetained].(bool); !ok || v {
		t.Errorf("headers[%q] = %v, want false", HeaderMQTTRetained, env.Headers()[HeaderMQTTRetained])
	}
	if v, ok := env.Headers()[HeaderMQTTQoS].(int); !ok || v != 0 {
		t.Errorf("headers[%q] = %v, want 0", HeaderMQTTQoS, env.Headers()[HeaderMQTTQoS])
	}
}

// TestEnvelopeFromPublish_MixedCaseReservedHeaders validates that
// mixed-case user properties with reserved prefix are stripped.
func TestEnvelopeFromPublish_MixedCaseReservedHeaders(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: "X-Bridge.Route-Id", Value: "injected"},
				{Key: "X-BRIDGE.SOURCE-ID", Value: "injected"},
				{Key: "safe-key", Value: "safe-val"},
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()["X-Bridge.Route-Id"]; ok {
		t.Error("mixed-case reserved header should be stripped")
	}
	if _, ok := env.Headers()["X-BRIDGE.SOURCE-ID"]; ok {
		t.Error("uppercase reserved header should be stripped")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), "safe-key"); v != "safe-val" {
		t.Error("safe key should be preserved")
	}
}

// TestEnvelopeFromPublish_RejectsNonPrintableResponseTopic validates that
// response topics with control characters are dropped.
func TestEnvelopeFromPublish_RejectsNonPrintableResponseTopic(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			ResponseTopic: "reply/\x00topic",
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()[headerMQTTResponseTopic]; ok {
		t.Error("non-printable response topic should be rejected")
	}
}

// TestEnvelopeFromPublish_RejectsOversizedUserProperty validates that
// user properties with oversized keys or values are dropped.
func TestEnvelopeFromPublish_RejectsOversizedUserProperty(t *testing.T) {
	longVal := make([]byte, maxHeaderValueLen+1)
	for i := range longVal {
		longVal[i] = 'x'
	}

	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: "normal-key", Value: string(longVal)},
				{Key: "safe-key", Value: "safe-val"},
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()["normal-key"]; ok {
		t.Error("oversized user property value should be rejected")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), "safe-key"); v != "safe-val" {
		t.Error("safe user property should be preserved")
	}
}

// TestEnvelopeFromPublish_RejectsNonPrintableUserPropertyKey validates that
// user properties with non-printable key characters are dropped.
func TestEnvelopeFromPublish_RejectsNonPrintableUserPropertyKey(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: "bad\nkey", Value: "val"},
				{Key: "good-key", Value: "good-val"},
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if _, ok := env.Headers()["bad\nkey"]; ok {
		t.Error("user property with non-printable key should be rejected")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), "good-key"); v != "good-val" {
		t.Error("good user property should be preserved")
	}
}

// verifies EnvelopeFromPublish recovers envelope ID from mqtt.message-id user property.
func TestEnvelopeFromPublish_IDFromMessageIDHeader(t *testing.T) {
	pub := &pahov5.Publish{
		Topic:   "t",
		Payload: []byte("p"),
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: HeaderMessageID, Value: "my-envelope-id"},
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if env.ID() != "my-envelope-id" {
		t.Errorf("ID = %q, want %q", env.ID(), "my-envelope-id")
	}
	if v, _ := messaging.GetHeaderString(env.Headers(), HeaderMessageID); v != "my-envelope-id" {
		t.Errorf("header %s = %q, want %q", HeaderMessageID, v, "my-envelope-id")
	}
}

// verifies that mqtt.message-id takes precedence over correlation-id for Envelope.ID().
func TestEnvelopeFromPublish_IDPrecedence_MsgIDOverCorrelation(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("corr-id"),
			User: []pahov5.UserProperty{
				{Key: HeaderMessageID, Value: "msg-id"},
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if env.ID() != "msg-id" {
		t.Errorf("ID = %q, want %q (mqtt.message-id should take precedence)", env.ID(), "msg-id")
	}
}

// verifies that correlation-id is used as Envelope.ID() when mqtt.message-id is absent.
func TestEnvelopeFromPublish_IDFromCorrelation(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			CorrelationData: []byte("corr-fallback"),
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if env.ID() != "corr-fallback" {
		t.Errorf("ID = %q, want %q (should fall back to correlation-id)", env.ID(), "corr-fallback")
	}
}

// verifies that separate equal-valued MQTT publishes without producer
// identity receive distinct fallback IDs. MQTT packet fields and content do
// not prove application identity, so collapsing them would lose an event in a
// deduplicating outbox.
func TestEnvelopeFromPublish_IDFallbackDistinctPerPublish(t *testing.T) {
	first := &pahov5.Publish{Topic: "test/topic", QoS: 1, Payload: []byte("some-payload")}
	second := &pahov5.Publish{Topic: "test/topic", QoS: 1, Payload: []byte("some-payload")}

	env1 := EnvelopeFromPublish(first, nil)
	env2 := EnvelopeFromPublish(second, nil)

	if env1.ID() == "" || env2.ID() == "" {
		t.Fatal("fallback IDs must not be empty")
	}
	if env1.ID() == env2.ID() {
		t.Fatalf("separate equal-valued publishes must receive distinct IDs, both got %q", env1.ID())
	}
	for _, id := range []string{env1.ID(), env2.ID()} {
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '4' {
			t.Fatalf("fallback ID %q is not an RFC 4122 UUIDv4", id)
		}
		switch id[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("fallback ID %q does not have the RFC 4122 variant", id)
		}
	}
}

// verifies PublishFromEnvelope includes Envelope.ID() as mqtt.message-id user property.
func TestPublishFromEnvelope_IncludesMessageID(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "my-id-123",
		Subject: "t",
		Payload: []byte("p"),
	})
	pub := PublishFromEnvelope(env, env.Subject(), SenderOptions{QoS: 1}, nil)

	if pub.Properties == nil {
		t.Fatal("properties should be set")
	}
	found := false
	for _, u := range pub.Properties.User {
		if u.Key == HeaderMessageID && u.Value == "my-id-123" {
			found = true
		}
	}
	if !found {
		t.Errorf("mqtt.message-id user property not found; user=%+v", pub.Properties.User)
	}
}

// verifies full round-trip of Envelope.ID() through Publish and back.
func TestRoundTrip_EnvelopeID(t *testing.T) {
	original := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "round-trip-id-456",
		Subject: "rt/topic",
		Payload: []byte("data"),
	})

	pub := PublishFromEnvelope(original, original.Subject(), SenderOptions{QoS: 1}, nil)
	restored := EnvelopeFromPublish(pub, nil)

	if restored.ID() != original.ID() {
		t.Errorf("Envelope.ID() round-trip: got %q, want %q", restored.ID(), original.ID())
	}
}

// verifies PublishFromEnvelope followed by EnvelopeFromPublish preserves key fields and headers.
func TestRoundTrip_EnvelopePublishEnvelope(t *testing.T) {
	original := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		Subject: "round/trip",
		Payload: []byte("data"),
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "rt-id",
			messaging.HeaderContentType:   "application/octet-stream",
			"custom":                      "value",
		},
	})

	opts := SenderOptions{QoS: 1}
	pub := PublishFromEnvelope(original, original.Subject(), opts, nil)
	restored := EnvelopeFromPublish(pub, nil)

	if restored.Subject() != original.Subject() {
		t.Errorf("subject = %q, want %q", restored.Subject(), original.Subject())
	}
	if string(restored.Payload()) != string(original.Payload()) {
		t.Errorf("payload = %q, want %q", restored.Payload(), original.Payload())
	}
	if v, _ := messaging.GetHeaderString(restored.Headers(), messaging.HeaderCorrelationID); v != "rt-id" {
		t.Errorf("correlation = %q, want %q", v, "rt-id")
	}
	if v, _ := messaging.GetHeaderString(restored.Headers(), messaging.HeaderContentType); v != "application/octet-stream" {
		t.Errorf("content-type = %q, want %q", v, "application/octet-stream")
	}
	if v, _ := messaging.GetHeaderString(restored.Headers(), "custom"); v != "value" {
		t.Errorf("custom = %q, want %q", v, "value")
	}
}
