package paho

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
	"unicode"
	"unicode/utf8"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

const headerMQTTResponseTopic = "mqtt.response-topic"

// HeaderMessageID is the user-property key used to round-trip the
// domain Envelope.ID through MQTT. On receive, this header takes
// precedence for setting Envelope.ID; the correlation-id header is
// the second choice, and a deterministic hash is the fallback.
const HeaderMessageID = "mqtt.message-id"

// HeaderGobridgeSubject is the user-property key used to round-trip
// the logical Envelope.Subject through MQTT, distinct from the
// transport-level publish topic. PublishFromEnvelope writes this
// property when env.Subject() is non-empty; EnvelopeFromPublish reads
// it back into env.Subject(). Inbound user properties carrying this
// key from a peer bridge are honoured (subject-preserving round
// trip is intentional); broker-injected duplicates lose to typed
// extraction because the generic user-property loop skips this
// reserved key.
const HeaderGobridgeSubject = "gobridge.subject"

// HeaderMQTTTopic is the envelope-headers key under which the
// MQTT publish topic (transport-level destination) is recorded on
// receive. It is set unconditionally by EnvelopeFromPublish and is
// adapter-controlled: inbound user properties literally named
// "mqtt.topic" are dropped to prevent a hostile broker from
// spoofing the recorded transport address.
const HeaderMQTTTopic = "mqtt.topic"

// HeaderMQTTRetained is the envelope-headers key recording whether the
// inbound MQTT publish carried the RETAIN flag (a broker-retained
// message replayed on subscribe, not a fresh event). It is set
// unconditionally by EnvelopeFromPublish (bool value) and is
// adapter-controlled: inbound user properties literally named
// "mqtt.retained" are dropped so a peer cannot spoof retained state.
const HeaderMQTTRetained = "mqtt.retained"

// HeaderMQTTQoS is the envelope-headers key recording the MQTT QoS level
// (0, 1 or 2) the publish was delivered at. It is set unconditionally by
// EnvelopeFromPublish (int value) and is adapter-controlled: inbound user
// properties literally named "mqtt.qos" are dropped so a peer cannot
// spoof the delivery QoS.
const HeaderMQTTQoS = "mqtt.qos"

const maxHeaderValueLen = 256

