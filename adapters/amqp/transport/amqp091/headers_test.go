package amqp091

import (
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
)

func requireHeaderEqual(t *testing.T, headers map[string]any, key string, want any) {
	t.Helper()
	got, ok := headers[key]
	if !ok {
		t.Errorf("header %q: missing", key)
		return
	}
	if got != want {
		t.Errorf("header %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

func requireHeaderAbsent(t *testing.T, headers map[string]any, key string) {
	t.Helper()
	if v, ok := headers[key]; ok {
		t.Errorf("header %q should be absent, got %v", key, v)
	}
}

// verifies deliveryToHeaders maps all AMQP system properties.
func TestDeliveryToHeaders(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)

	d := amqp.Delivery{
		MessageId:       "msg-001",
		CorrelationId:   "corr-001",
		ContentType:     "application/json",
		ContentEncoding: "gzip",
		ReplyTo:         "reply-q",
		Type:            "order.created",
		AppId:           "myapp",
		DeliveryMode:    2,
		Priority:        5,
		Expiration:      "60000",
		Timestamp:       now,
		Exchange:        "orders",
		RoutingKey:      "order.new",
		DeliveryTag:     42,
		Redelivered:     true,
		ConsumerTag:     "ctag-1",
	}

	h := deliveryToHeaders(d)

	requireHeaderEqual(t, h, HeaderMessageID, "msg-001")
	requireHeaderEqual(t, h, HeaderCorrelationID, "corr-001")
	requireHeaderEqual(t, h, HeaderContentType, "application/json")
	requireHeaderEqual(t, h, HeaderContentEncoding, "gzip")
	requireHeaderEqual(t, h, HeaderReplyTo, "reply-q")
	requireHeaderEqual(t, h, HeaderType, "order.created")
	requireHeaderEqual(t, h, HeaderAppID, "myapp")
	requireHeaderEqual(t, h, HeaderDeliveryMode, uint8(2))
	requireHeaderEqual(t, h, HeaderPriority, uint8(5))
	requireHeaderEqual(t, h, HeaderExpiration, "60000")
	requireHeaderEqual(t, h, HeaderTimestamp, now)
	requireHeaderEqual(t, h, HeaderExchange, "orders")
	requireHeaderEqual(t, h, HeaderRoutingKey, "order.new")
	requireHeaderEqual(t, h, HeaderDeliveryTag, uint64(42))
	requireHeaderEqual(t, h, HeaderRedelivered, true)
	requireHeaderEqual(t, h, HeaderConsumerTag, "ctag-1")
}

// verifies deliveryToHeaders omits zero-value system properties.
func TestDeliveryToHeaders_ZeroValues(t *testing.T) {
	d := amqp.Delivery{
		DeliveryTag: 0,
		Redelivered: false,
	}

	h := deliveryToHeaders(d)

	requireHeaderEqual(t, h, HeaderDeliveryTag, uint64(0))
	requireHeaderEqual(t, h, HeaderRedelivered, false)

	requireHeaderAbsent(t, h, HeaderMessageID)
	requireHeaderAbsent(t, h, HeaderCorrelationID)
	requireHeaderAbsent(t, h, HeaderContentType)
	requireHeaderAbsent(t, h, HeaderContentEncoding)
	requireHeaderAbsent(t, h, HeaderReplyTo)
	requireHeaderAbsent(t, h, HeaderType)
	requireHeaderAbsent(t, h, HeaderAppID)
	requireHeaderAbsent(t, h, HeaderDeliveryMode)
	requireHeaderAbsent(t, h, HeaderPriority)
	requireHeaderAbsent(t, h, HeaderExpiration)
	requireHeaderAbsent(t, h, HeaderTimestamp)
	requireHeaderAbsent(t, h, HeaderExchange)
	requireHeaderAbsent(t, h, HeaderRoutingKey)
	requireHeaderAbsent(t, h, HeaderConsumerTag)
}

// verifies deliveryToHeaders strips x-bridge.* reserved headers from the AMQP table.
func TestDeliveryToHeaders_ReservedFiltered(t *testing.T) {
	d := amqp.Delivery{
		Headers: amqp.Table{
			domain.HeaderCorrelationID: "injected",
			domain.HeaderRouteID:       "injected-route",
			"safe-key":                 "keep-me",
		},
	}

	h := deliveryToHeaders(d)

	requireHeaderAbsent(t, h, domain.HeaderCorrelationID)
	requireHeaderAbsent(t, h, domain.HeaderRouteID)
	requireHeaderEqual(t, h, "safe-key", "keep-me")
}

// verifies deliveryToHeaders maps custom AMQP Table entries into the header map.
func TestDeliveryToHeaders_TableEntries(t *testing.T) {
	d := amqp.Delivery{
		Headers: amqp.Table{
			"tenant":  "acme",
			"version": int32(3),
			"flag":    true,
		},
	}

	h := deliveryToHeaders(d)

	requireHeaderEqual(t, h, "tenant", "acme")
	requireHeaderEqual(t, h, "version", int32(3))
	requireHeaderEqual(t, h, "flag", true)
}

// verifies headersToPublishing maps well-known amqp091.* headers back to AMQP properties.
func TestHeadersToPublishing_WellKnown(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)

	headers := map[string]any{
		HeaderMessageID:       "msg-100",
		HeaderCorrelationID:   "corr-100",
		HeaderContentType:     "text/plain",
		HeaderContentEncoding: "utf-8",
		HeaderReplyTo:         "reply-q",
		HeaderType:            "evt.shipped",
		HeaderAppID:           "sender-app",
		HeaderDeliveryMode:    uint8(2),
		HeaderPriority:        uint8(9),
		HeaderExpiration:      "30000",
		HeaderTimestamp:       now,
	}

	pub := headersToPublishing(headers)

	if pub.MessageId != "msg-100" {
		t.Errorf("MessageId = %q", pub.MessageId)
	}
	if pub.CorrelationId != "corr-100" {
		t.Errorf("CorrelationId = %q", pub.CorrelationId)
	}
	if pub.ContentType != "text/plain" {
		t.Errorf("ContentType = %q", pub.ContentType)
	}
	if pub.ContentEncoding != "utf-8" {
		t.Errorf("ContentEncoding = %q", pub.ContentEncoding)
	}
	if pub.ReplyTo != "reply-q" {
		t.Errorf("ReplyTo = %q", pub.ReplyTo)
	}
	if pub.Type != "evt.shipped" {
		t.Errorf("Type = %q", pub.Type)
	}
	if pub.AppId != "sender-app" {
		t.Errorf("AppId = %q", pub.AppId)
	}
	if pub.DeliveryMode != 2 {
		t.Errorf("DeliveryMode = %d", pub.DeliveryMode)
	}
	if pub.Priority != 9 {
		t.Errorf("Priority = %d", pub.Priority)
	}
	if pub.Expiration != "30000" {
		t.Errorf("Expiration = %q", pub.Expiration)
	}
	if pub.Timestamp != now {
		t.Errorf("Timestamp = %v", pub.Timestamp)
	}
	if pub.Headers != nil {
		t.Errorf("Headers table should be nil for well-known-only input, got %v", pub.Headers)
	}
}

// verifies headersToPublishing places non-amqp091 keys in the AMQP Headers table.
func TestHeadersToPublishing(t *testing.T) {
	headers := map[string]any{
		HeaderMessageID: "msg-200",
		"tenant":        "acme",
		"env":           "staging",
	}

	pub := headersToPublishing(headers)

	if pub.MessageId != "msg-200" {
		t.Errorf("MessageId = %q", pub.MessageId)
	}
	if pub.Headers == nil {
		t.Fatal("Headers table is nil")
	}
	if pub.Headers["tenant"] != "acme" {
		t.Errorf("Headers[tenant] = %v", pub.Headers["tenant"])
	}
	if pub.Headers["env"] != "staging" {
		t.Errorf("Headers[env] = %v", pub.Headers["env"])
	}
	if _, ok := pub.Headers[HeaderMessageID]; ok {
		t.Error("well-known header should not appear in table")
	}
}

// verifies headersToPublishing excludes amqp091-prefixed and reserved headers from the table.
func TestHeadersToPublishing_ExcludesReserved(t *testing.T) {
	headers := map[string]any{
		HeaderDeliveryTag:          uint64(1),
		HeaderRedelivered:         true,
		domain.HeaderCorrelationID: "injected",
		"custom":                   "keep",
	}

	pub := headersToPublishing(headers)

	if pub.Headers == nil {
		t.Fatal("Headers table is nil")
	}
	if _, ok := pub.Headers[HeaderDeliveryTag]; ok {
		t.Error("amqp091-prefixed key should not be in table")
	}
	if _, ok := pub.Headers[HeaderRedelivered]; ok {
		t.Error("amqp091-prefixed key should not be in table")
	}
	if _, ok := pub.Headers[domain.HeaderCorrelationID]; ok {
		t.Error("reserved header should not be in table")
	}
	if pub.Headers["custom"] != "keep" {
		t.Errorf("Headers[custom] = %v", pub.Headers["custom"])
	}
}

// verifies headersToPublishing handles nil headers.
func TestHeadersToPublishing_Nil(t *testing.T) {
	pub := headersToPublishing(nil)
	if pub.MessageId != "" {
		t.Errorf("MessageId = %q, want empty", pub.MessageId)
	}
	if pub.Headers != nil {
		t.Errorf("Headers = %v, want nil", pub.Headers)
	}
}

// verifies envelopeToPublishing maps envelope fields to an AMQP Publishing.
func TestEnvelopeToPublishing(t *testing.T) {
	env := &domain.Envelope{
		ID:      "env-001",
		Subject: "order.created",
		Payload: []byte(`{"order":1}`),
		Headers: map[string]any{
			HeaderContentType:     "application/json",
			domain.HeaderContentType: "application/json",
			"tenant":              "acme",
		},
	}

	pub := envelopeToPublishing(env, SenderConfig{})

	if string(pub.Body) != `{"order":1}` {
		t.Errorf("Body = %q", pub.Body)
	}
	if pub.MessageId != "env-001" {
		t.Errorf("MessageId = %q, want %q", pub.MessageId, "env-001")
	}
	if pub.ContentType != "application/json" {
		t.Errorf("ContentType = %q", pub.ContentType)
	}
}

// verifies envelopeToPublishing does not override MessageId if already set in headers.
func TestEnvelopeToPublishing_HeaderMessageIDPrecedence(t *testing.T) {
	env := &domain.Envelope{
		ID: "env-from-id",
		Headers: map[string]any{
			HeaderMessageID: "from-header",
		},
	}

	pub := envelopeToPublishing(env, SenderConfig{})

	if pub.MessageId != "from-header" {
		t.Errorf("MessageId = %q, want %q", pub.MessageId, "from-header")
	}
}

// verifies envelopeToPublishing sets expiration from envelope TTL.
func TestEnvelopeToPublishing_Expiry(t *testing.T) {
	env := &domain.Envelope{
		ID:        "env-ttl",
		ExpiresAt: time.Now().Add(5 * time.Second),
	}

	pub := envelopeToPublishing(env, SenderConfig{})

	if pub.Expiration == "" {
		t.Fatal("Expiration should be set for envelope with TTL")
	}
}

// verifies envelopeToPublishing falls back to x-bridge.content-type when
// amqp091.content-type header is absent.
func TestEnvelopeToPublishing_FallbackContentType(t *testing.T) {
	env := &domain.Envelope{
		ID: "env-ct",
		Headers: map[string]any{
			domain.HeaderContentType: "text/xml",
		},
	}

	pub := envelopeToPublishing(env, SenderConfig{})

	if pub.ContentType != "text/xml" {
		t.Errorf("ContentType = %q, want %q", pub.ContentType, "text/xml")
	}
}
