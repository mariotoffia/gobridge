package servicebus

import (
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
)

const (
	asbHeaderMessageID     = "asb.message-id"
	asbHeaderCorrelationID = "asb.correlation-id"
	asbHeaderSessionID     = "asb.session-id"
	asbHeaderContentType   = "asb.content-type"
	asbHeaderSubject       = "asb.subject"
	asbHeaderTo            = "asb.to"
	asbHeaderReplyTo       = "asb.reply-to"
	asbHeaderTTL           = "asb.ttl"
	asbHeaderEnqueuedTime  = "asb.enqueued-time"
	asbHeaderSequenceNum   = "asb.sequence-number"
	asbHeaderDeliveryCount = "asb.delivery-count"
)

var asbWellKnownHeaders = map[string]bool{
	asbHeaderMessageID:     true,
	asbHeaderCorrelationID: true,
	asbHeaderSessionID:     true,
	asbHeaderContentType:   true,
	asbHeaderSubject:       true,
	asbHeaderTo:            true,
	asbHeaderReplyTo:       true,
	asbHeaderTTL:           true,
	asbHeaderEnqueuedTime:  true,
	asbHeaderSequenceNum:   true,
	asbHeaderDeliveryCount: true,
}

func messageToHeaders(msg *azservicebus.ReceivedMessage) map[string]any {
	h := make(map[string]any, len(msg.ApplicationProperties)+11)

	h[asbHeaderMessageID] = msg.MessageID
	h[asbHeaderDeliveryCount] = msg.DeliveryCount

	if msg.CorrelationID != nil {
		h[asbHeaderCorrelationID] = *msg.CorrelationID
	}
	if msg.SessionID != nil {
		h[asbHeaderSessionID] = *msg.SessionID
	}
	if msg.ContentType != nil {
		h[asbHeaderContentType] = *msg.ContentType
	}
	if msg.Subject != nil {
		h[asbHeaderSubject] = *msg.Subject
	}
	if msg.To != nil {
		h[asbHeaderTo] = *msg.To
	}
	if msg.ReplyTo != nil {
		h[asbHeaderReplyTo] = *msg.ReplyTo
	}
	if msg.TimeToLive != nil {
		h[asbHeaderTTL] = *msg.TimeToLive
	}
	if msg.EnqueuedTime != nil {
		h[asbHeaderEnqueuedTime] = *msg.EnqueuedTime
	}
	if msg.SequenceNumber != nil {
		h[asbHeaderSequenceNum] = *msg.SequenceNumber
	}

	for k, v := range msg.ApplicationProperties {
		if domain.IsReservedHeader(k) {
			continue
		}
		h[k] = v
	}

	return h
}

func headersToMessage(headers map[string]any) *azservicebus.Message {
	msg := &azservicebus.Message{}

	if v, ok := headers[asbHeaderMessageID]; ok {
		if s, ok := v.(string); ok {
			msg.MessageID = &s
		}
	}
	if v, ok := headers[asbHeaderCorrelationID]; ok {
		if s, ok := v.(string); ok {
			msg.CorrelationID = &s
		}
	}
	if v, ok := headers[asbHeaderSessionID]; ok {
		if s, ok := v.(string); ok {
			msg.SessionID = &s
		}
	}
	if v, ok := headers[asbHeaderContentType]; ok {
		if s, ok := v.(string); ok {
			msg.ContentType = &s
		}
	}
	if v, ok := headers[asbHeaderSubject]; ok {
		if s, ok := v.(string); ok {
			msg.Subject = &s
		}
	}
	if v, ok := headers[asbHeaderTo]; ok {
		if s, ok := v.(string); ok {
			msg.To = &s
		}
	}
	if v, ok := headers[asbHeaderReplyTo]; ok {
		if s, ok := v.(string); ok {
			msg.ReplyTo = &s
		}
	}
	if v, ok := headers[asbHeaderTTL]; ok {
		var d time.Duration
		switch val := v.(type) {
		case time.Duration:
			d = val
		case string:
			if parsed, err := time.ParseDuration(val); err == nil {
				d = parsed
			}
		case int:
			d = time.Duration(val) * time.Second
		case int64:
			d = time.Duration(val) * time.Second
		case float64:
			d = time.Duration(val * float64(time.Second))
		}
		if d > 0 {
			msg.TimeToLive = &d
		}
	}

	var appProps map[string]any
	for k, v := range headers {
		if asbWellKnownHeaders[k] || strings.HasPrefix(k, "asb.") {
			continue
		}
		if appProps == nil {
			appProps = make(map[string]any, len(headers))
		}
		appProps[k] = v
	}
	msg.ApplicationProperties = appProps

	return msg
}
