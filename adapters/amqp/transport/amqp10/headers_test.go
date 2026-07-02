// Validates bidirectional mapping between AMQP 1.0 message properties and envelope headers.
package amqp10

import (
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

func TestMessageToHeaders(t *testing.T) {
	ct := "application/json"
	ce := "gzip"
	subj := "order.created"
	to := "/topic/orders"
	replyTo := "/queue/reply"
	groupID := "group-1"
	var groupSeq uint32 = 42
	replyGroup := "reply-group"
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)

	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			MessageID:          "msg-123",
			CorrelationID:      "corr-456",
			ContentType:        &ct,
			ContentEncoding:    &ce,
			Subject:            &subj,
			To:                 &to,
			ReplyTo:            &replyTo,
			GroupID:            &groupID,
			GroupSequence:      &groupSeq,
			ReplyToGroupID:     &replyGroup,
			CreationTime:       &created,
			AbsoluteExpiryTime: &expiry,
		},
		Header: &amqp.MessageHeader{
			DeliveryCount: 3,
		},
		ApplicationProperties: map[string]any{
			"user-key": "user-value",
		},
	}

	h := messageToHeaders(msg)

	expectations := map[string]any{
		"amqp10.message-id":           "msg-123",
		"amqp10.correlation-id":       "corr-456",
		"amqp10.content-type":         "application/json",
		"amqp10.content-encoding":     "gzip",
		"amqp10.subject":              "order.created",
		"amqp10.to":                   "/topic/orders",
		"amqp10.reply-to":             "/queue/reply",
		"amqp10.group-id":             "group-1",
		"amqp10.group-sequence":       uint32(42),
		"amqp10.reply-to-group-id":    "reply-group",
		"amqp10.creation-time":        created,
		"amqp10.absolute-expiry-time": expiry,
		"amqp10.delivery-count":       uint32(3),
		"user-key":                    "user-value",
	}

	for k, want := range expectations {
		got, ok := h[k]
		if !ok {
			t.Errorf("header %q missing", k)
			continue
		}
		if got != want {
			t.Errorf("header %q = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}
}

func TestMessageToHeaders_NilProperties(t *testing.T) {
	msg := &amqp.Message{}
	h := messageToHeaders(msg)

	if len(h) != 0 {
		t.Fatalf("expected empty headers for nil properties, got %d entries", len(h))
	}
}

func TestMessageToHeaders_ReservedFiltered(t *testing.T) {
	msg := &amqp.Message{
		ApplicationProperties: map[string]any{
			"x-bridge.route-id":  "should-be-stripped",
			"x-bridge.source-id": "also-stripped",
			"safe-key":           "kept",
		},
	}

	h := messageToHeaders(msg)

	if _, ok := h["x-bridge.route-id"]; ok {
		t.Error("reserved header x-bridge.route-id should be stripped")
	}
	if _, ok := h["x-bridge.source-id"]; ok {
		t.Error("reserved header x-bridge.source-id should be stripped")
	}
	if v, ok := h["safe-key"]; !ok || v != "kept" {
		t.Errorf("safe-key = %v, want %q", v, "kept")
	}
}

func TestMessageToHeaders_ApplicationProperties(t *testing.T) {
	msg := &amqp.Message{
		ApplicationProperties: map[string]any{
			"tenant":  "acme",
			"version": 2,
		},
	}

	h := messageToHeaders(msg)

	if h["tenant"] != "acme" {
		t.Errorf("tenant = %v, want %q", h["tenant"], "acme")
	}
	if h["version"] != 2 {
		t.Errorf("version = %v, want 2", h["version"])
	}
}

func TestHeadersToMessage(t *testing.T) {
	ct := "text/plain"
	created := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	expiry := time.Date(2025, 6, 16, 12, 0, 0, 0, time.UTC)

	headers := map[string]any{
		"amqp10.message-id":           "id-abc",
		"amqp10.correlation-id":       "corr-xyz",
		"amqp10.content-type":         ct,
		"amqp10.subject":              "evt.shipped",
		"amqp10.to":                   "/queue/target",
		"amqp10.reply-to":             "/queue/resp",
		"amqp10.group-id":             "grp-A",
		"amqp10.group-sequence":       uint32(7),
		"amqp10.reply-to-group-id":    "rgrp-B",
		"amqp10.creation-time":        created,
		"amqp10.absolute-expiry-time": expiry,
		"custom-prop":                 "hello",
	}

	msg := headersToMessage(headers)

	if msg.Properties == nil {
		t.Fatal("Properties should be non-nil")
	}
	if msg.Properties.MessageID != "id-abc" {
		t.Errorf("MessageID = %v, want %q", msg.Properties.MessageID, "id-abc")
	}
	if msg.Properties.CorrelationID != "corr-xyz" {
		t.Errorf("CorrelationID = %v, want %q", msg.Properties.CorrelationID, "corr-xyz")
	}
	if msg.Properties.ContentType == nil || *msg.Properties.ContentType != ct {
		t.Errorf("ContentType = %v, want %q", msg.Properties.ContentType, ct)
	}
	// Finding 5 (domain invariant): the amqp10.subject header must NOT
	// drive the egress Subject. headersToMessage leaves Properties.Subject
	// unset; Envelope.Subject is the sole source (see envelopeToMessage
	// and TestEnvelopeToMessage_SubjectOnlyFromEnvelope).
	if msg.Properties.Subject != nil {
		t.Errorf("Subject = %v, want nil (amqp10.subject header must not set Subject)", *msg.Properties.Subject)
	}
	if msg.Properties.To == nil || *msg.Properties.To != "/queue/target" {
		t.Errorf("To = %v", msg.Properties.To)
	}
	if msg.Properties.ReplyTo == nil || *msg.Properties.ReplyTo != "/queue/resp" {
		t.Errorf("ReplyTo = %v", msg.Properties.ReplyTo)
	}
	if msg.Properties.GroupID == nil || *msg.Properties.GroupID != "grp-A" {
		t.Errorf("GroupID = %v", msg.Properties.GroupID)
	}
	if msg.Properties.GroupSequence == nil || *msg.Properties.GroupSequence != 7 {
		t.Errorf("GroupSequence = %v", msg.Properties.GroupSequence)
	}
	if msg.Properties.ReplyToGroupID == nil || *msg.Properties.ReplyToGroupID != "rgrp-B" {
		t.Errorf("ReplyToGroupID = %v", msg.Properties.ReplyToGroupID)
	}
	if msg.Properties.CreationTime == nil || !msg.Properties.CreationTime.Equal(created) {
		t.Errorf("CreationTime = %v", msg.Properties.CreationTime)
	}
	if msg.Properties.AbsoluteExpiryTime == nil || !msg.Properties.AbsoluteExpiryTime.Equal(expiry) {
		t.Errorf("AbsoluteExpiryTime = %v", msg.Properties.AbsoluteExpiryTime)
	}

	if msg.ApplicationProperties["custom-prop"] != "hello" {
		t.Errorf("ApplicationProperties[custom-prop] = %v", msg.ApplicationProperties["custom-prop"])
	}
}

func TestHeadersToMessage_WellKnown(t *testing.T) {
	headers := map[string]any{
		"amqp10.message-id":     "well-known-id",
		"amqp10.content-type":   "application/octet-stream",
		"amqp10.group-sequence": int(99),
	}

	msg := headersToMessage(headers)

	if msg.Properties == nil {
		t.Fatal("Properties should be non-nil for well-known headers")
	}
	if msg.Properties.MessageID != "well-known-id" {
		t.Errorf("MessageID = %v", msg.Properties.MessageID)
	}
	if msg.Properties.GroupSequence == nil || *msg.Properties.GroupSequence != 99 {
		t.Errorf("GroupSequence = %v (int conversion)", msg.Properties.GroupSequence)
	}
	if msg.ApplicationProperties != nil {
		t.Errorf("well-known headers should not appear in ApplicationProperties, got %v", msg.ApplicationProperties)
	}
}

func TestHeadersToMessage_EmptyHeaders(t *testing.T) {
	msg := headersToMessage(nil)

	if msg.Properties != nil {
		t.Error("Properties should be nil for empty headers")
	}
	if msg.ApplicationProperties != nil {
		t.Error("ApplicationProperties should be nil for empty headers")
	}
}

func TestHeadersToMessage_ReservedFiltered(t *testing.T) {
	headers := map[string]any{
		"x-bridge.route-id": "route-1",
		"custom":            "kept",
	}

	msg := headersToMessage(headers)

	if _, ok := msg.ApplicationProperties["x-bridge.route-id"]; ok {
		t.Error("reserved header should not appear in ApplicationProperties")
	}
	if msg.ApplicationProperties["custom"] != "kept" {
		t.Errorf("custom = %v, want %q", msg.ApplicationProperties["custom"], "kept")
	}
}

// TestHeadersToMessage_BridgeToBridgePropagated pins the central egress
// header policy: INTERNAL-ONLY and unclassified reserved keys are
// stripped, while BRIDGE-TO-BRIDGE PROPAGATED headers cross to the
// application properties so a peer bridge can correlate / deduplicate.
func TestHeadersToMessage_BridgeToBridgePropagated(t *testing.T) {
	headers := map[string]any{
		messaging.HeaderCorrelationID:  "corr-1",  // bridge-to-bridge -> propagate
		messaging.HeaderIdempotencyKey: "idem-1",  // bridge-to-bridge -> propagate
		messaging.HeaderTraceParent:    "00-tp",   // bridge-to-bridge (no prefix) -> propagate
		messaging.HeaderRouteID:        "route-1", // internal-only -> strip
		"x-bridge.session-id":          "sess-1",  // unclassified reserved -> strip
		"custom":                       "kept",    // application -> propagate
	}

	msg := headersToMessage(headers)

	if msg.ApplicationProperties[messaging.HeaderCorrelationID] != "corr-1" {
		t.Errorf("bridge-to-bridge correlation-id must propagate, got %v",
			msg.ApplicationProperties[messaging.HeaderCorrelationID])
	}
	if msg.ApplicationProperties[messaging.HeaderIdempotencyKey] != "idem-1" {
		t.Error("bridge-to-bridge idempotency-key must propagate")
	}
	if msg.ApplicationProperties[messaging.HeaderTraceParent] != "00-tp" {
		t.Error("bridge-to-bridge traceparent must propagate")
	}
	if _, ok := msg.ApplicationProperties[messaging.HeaderRouteID]; ok {
		t.Error("internal-only route-id must be stripped on egress")
	}
	if _, ok := msg.ApplicationProperties["x-bridge.session-id"]; ok {
		t.Error("unclassified reserved x-bridge.session-id must be stripped on egress")
	}
	if msg.ApplicationProperties["custom"] != "kept" {
		t.Error("application header must pass through")
	}
}

// TestEnvelopeToMessage_SubjectOnlyFromEnvelope is the finding-5
// regression guard: the egress AMQP Subject comes only from
// Envelope.Subject, never from the amqp10.subject header.
func TestEnvelopeToMessage_SubjectOnlyFromEnvelope(t *testing.T) {
	// Header carries a subject but Envelope.Subject is empty -> Subject
	// must stay unset (header must not leak into Properties.Subject).
	envNoSubject := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e1",
		Payload: []byte("x"),
		Headers: map[string]any{"amqp10.subject": "from-header"},
	})
	msg := envelopeToMessage(envNoSubject)
	if msg.Properties != nil && msg.Properties.Subject != nil {
		t.Errorf("Subject must not come from amqp10.subject header, got %q", *msg.Properties.Subject)
	}

	// Envelope.Subject set -> egress Subject reflects it, even when the
	// header disagrees.
	envWithSubject := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e2",
		Subject: "from-envelope",
		Payload: []byte("x"),
		Headers: map[string]any{"amqp10.subject": "from-header"},
	})
	msg2 := envelopeToMessage(envWithSubject)
	if msg2.Properties == nil || msg2.Properties.Subject == nil || *msg2.Properties.Subject != "from-envelope" {
		t.Errorf("Subject must come from Envelope.Subject, got %v", msg2.Properties.Subject)
	}
}

func TestHeadersToMessage_ContentEncoding(t *testing.T) {
	headers := map[string]any{
		"amqp10.content-encoding": "deflate",
	}

	msg := headersToMessage(headers)

	if msg.Properties == nil || msg.Properties.ContentEncoding == nil {
		t.Fatal("ContentEncoding should be set")
	}
	if *msg.Properties.ContentEncoding != "deflate" {
		t.Errorf("ContentEncoding = %q, want %q", *msg.Properties.ContentEncoding, "deflate")
	}
}
