package paho

import (
	"time"
	"unicode"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/domain"
)

const headerMQTTResponseTopic = "mqtt.response-topic"

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

// EnvelopeFromPublish converts an incoming MQTT publish into a domain.Envelope.
// Reserved x-bridge.* headers are stripped from user properties to prevent
// header injection from external sources. CorrelationData and ContentType are
// validated for length and character safety before being accepted.
func EnvelopeFromPublish(pub *pahov5.Publish) *domain.Envelope {
	now := time.Now()

	env := &domain.Envelope{
		Subject:   pub.Topic,
		Payload:   pub.Payload,
		CreatedAt: now,
	}

	headers := make(map[string]any)

	if pub.Properties != nil {
		if pub.Properties.CorrelationData != nil {
			corr := string(pub.Properties.CorrelationData)
			if len(corr) <= maxHeaderValueLen && isPrintableASCII(corr) {
				headers[domain.HeaderCorrelationID] = corr
			}
		}
		if pub.Properties.ContentType != "" {
			ct := pub.Properties.ContentType
			if len(ct) <= maxHeaderValueLen && isPrintableASCII(ct) {
				headers[domain.HeaderContentType] = ct
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
			if domain.IsReservedHeader(u.Key) {
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

	if len(headers) > 0 {
		env.Headers = headers
	}

	return env
}

// PublishFromEnvelope converts a domain.Envelope into an MQTT publish packet
// with mapped headers and message expiry.
func PublishFromEnvelope(env *domain.Envelope, opts SenderOptions) *pahov5.Publish {
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

	if env.HasExpiry() {
		remaining := env.RemainingTTL()
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
		if v, ok := domain.GetHeaderString(env.Headers, domain.HeaderCorrelationID); ok {
			props.CorrelationData = []byte(v)
			hasProps = true
		}
		if v, ok := domain.GetHeaderString(env.Headers, domain.HeaderContentType); ok {
			props.ContentType = v
			hasProps = true
		}
		if v, ok := domain.GetHeaderString(env.Headers, headerMQTTResponseTopic); ok {
			props.ResponseTopic = v
			hasProps = true
		}

		for k, v := range env.Headers {
			if k == domain.HeaderCorrelationID || k == domain.HeaderContentType || k == headerMQTTResponseTopic {
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
