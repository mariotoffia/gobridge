package paho

import (
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/domain"
)

// EnvelopeFromPublish converts an incoming MQTT publish into a domain.Envelope.
// Reserved x-bridge.* headers are stripped from user properties to prevent
// header injection from external sources.
func EnvelopeFromPublish(pub *pahov5.Publish) *domain.Envelope {
	env := &domain.Envelope{
		Subject:   pub.Topic,
		Payload:   pub.Payload,
		CreatedAt: time.Now(),
	}

	headers := make(map[string]any)

	if pub.Properties != nil {
		if pub.Properties.CorrelationData != nil {
			headers[domain.HeaderCorrelationID] = string(pub.Properties.CorrelationData)
		}
		if pub.Properties.ContentType != "" {
			headers[domain.HeaderContentType] = pub.Properties.ContentType
		}
		if pub.Properties.ResponseTopic != "" {
			headers["mqtt.response-topic"] = pub.Properties.ResponseTopic
		}
		if pub.Properties.MessageExpiry != nil {
			env.ExpiresAt = time.Now().Add(time.Duration(*pub.Properties.MessageExpiry) * time.Second)
		}

		for _, u := range pub.Properties.User {
			if domain.IsReservedHeader(u.Key) {
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
		if v, ok := domain.GetHeaderString(env.Headers, "mqtt.response-topic"); ok {
			props.ResponseTopic = v
			hasProps = true
		}

		for k, v := range env.Headers {
			if k == domain.HeaderCorrelationID || k == domain.HeaderContentType || k == "mqtt.response-topic" {
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