// isSafeHeaderValue reports whether s is a safe MQTT header string: valid
// UTF-8 with no control characters. MQTT v5 user properties, ContentType,
// ResponseTopic and CorrelationData are UTF-8 strings by spec, so this
// admits legal non-ASCII values (e.g. "Malmö") that the previous
// printable-ASCII-only filter dropped silently (M-2). Control characters
// (including NUL, newline, and C1 controls) are still rejected to prevent
// log/header injection; invalid UTF-8 (arbitrary binary, e.g. non-text
// CorrelationData) is rejected because it cannot round-trip as a header.
func isSafeHeaderValue(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
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
// The MQTT publish topic (transport-level destination) is recorded under
// the HeaderMQTTTopic envelope-headers key, distinct from the logical
// Envelope.Subject. Envelope.Subject is populated only from the
// HeaderGobridgeSubject user property; if absent, Subject is left empty.
//
// Envelope.ID is resolved in priority order:
//  1. mqtt.message-id user property (set by PublishFromEnvelope)
//  2. x-bridge.correlation-id from CorrelationData
//  3. Deterministic derivation from topic + payload hash
//
// The *pahov5.Publish parameter is the SDK boundary input this ACL
// helper exists to translate; its only callers are ACL files
// (acl_router.go), so the SDK type does not cross into port-side code.
//
// An optional MetricsExporter (variadic, mirroring PublishFromEnvelope)
// counts application/bridge user properties dropped by the safety filter
// (unsafe key/value or over-length) on MetricMQTTIngressHeaderDropped, so
// the otherwise-silent ingress drop is observable (M-2). Reserved and
// adapter-controlled keys stripped by policy are NOT counted. When no
// exporter is supplied the drop is still applied, just uncounted
// (test/legacy call sites).
//
//aclcheck:allow-export
func EnvelopeFromPublish(pub *pahov5.Publish, clk clock.Clock, metrics ...ports.MetricsExporter) *messaging.Envelope {
	if clk == nil {
		clk = clock.System
	}
	now := clk.Now()

	headers := map[string]any{
		HeaderMQTTTopic:    pub.Topic,
		HeaderMQTTRetained: pub.Retain,
		HeaderMQTTQoS:      int(pub.QoS),
	}
	var mqttMsgID string
	var subject string
	var expiresAt time.Time
	// droppedHeaders counts every inbound header that fails the length/safety
	// filter — both MQTT v5 properties (correlation data, content type, response
	// topic) and arbitrary user properties. It feeds MetricMQTTIngressHeaderDropped
	// so a correlation-id loss is observable rather than silent (A-14).
	droppedHeaders := 0

	if pub.Properties != nil {
		if pub.Properties.CorrelationData != nil {
			corr := string(pub.Properties.CorrelationData)
			if len(corr) <= maxHeaderValueLen && isSafeHeaderValue(corr) {
				headers[messaging.HeaderCorrelationID] = corr
			} else {
				droppedHeaders++
			}
		}
		if pub.Properties.ContentType != "" {
			ct := pub.Properties.ContentType
			if len(ct) <= maxHeaderValueLen && isSafeHeaderValue(ct) {
				headers[messaging.HeaderContentType] = ct
			} else {
				droppedHeaders++
			}
		}
		if pub.Properties.ResponseTopic != "" {
			rt := pub.Properties.ResponseTopic
			if len(rt) <= maxHeaderValueLen && isSafeHeaderValue(rt) {
				headers[headerMQTTResponseTopic] = rt
			} else {
				droppedHeaders++
			}
		}
		if pub.Properties.MessageExpiry != nil {
			expiresAt = now.Add(time.Duration(*pub.Properties.MessageExpiry) * time.Second)
		}

		for _, u := range pub.Properties.User {
			if u.Key == HeaderGobridgeSubject {
				if len(u.Value) <= maxHeaderValueLen && isSafeHeaderValue(u.Value) {
					subject = u.Value
				}
				continue
			}
			if u.Key == HeaderMQTTTopic || u.Key == HeaderMQTTRetained || u.Key == HeaderMQTTQoS {
				// Adapter-controlled: never let an inbound user property
				// override the recorded transport-level topic, retained
				// flag, or delivery QoS.
				continue
			}
			if u.Key == HeaderMessageID {
				if len(u.Value) <= maxHeaderValueLen && isSafeHeaderValue(u.Value) {
					mqttMsgID = u.Value
					headers[HeaderMessageID] = u.Value
				}
				continue
			}
			if messaging.IsReservedHeader(u.Key) {
				continue
			}
			if len(u.Key) > maxHeaderValueLen || len(u.Value) > maxHeaderValueLen {
				droppedHeaders++
				continue
			}
			if !isSafeHeaderValue(u.Key) || !isSafeHeaderValue(u.Value) {
				droppedHeaders++
				continue
			}
			headers[u.Key] = u.Value
		}
	}

	if droppedHeaders > 0 && len(metrics) > 0 && metrics[0] != nil {
		metrics[0].Counter(MetricMQTTIngressHeaderDropped, int64(droppedHeaders))
	}

	// Determine ID before construction — MustEnvelope requires a non-empty ID.
	var id string
	switch {
	case mqttMsgID != "":
		id = mqttMsgID
	case headers[messaging.HeaderCorrelationID] != nil:
		id, _ = headers[messaging.HeaderCorrelationID].(string)
	}
	if id == "" {
		id = deriveEnvelopeID(pub.Topic, pub.Payload)
	}

	// id is always non-empty (generate fallback above); now is non-zero.
	// NewEnvelope cannot fail here; the panic guards an impossible branch.
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:        id,
		Subject:   subject,
		Payload:   pub.Payload,
		CreatedAt: now,
	}, now)
	if err != nil {
		panic("paho: EnvelopeFromPublish: impossible construction error: " + err.Error())
	}

	// Headers carry adapter-stamped reserved keys (correlation ID,
	// content type, …) that are sourced from MQTT5 PUBLISH properties
	// — the broker controls them, not external user properties. Use
	// the trusted whole-map setter so those are not stripped; user
	// properties are filtered through IsReservedHeader above.
	env.StampHeaders(headers)

	if !expiresAt.IsZero() {
		// expiresAt = now + positive duration, so Before(createdAt) is
		// unreachable here; ignore the error to satisfy errcheck.
		_ = env.SetExpiry(expiresAt)
	}

	return env
}

