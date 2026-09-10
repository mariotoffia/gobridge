package paho

import (
	"fmt"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// ingressCapViolation reports whether pub violates a LOCAL representational
// ingress cap — max_payload_bytes, the User Property count cap, or the
// metadata byte cap — returning a bounded class name (for log dedup) and the
// violation. These caps are NOT broker-enforceable: the CONNECT advertises
// only the whole-packet Maximum Packet Size (max_payload_bytes + the metadata
// allowance), so a compliant broker forwards ANY packet whose total fits —
// including one whose payload alone, metadata alone, or property count
// exceeds the local cap. Such a packet is producible by any authorized
// publisher, so it MUST NOT latch the session terminal: an un-acked poison
// packet is redelivered on every clean_start=false resume and would re-latch
// the session forever, a publisher-triggerable permanent kill switch
// The caller acks-and-drops it instead (dropPoisonIngress).
// Violations only a NON-compliant broker can produce (malformed structure,
// total packet size above the advertised maximum) are rejected terminally by
// the raw pre-decode guard (ingress_conn.go) and never reach this callback.
func (r *router) ingressCapViolation(pub *pahov5.Publish) (class string, violation error) {
	if pub == nil {
		return "", nil
	}
	userProperties := 0
	if pub.Properties != nil {
		userProperties = len(pub.Properties.User)
	}
	switch {
	case r.maxPayloadBytes > 0 && uint64(len(pub.Payload)) > uint64(r.maxPayloadBytes):
		return "payload", shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: inbound payload %d exceeds max_payload_bytes %d",
			len(pub.Payload), r.maxPayloadBytes,
		))
	case userProperties > maxIngressUserProperties:
		return "user_properties", shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: inbound User Properties count %d exceeds retained-memory cap %d",
			userProperties, maxIngressUserProperties,
		))
	case ingressMetadataBytes(pub) > maxIngressMetadataBytes:
		return "metadata", shared.ErrInvalidPayload.WithMessage(fmt.Sprintf(
			"mqtt: inbound metadata exceeds retained-memory cap %d bytes",
			maxIngressMetadataBytes,
		))
	}
	return "", nil
}

// dropPoisonIngress acks-and-drops one publish that violates a local
// representational ingress cap. The ack is the escape hatch that
// keeps a spec-legal-but-refused publish from becoming a session kill
// switch: it frees the broker's Receive-Maximum in-flight slot and stops
// redelivery, at the deliberate cost of acknowledged loss counted on
// MetricMQTTIngressPoisonDropped — alert on any non-zero value and find the
// publisher (docs/runbooks/mqtt-ingress-poison.md). QoS 0 carries no ack
// (nil ack). A failed ack (e.g. the connection cycled, ErrPacketNotFound)
// is logged at Debug and left to broker redelivery — the redelivered copy
// is dropped and counted again here, never dispatched. The Error log fires
// once per violation class per router lifetime (bounded, never keyed by
// attacker-controlled topic); the metric counts every drop.
func (r *router) dropPoisonIngress(pub *pahov5.Publish, class string, violation error, ack func() error) {
	r.dropCount.Add(1)
	r.poisonDropped.Add(1)
	r.metrics.Counter(MetricMQTTIngressPoisonDropped, 1, r.sessionTag()...)
	if ack != nil {
		if err := ack(); err != nil {
			logging.Debug(r.logger, "mqtt: ack of poison-dropped publish failed; broker will redeliver and it will be dropped again",
				"topic", pub.Topic, "error", err)
		}
	}
	r.mu.Lock()
	_, logged := r.poisonLogged[class]
	if !logged {
		r.poisonLogged[class] = struct{}{}
	}
	r.mu.Unlock()
	if r.logger == nil {
		return
	}
	if logged {
		logging.Debug(r.logger, "mqtt: acked-and-dropped inbound packet violating a local ingress cap",
			"topic", pub.Topic, "qos", pub.QoS, "class", class, "error", violation)
		return
	}
	r.logger.Error("mqtt: acked-and-dropped inbound packet violating a local ingress cap the broker "+
		"cannot enforce — an authorized publisher is sending packets this bridge is configured to refuse. "+
		"The MESSAGE IS LOST by design (acking is what prevents a permanent redelivery/terminal loop); "+
		"further drops of this class log at Debug, MQTTIngressPoisonDropped counts every one. "+
		"See docs/runbooks/mqtt-ingress-poison.md",
		"topic", pub.Topic,
		"qos", pub.QoS,
		"class", class,
		"payload_bytes", len(pub.Payload),
		"max_payload_bytes", r.maxPayloadBytes,
		"error", violation,
	)
}

func ingressMetadataBytes(pub *pahov5.Publish) uint64 {
	if pub == nil {
		return 0
	}
	// Fixed header + worst-case Remaining Length + topic length prefix +
	// packet identifier + worst-case property-length VBI.
	total := uint64(1 + 4 + 2 + len(pub.Topic) + 2 + 4)
	if pub.Properties == nil {
		return total
	}
	props := pub.Properties
	if props.PayloadFormat != nil {
		total += 2
	}
	if props.MessageExpiry != nil {
		total += 5
	}
	if props.ContentType != "" {
		total += uint64(3 + len(props.ContentType))
	}
	if props.ResponseTopic != "" {
		total += uint64(3 + len(props.ResponseTopic))
	}
	if props.CorrelationData != nil {
		total += uint64(3 + len(props.CorrelationData))
	}
	if props.SubscriptionIdentifier != nil {
		total += 5
	}
	if props.TopicAlias != nil {
		total += 3
	}
	for _, property := range props.User {
		total += uint64(5 + len(property.Key) + len(property.Value))
	}
	return total
}
