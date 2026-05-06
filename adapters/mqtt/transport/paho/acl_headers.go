package paho

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"
	"unicode"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

const headerMQTTResponseTopic = "mqtt.response-topic"

// HeaderMessageID is the user-property key used to round-trip the
// domain Envelope.ID through MQTT. On receive, this header takes
// precedence for setting Envelope.ID; the correlation-id header is
// the second choice, and a deterministic hash is the fallback.
const HeaderMessageID = "mqtt.message-id"

const maxHeaderValueLen = 256

// isPrintableASCII reports whether every byte in s is printable ASCII (0x20–0x7E).
func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// EnvelopeFromPublish converts an incoming MQTT publish into a messaging.Envelope.
// Reserved x-bridge.* headers are stripped from user properties to prevent
// header injection from external sources. CorrelationData and ContentType are
// validated for length and character safety before being accepted.
//
// Envelope.ID is resolved in priority order:
//  1. mqtt.message-id user property (set by PublishFromEnvelope)
//  2. x-bridge.correlation-id from CorrelationData
//  3. Deterministic derivation from topic + payload hash
func EnvelopeFromPublish(pub *pahov5.Publish, clk clock.Clock) *messaging.Envelope {
	if clk == nil {
		clk = clock.System
	}
	now := clk.Now()

	env := &messaging.Envelope{
		Subject:   pub.Topic,
		Payload:   pub.Payload,
		CreatedAt: now,
	}

	headers := make(map[string]any)
	var mqttMsgID string

	if pub.Properties != nil {
		if pub.Properties.CorrelationData != nil {
			corr := string(pub.Properties.CorrelationData)
			if len(corr) <= maxHeaderValueLen && isPrintableASCII(corr) {
				headers[messaging.HeaderCorrelationID] = corr
			}
		}
		if pub.Properties.ContentType != "" {
			ct := pub.Properties.ContentType
			if len(ct) <= maxHeaderValueLen && isPrintableASCII(ct) {
				headers[messaging.HeaderContentType] = ct
			}
		}
		if pub.Properties.ResponseTopic != "" {
			rt := pub.Properties.ResponseTopic
			if len(rt) <= maxHeaderValueLen && isPrintableASCII(rt) {
				headers[headerMQTTResponseTopic] = rt
			}
		}
		if pub.Properties.MessageExpiry != nil {
			env.ExpiresAt = now.Add(time.Duration(*pub.Properties.MessageExpiry) * time.Second)
		}

		for _, u := range pub.Properties.User {
			if u.Key == HeaderMessageID {
				if len(u.Value) <= maxHeaderValueLen && isPrintableASCII(u.Value) {
					mqttMsgID = u.Value
					headers[HeaderMessageID] = u.Value
				}
				continue
			}
			if messaging.IsReservedHeader(u.Key) {
				continue
			}
			if len(u.Key) > maxHeaderValueLen || len(u.Value) > maxHeaderValueLen {
				continue
			}
			if !isPrintableASCII(u.Key) || !isPrintableASCII(u.Value) {
				continue
			}
			headers[u.Key] = u.Value
		}
	}

	switch {
	case mqttMsgID != "":
		env.ID = mqttMsgID
	case headers[messaging.HeaderCorrelationID] != nil:
		env.ID, _ = headers[messaging.HeaderCorrelationID].(string)
	}
	if env.ID == "" {
		env.ID = generateEnvelopeID()
	}

	if len(headers) > 0 {
		env.Headers = headers
	}

	return env
}

// PublishFromEnvelope converts a messaging.Envelope into an MQTT publish packet
// with mapped headers and message expiry. The Envelope.ID is included as a
// mqtt.message-id user property so EnvelopeFromPublish can recover it.
func PublishFromEnvelope(env *messaging.Envelope, opts SenderOptions, clk clock.Clock) *pahov5.Publish {
	if clk == nil {
		clk = clock.System
	}
	topic := env.Subject
	if topic == "" {
		topic = opts.DefaultTopic
	}

	pub := &pahov5.Publish{
		Topic:   topic,
		QoS:     opts.QoS,
		Retain:  opts.Retain,
		Payload: env.Payload,
	}

	props := &pahov5.PublishProperties{}
	hasProps := false

	if env.ID != "" {
		props.User = append(props.User, pahov5.UserProperty{
			Key: HeaderMessageID, Value: env.ID,
		})
		hasProps = true
	}

	if env.HasExpiry() {
		remaining := env.RemainingTTL(clk)
		if remaining > 0 {
			secs := uint32(remaining.Seconds())
			if secs == 0 {
				secs = 1
			}
			props.MessageExpiry = &secs
			hasProps = true
		}
	}

	if env.Headers != nil {
		if v, ok := messaging.GetHeaderString(env.Headers, messaging.HeaderCorrelationID); ok {
			props.CorrelationData = []byte(v)
			hasProps = true
		}
		if v, ok := messaging.GetHeaderString(env.Headers, messaging.HeaderContentType); ok {
			props.ContentType = v
			hasProps = true
		}
		if v, ok := messaging.GetHeaderString(env.Headers, headerMQTTResponseTopic); ok {
			props.ResponseTopic = v
			hasProps = true
		}

		for k, v := range env.Headers {
			if k == messaging.HeaderCorrelationID || k == messaging.HeaderContentType ||
				k == headerMQTTResponseTopic || k == HeaderMessageID {
				continue
			}
			s, ok := v.(string)
			if !ok {
				continue
			}
			props.User = append(props.User, pahov5.UserProperty{Key: k, Value: s})
			hasProps = true
		}
	}

	if hasProps {
		pub.Properties = props
	}

	return pub
}

// generateEnvelopeID returns a random 16-byte hex string used as a
// last-resort Envelope.ID when no header or payload derivation is available.
func generateEnvelopeID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("paho: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
