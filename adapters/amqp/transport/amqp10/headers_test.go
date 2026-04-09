// Validates bidirectional mapping between AMQP 1.0 message properties and envelope headers.
package amqp10

import (
	"testing"
	"time"

	"github.com/Azure/go-amqp"
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
			MessageID:         "msg-123",
			CorrelationID:     "corr-456",
			ContentType:       &ct,
			ContentEncoding:   &ce,
			Subject:           &subj,
			To:                &to,
			ReplyTo:           &replyTo,
			GroupID:           &groupID,
			GroupSequence:     &groupSeq,
			ReplyToGroupID:    &replyGroup,
			CreationTime:      &created,
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
	if msg.Properties.Subject == nil || *msg.Properties.Subject != "evt.shipped" {
		t.Errorf("Subject = %v", msg.Properties.Subject)
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
