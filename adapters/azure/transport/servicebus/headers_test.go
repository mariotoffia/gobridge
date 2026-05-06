package servicebus

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

func strPtr(s string) *string               { return &s }
func durPtr(d time.Duration) *time.Duration { return &d }
func timePtr(t time.Time) *time.Time        { return &t }
func int64Ptr(n int64) *int64               { return &n }

func requireEqual(t *testing.T, key string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("header %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func requireAbsent(t *testing.T, headers map[string]any, key string) {
	t.Helper()
	if v, ok := headers[key]; ok {
		t.Errorf("header %q should be absent, got %v", key, v)
	}
}

// verifies messageToHeaders maps Service Bus system properties to header keys.
func TestMessageToHeaders_SystemProperties(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	ttl := 30 * time.Second
	var seqNum int64 = 42
	var deliveryCount uint32 = 3

	msg := &azservicebus.ReceivedMessage{
		MessageID:             "msg-001",
		CorrelationID:         strPtr("corr-001"),
		SessionID:             strPtr("sess-001"),
		ContentType:           strPtr("application/json"),
		Subject:               strPtr("order.created"),
		To:                    strPtr("queue-a"),
		ReplyTo:               strPtr("queue-b"),
		TimeToLive:            durPtr(ttl),
		EnqueuedTime:          timePtr(now),
		SequenceNumber:        int64Ptr(seqNum),
		DeliveryCount:         deliveryCount,
		ApplicationProperties: map[string]any{},
	}

	h := messageToHeaders(msg)

	requireEqual(t, asbHeaderMessageID, h[asbHeaderMessageID], "msg-001")
	requireEqual(t, asbHeaderCorrelationID, h[asbHeaderCorrelationID], "corr-001")
	requireEqual(t, asbHeaderSessionID, h[asbHeaderSessionID], "sess-001")
	requireEqual(t, asbHeaderContentType, h[asbHeaderContentType], "application/json")
	requireEqual(t, asbHeaderSubject, h[asbHeaderSubject], "order.created")
	requireEqual(t, asbHeaderTo, h[asbHeaderTo], "queue-a")
	requireEqual(t, asbHeaderReplyTo, h[asbHeaderReplyTo], "queue-b")
	requireEqual(t, asbHeaderTTL, h[asbHeaderTTL], ttl)
	requireEqual(t, asbHeaderEnqueuedTime, h[asbHeaderEnqueuedTime], now)
	requireEqual(t, asbHeaderSequenceNum, h[asbHeaderSequenceNum], seqNum)
	requireEqual(t, asbHeaderDeliveryCount, h[asbHeaderDeliveryCount], deliveryCount)
}

// verifies messageToHeaders omits headers when optional system properties are nil.
func TestMessageToHeaders_NilPointers(t *testing.T) {
	msg := &azservicebus.ReceivedMessage{
		MessageID: "msg-002",
	}

	h := messageToHeaders(msg)

	requireEqual(t, asbHeaderMessageID, h[asbHeaderMessageID], "msg-002")
	requireEqual(t, asbHeaderDeliveryCount, h[asbHeaderDeliveryCount], uint32(0))

	for _, key := range []string{
		asbHeaderCorrelationID,
		asbHeaderSessionID,
		asbHeaderContentType,
		asbHeaderSubject,
		asbHeaderTo,
		asbHeaderReplyTo,
		asbHeaderTTL,
		asbHeaderEnqueuedTime,
		asbHeaderSequenceNum,
	} {
		requireAbsent(t, h, key)
	}
}

// verifies messageToHeaders copies application properties into the header map.
func TestMessageToHeaders_ApplicationProperties(t *testing.T) {
	msg := &azservicebus.ReceivedMessage{
		MessageID: "msg-003",
		ApplicationProperties: map[string]any{
			"tenant":  "acme",
			"version": 2,
		},
	}

	h := messageToHeaders(msg)

	requireEqual(t, "tenant", h["tenant"], "acme")
	requireEqual(t, "version", h["version"], 2)
}

// verifies messageToHeaders strips bridge-reserved keys from application properties.
func TestMessageToHeaders_StripsReservedHeaders(t *testing.T) {
	msg := &azservicebus.ReceivedMessage{
		MessageID: "msg-004",
		ApplicationProperties: map[string]any{
			messaging.HeaderCorrelationID: "injected-corr",
			messaging.HeaderRouteID:       "injected-route",
			"safe-key":                    "keep-me",
		},
	}

	h := messageToHeaders(msg)

	requireAbsent(t, h, messaging.HeaderCorrelationID)
	requireAbsent(t, h, messaging.HeaderRouteID)
	requireEqual(t, "safe-key", h["safe-key"], "keep-me")
}

// verifies messageToHeaders combines system properties, delivery metadata, and custom properties.
func TestMessageToHeaders_MixedProperties(t *testing.T) {
	msg := &azservicebus.ReceivedMessage{
		MessageID:     "msg-005",
		CorrelationID: strPtr("corr-005"),
		Subject:       strPtr("evt.shipped"),
		DeliveryCount: 1,
		ApplicationProperties: map[string]any{
			"region":   "eu-west-1",
			"priority": "high",
		},
	}

	h := messageToHeaders(msg)

	requireEqual(t, asbHeaderMessageID, h[asbHeaderMessageID], "msg-005")
	requireEqual(t, asbHeaderCorrelationID, h[asbHeaderCorrelationID], "corr-005")
	requireEqual(t, asbHeaderSubject, h[asbHeaderSubject], "evt.shipped")
	requireEqual(t, asbHeaderDeliveryCount, h[asbHeaderDeliveryCount], uint32(1))
	requireEqual(t, "region", h["region"], "eu-west-1")
	requireEqual(t, "priority", h["priority"], "high")

	requireAbsent(t, h, asbHeaderSessionID)
	requireAbsent(t, h, asbHeaderTo)
}

// verifies headersToMessage maps ASB header keys onto azservicebus.Message system fields.
func TestHeadersToMessage_SystemProperties(t *testing.T) {
	ttl := 60 * time.Second
	headers := map[string]any{
		asbHeaderMessageID:     "msg-100",
		asbHeaderCorrelationID: "corr-100",
		asbHeaderSessionID:     "sess-100",
		asbHeaderContentType:   "text/plain",
		asbHeaderSubject:       "order.cancelled",
		asbHeaderTo:            "dest-queue",
		asbHeaderReplyTo:       "reply-queue",
		asbHeaderTTL:           ttl,
	}

	msg := headersToMessage(headers)

	if msg.MessageID == nil || *msg.MessageID != "msg-100" {
		t.Fatalf("MessageID = %v, want %q", msg.MessageID, "msg-100")
	}
	if msg.CorrelationID == nil || *msg.CorrelationID != "corr-100" {
		t.Fatalf("CorrelationID = %v, want %q", msg.CorrelationID, "corr-100")
	}
	if msg.SessionID == nil || *msg.SessionID != "sess-100" {
		t.Fatalf("SessionID = %v, want %q", msg.SessionID, "sess-100")
	}
	if msg.ContentType == nil || *msg.ContentType != "text/plain" {
		t.Fatalf("ContentType = %v, want %q", msg.ContentType, "text/plain")
	}
	if msg.Subject == nil || *msg.Subject != "order.cancelled" {
		t.Fatalf("Subject = %v, want %q", msg.Subject, "order.cancelled")
	}
	if msg.To == nil || *msg.To != "dest-queue" {
		t.Fatalf("To = %v, want %q", msg.To, "dest-queue")
	}
	if msg.ReplyTo == nil || *msg.ReplyTo != "reply-queue" {
		t.Fatalf("ReplyTo = %v, want %q", msg.ReplyTo, "reply-queue")
	}
	if msg.TimeToLive == nil || *msg.TimeToLive != ttl {
		t.Fatalf("TimeToLive = %v, want %v", msg.TimeToLive, ttl)
	}
}

// verifies headersToMessage places non-ASB keys in ApplicationProperties.
func TestHeadersToMessage_ApplicationProperties(t *testing.T) {
	headers := map[string]any{
		"tenant": "acme",
		"env":    "staging",
	}

	msg := headersToMessage(headers)

	if msg.ApplicationProperties == nil {
		t.Fatal("ApplicationProperties is nil")
	}
	if got := msg.ApplicationProperties["tenant"]; got != "acme" {
		t.Errorf("ApplicationProperties[tenant] = %v, want %q", got, "acme")
	}
	if got := msg.ApplicationProperties["env"]; got != "staging" {
		t.Errorf("ApplicationProperties[env] = %v, want %q", got, "staging")
	}
}

// verifies headersToMessage does not duplicate ASB-prefixed keys in ApplicationProperties.
func TestHeadersToMessage_ExcludesASBHeaders(t *testing.T) {
	headers := map[string]any{
		asbHeaderMessageID:     "msg-200",
		asbHeaderCorrelationID: "corr-200",
		asbHeaderTTL:           10 * time.Second,
		"custom-key":           "custom-val",
	}

	msg := headersToMessage(headers)

	if msg.ApplicationProperties == nil {
		t.Fatal("ApplicationProperties is nil")
	}
	if _, ok := msg.ApplicationProperties[asbHeaderMessageID]; ok {
		t.Error("ApplicationProperties should not contain asb.message-id")
	}
	if _, ok := msg.ApplicationProperties[asbHeaderCorrelationID]; ok {
		t.Error("ApplicationProperties should not contain asb.correlation-id")
	}
	if _, ok := msg.ApplicationProperties[asbHeaderTTL]; ok {
		t.Error("ApplicationProperties should not contain asb.ttl")
	}
	if got := msg.ApplicationProperties["custom-key"]; got != "custom-val" {
		t.Errorf("ApplicationProperties[custom-key] = %v, want %q", got, "custom-val")
	}
}

// verifies headersToMessage handles nil and empty header maps without setting IDs or application props.
func TestHeadersToMessage_NilHeaders(t *testing.T) {
	msg := headersToMessage(nil)

	if msg.MessageID != nil {
		t.Errorf("MessageID = %v, want nil", msg.MessageID)
	}
	if msg.ApplicationProperties != nil {
		t.Errorf("ApplicationProperties = %v, want nil", msg.ApplicationProperties)
	}

	msg2 := headersToMessage(map[string]any{})

	if msg2.MessageID != nil {
		t.Errorf("empty map: MessageID = %v, want nil", msg2.MessageID)
	}
	if msg2.ApplicationProperties != nil {
		t.Errorf("empty map: ApplicationProperties = %v, want nil", msg2.ApplicationProperties)
	}
}

// verifies headersToMessage sets TimeToLive from the ASB TTL header.
func TestHeadersToMessage_TTLRoundTrip(t *testing.T) {
	ttl := 5 * time.Minute

	headers := map[string]any{
		asbHeaderTTL: ttl,
	}

	msg := headersToMessage(headers)

	if msg.TimeToLive == nil {
		t.Fatal("TimeToLive is nil")
	}
	if *msg.TimeToLive != ttl {
		t.Errorf("TimeToLive = %v, want %v", *msg.TimeToLive, ttl)
	}
}
