package amqp10

import (
	"strings"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
)

// AMQP 1.0 message property header keys.
const (
	headerMessageID       = "amqp10.message-id"
	headerCorrelationID   = "amqp10.correlation-id"
	headerContentType     = "amqp10.content-type"
	headerContentEncoding = "amqp10.content-encoding"
	headerSubject         = "amqp10.subject"
	headerTo              = "amqp10.to"
	headerReplyTo         = "amqp10.reply-to"
	headerGroupID         = "amqp10.group-id"
	headerGroupSequence   = "amqp10.group-sequence"
	headerReplyToGroupID  = "amqp10.reply-to-group-id"
	headerCreationTime    = "amqp10.creation-time"
	headerAbsoluteExpiry  = "amqp10.absolute-expiry-time"
	headerDeliveryCount   = "amqp10.delivery-count"
)

const headerPrefix = "amqp10."

var wellKnownHeaders = map[string]bool{
	headerMessageID:       true,
	headerCorrelationID:   true,
	headerContentType:     true,
	headerContentEncoding: true,
	headerSubject:         true,
	headerTo:              true,
	headerReplyTo:         true,
	headerGroupID:         true,
	headerGroupSequence:   true,
	headerReplyToGroupID:  true,
	headerCreationTime:    true,
	headerAbsoluteExpiry:  true,
	headerDeliveryCount:   true,
}

// messageToHeaders maps AMQP 1.0 message properties and application
// properties to envelope headers. Reserved bridge headers are filtered.
func messageToHeaders(msg *amqp.Message) map[string]any {
	size := len(msg.ApplicationProperties) + 13
	h := make(map[string]any, size)

	if msg.Properties != nil {
		if msg.Properties.MessageID != nil {
			h[headerMessageID] = msg.Properties.MessageID
		}
		if msg.Properties.CorrelationID != nil {
			h[headerCorrelationID] = msg.Properties.CorrelationID
		}
		if msg.Properties.ContentType != nil {
			h[headerContentType] = *msg.Properties.ContentType
		}
		if msg.Properties.ContentEncoding != nil {
			h[headerContentEncoding] = *msg.Properties.ContentEncoding
		}
		if msg.Properties.Subject != nil {
			h[headerSubject] = *msg.Properties.Subject
		}
		if msg.Properties.To != nil {
			h[headerTo] = *msg.Properties.To
		}
		if msg.Properties.ReplyTo != nil {
			h[headerReplyTo] = *msg.Properties.ReplyTo
		}
		if msg.Properties.GroupID != nil {
			h[headerGroupID] = *msg.Properties.GroupID
		}
		if msg.Properties.GroupSequence != nil {
			h[headerGroupSequence] = *msg.Properties.GroupSequence
		}
		if msg.Properties.ReplyToGroupID != nil {
			h[headerReplyToGroupID] = *msg.Properties.ReplyToGroupID
		}
		if msg.Properties.CreationTime != nil {
			h[headerCreationTime] = *msg.Properties.CreationTime
		}
		if msg.Properties.AbsoluteExpiryTime != nil {
			h[headerAbsoluteExpiry] = *msg.Properties.AbsoluteExpiryTime
		}
	}

	if msg.Header != nil {
		h[headerDeliveryCount] = msg.Header.DeliveryCount
	}

	for k, v := range msg.ApplicationProperties {
		if domain.IsReservedHeader(k) || strings.HasPrefix(k, headerPrefix) {
			continue
		}
		h[k] = v
	}

	return h
}

// headersToMessage maps envelope headers back to AMQP 1.0 message
// properties and application properties.
func headersToMessage(headers map[string]any) *amqp.Message {
	msg := &amqp.Message{}
	props := &amqp.MessageProperties{}
	hasProps := false

	if v, ok := headers[headerMessageID]; ok {
		props.MessageID = v
		hasProps = true
	}
	if v, ok := headers[headerCorrelationID]; ok {
		props.CorrelationID = v
		hasProps = true
	}
	if v, ok := headers[headerContentType]; ok {
		if s, ok := v.(string); ok {
			props.ContentType = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerContentEncoding]; ok {
		if s, ok := v.(string); ok {
			props.ContentEncoding = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerSubject]; ok {
		if s, ok := v.(string); ok {
			props.Subject = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerTo]; ok {
		if s, ok := v.(string); ok {
			props.To = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerReplyTo]; ok {
		if s, ok := v.(string); ok {
			props.ReplyTo = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerGroupID]; ok {
		if s, ok := v.(string); ok {
			props.GroupID = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerGroupSequence]; ok {
		switch n := v.(type) {
		case uint32:
			props.GroupSequence = &n
			hasProps = true
		case int:
			if n >= 0 {
				u := uint32(n)
				props.GroupSequence = &u
				hasProps = true
			}
		}
	}
	if v, ok := headers[headerReplyToGroupID]; ok {
		if s, ok := v.(string); ok {
			props.ReplyToGroupID = &s
			hasProps = true
		}
	}
	if v, ok := headers[headerCreationTime]; ok {
		if t, ok := v.(time.Time); ok {
			props.CreationTime = &t
			hasProps = true
		}
	}
	if v, ok := headers[headerAbsoluteExpiry]; ok {
		if t, ok := v.(time.Time); ok {
			props.AbsoluteExpiryTime = &t
			hasProps = true
		}
	}

	if hasProps {
		msg.Properties = props
	}

	var appProps map[string]any
	for k, v := range headers {
		if wellKnownHeaders[k] || strings.HasPrefix(k, headerPrefix) {
			continue
		}
		if domain.IsReservedHeader(k) {
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