// PublishFromEnvelope converts a messaging.Envelope into an MQTT publish packet
// with mapped headers and message expiry. The publish topic is supplied by
// the caller (typically resolved from OutboundMessage.Address or
// SenderOptions.DefaultTopic) — Envelope.Subject is the logical event
// subject, NOT the transport address, and is round-tripped via the
// HeaderGobridgeSubject user property. The Envelope.ID is included as a
// HeaderMessageID user property so EnvelopeFromPublish can recover it.
//
// Egress header policy: INTERNAL-ONLY reserved headers
// (messaging.IsInternalOnlyHeader — route-id, route-override, source-id,
// content-type) are NOT serialized as MQTT user properties, so the bridge's
// private dispatch bookkeeping never leaks to a non-bridge subscriber.
// content-type is instead mapped to the native MQTT ContentType property.
// BRIDGE-TO-BRIDGE propagated headers (correlation, causation, idempotency,
// dedup, ordering, tenant, forwarded, trace) and application headers pass
// through so a peer bridge can correlate, deduplicate and continue a trace.
//
// The returned *pahov5.Publish is the SDK boundary output this ACL helper
// exists to produce; it is consumed by the pahoConn ACL seam
// (acl_client.go PublishEnvelope), which is the single production egress
// path since the Sender was routed through the seam (F-2).
//
// An optional MetricsExporter (variadic, mirroring NewSession) is used to
// count bridge-to-bridge / application headers dropped because their value
// is not a string (MQTT-N1): such a value cannot become an MQTT user
// property, so the drop is recorded via MetricMQTTNonStringHeaderDropped
// instead of vanishing silently. When no exporter is supplied the drop is
// still applied, just uncounted (test/legacy call sites).
//
//aclcheck:allow-export
func PublishFromEnvelope(env *messaging.Envelope, topic string, opts SenderOptions, clk clock.Clock, metrics ...ports.MetricsExporter) *pahov5.Publish {
	if clk == nil {
		clk = clock.System
	}

	pub := &pahov5.Publish{
		Topic:   topic,
		QoS:     opts.QoS,
		Retain:  opts.Retain,
		Payload: env.Payload(),
	}

	props := &pahov5.PublishProperties{}
	hasProps := false
	droppedNonString := 0

	if env.ID() != "" {
		props.User = append(props.User, pahov5.UserProperty{
			Key: HeaderMessageID, Value: env.ID(),
		})
		hasProps = true
	}

	if env.Subject() != "" {
		props.User = append(props.User, pahov5.UserProperty{
			Key: HeaderGobridgeSubject, Value: env.Subject(),
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

	if env.Headers() != nil {
		if v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID); ok {
			props.CorrelationData = []byte(v)
			hasProps = true
		}
		if v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderContentType); ok {
			props.ContentType = v
			hasProps = true
		}
		if v, ok := messaging.GetHeaderString(env.Headers(), headerMQTTResponseTopic); ok {
			props.ResponseTopic = v
			hasProps = true
		}

		for k, v := range env.Headers() {
			if k == messaging.HeaderCorrelationID || k == messaging.HeaderContentType ||
				k == headerMQTTResponseTopic || k == HeaderMessageID ||
				k == HeaderGobridgeSubject || k == HeaderMQTTTopic ||
				k == HeaderMQTTRetained || k == HeaderMQTTQoS {
				continue
			}
			// Egress header policy: strip INTERNAL-ONLY reserved headers
			// (route-id, route-override, source-id, content-type) so the
			// bridge's private dispatch bookkeeping is never serialized as
			// MQTT user properties to a non-bridge subscriber. BRIDGE-TO-
			// BRIDGE propagated headers (correlation/causation/idempotency/
			// dedup/ordering/tenant/forwarded/trace) and application headers
			// pass through. Mirrors messaging.StripInternalOnlyHeaders.
			if messaging.IsInternalOnlyHeader(k) {
				continue
			}
			s, ok := v.(string)
			if !ok {
				// A bridge-to-bridge / application header with a non-string
				// value cannot be serialised as an MQTT user property.
				// Count the drop (finding MQTT-N1) so a lost idempotency-key
				// or tenant-id is observable rather than silent.
				droppedNonString++
				continue
			}
			props.User = append(props.User, pahov5.UserProperty{Key: k, Value: s})
			hasProps = true
		}
	}

	if droppedNonString > 0 && len(metrics) > 0 && metrics[0] != nil {
		metrics[0].Counter(MetricMQTTNonStringHeaderDropped, int64(droppedNonString))
	}

	if hasProps {
		pub.Properties = props
	}

	return pub
}

// deriveEnvelopeID deterministically derives an Envelope.ID from the
// publish topic and payload (SHA-256, 128-bit hex) when no header
// provides one. Determinism is the point: a QoS 1 redelivery of the
// same publish from a NON-bridge producer (no mqtt.message-id user
// property, no correlation data) yields the SAME ID, so downstream
// idempotency/dedup can detect exactly the duplicates that broker
// redelivery creates. The MQTT packet identifier is deliberately NOT
// included — packet IDs are a 16-bit per-connection resource that the
// broker reuses, so mixing one in would make redeliveries look distinct
// (DUP redeliveries keep the ID, but re-sends after session resume may
// not) while adding no entropy across connections.
//
// Distinct application messages that share topic AND payload bytes
// collapse to the same ID by design — without a producer-supplied
// message ID they are indistinguishable on the wire anyway.
func deriveEnvelopeID(topic string, payload []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(topic))
	_, _ = h.Write([]byte{0}) // domain separator: topic vs payload bytes
	_, _ = h.Write(payload)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}
