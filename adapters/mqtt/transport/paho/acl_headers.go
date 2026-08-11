package paho

import (
	"encoding/base64"
	"time"
	"unicode"
	"unicode/utf8"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

const headerMQTTResponseTopic = "mqtt.response-topic"

// headerMQTTGeneratedID is an ADAPTER-INTERNAL user-property key that
// publishWithIdentity stamps alongside a generated mqtt.message-id to record
// that the resolved identity is adapter-minted (the producer supplied no stable
// mqtt.message-id / correlation data). It never crosses the wire: it is added
// only to the in-memory callback copy before fan-out, and EnvelopeFromPublish
// consumes it into messaging.HeaderGeneratedID rather than emitting it as an
// application header.
//
// Provenance is ingress-owned, never publisher-supplied. publishWithIdentity
// removes every inbound occurrence of this key before deciding whether to mint,
// so the marker on a dispatched publish is always the adapter's own. Without
// that strip a publisher could pair a stable mqtt.message-id with this marker
// and have its own countable identity classified as adapter-minted — the
// runtime then treats each redelivery as uncountable and terminalizes the first
// transient failure (DLQ or drop) instead of retrying a healthy message.
const headerMQTTGeneratedID = "mqtt.generated-id"

// HeaderMessageID is the user-property key used to round-trip the
// domain Envelope.ID through MQTT. On receive, this header takes
// precedence for setting Envelope.ID; the correlation-id header is
// the second choice, and a per-publish UUID is the fallback.
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
// printable-ASCII-only filter dropped silently. Control characters
// (including NUL, newline, and controls) are still rejected to prevent
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
//  2. CorrelationData — the textual value when it is a safe header string,
//     otherwise a stable encoded form of the raw bytes (mqtt-bin:<base64>), so
//     a producer that identifies its message with binary Correlation Data keeps
//     one identity across broker redelivery
//  3. A random UUIDv4 generated for this received publish
//
// Cases 1 and 2 are PRODUCER-OWNED: whoever may publish to the subscribed
// topics owns that source's envelope-ID namespace. Two publishes reusing one ID
// are one identity to the outbox, so the second is suppressed as a duplicate
// (counted on the runtime's duplicate-suppression metric). The namespace is
// scoped to the source: distinct sources reach distinct routes and bindings, so
// an ID is only ever compared with IDs from the same source.
//
// The *pahov5.Publish parameter is the SDK boundary input this ACL
// helper exists to translate; its only callers are ACL files
// (acl_router.go), so the SDK type does not cross into port-side code.
//
// An optional MetricsExporter (variadic, mirroring PublishFromEnvelope)
// counts application/bridge user properties dropped by the safety filter
// (unsafe key/value or over-length) on MetricMQTTIngressHeaderDropped, so
// the otherwise-silent ingress drop is observable. Reserved and
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
	var subject string
	var expiresAt time.Time
	// generatedIdentity records that the resolved Envelope.ID is adapter-minted
	// (no producer mqtt.message-id / correlation data). It is set either by the
	// headerMQTTGeneratedID marker publishWithIdentity stamps before fan-out, or
	// by the direct-caller fallback below. It flows to the runtime as
	// messaging.HeaderGeneratedID so the replay cap can terminate an uncountable
	// redelivery loop.
	generatedIdentity := false
	// droppedHeaders counts every inbound header that fails the length/safety
	// filter — both MQTT v5 properties (correlation data, content type, response
	// topic) and arbitrary user properties. It feeds MetricMQTTIngressHeaderDropped
	// so a correlation-id loss is observable rather than silent.
	droppedHeaders := 0

	if pub.Properties != nil {
		switch raw := pub.Properties.CorrelationData; classifyCorrelationData(raw) {
		case correlationText:
			headers[messaging.HeaderCorrelationID] = string(raw)
		case correlationBinary:
			// Legal binary Correlation Data. Retain the exact bytes encoded so the
			// identity derived from them is stable across redelivery and the egress
			// hop can reproduce them byte for byte. The key is reserved, so an
			// inbound user property of that name was already dropped by the
			// reserved-prefix filter below and cannot displace these bytes.
			headers[messaging.HeaderCorrelationData] = encodeCorrelationData(raw)
		case correlationUnusable:
			droppedHeaders++
		case correlationAbsent:
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
			if u.Key == headerMQTTGeneratedID {
				// Adapter-internal marker stamped by publishWithIdentity alongside a
				// generated mqtt.message-id. Consume it (never emit it as an
				// application header) and record that the identity is adapter-minted.
				generatedIdentity = true
				continue
			}
			if u.Key == HeaderMQTTTopic || u.Key == HeaderMQTTRetained || u.Key == HeaderMQTTQoS {
				// Adapter-controlled: never let an inbound user property
				// override the recorded transport-level topic, retained
				// flag, or delivery QoS. (The retained correlation bytes are
				// protected by the reserved-prefix filter below instead.)
				continue
			}
			if u.Key == HeaderMessageID {
				if len(u.Value) <= maxHeaderValueLen && isSafeHeaderValue(u.Value) {
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
	// The router stamps a generated mqtt.message-id once before fan-out. Direct
	// ACL callers still receive a unique fallback without mutating their SDK
	// input.
	id := publishIdentity(pub)
	if id == "" {
		id = newIngressEnvelopeID()
		generatedIdentity = true
	}

	// Record an adapter-minted identity so the runtime replay cap can terminate a
	// count-less redelivery loop. StampHeaders is the trusted setter,
	// so this reserved internal-only key is preserved; it is stripped again at
	// egress by PublishFromEnvelope's IsInternalOnlyHeader filter.
	if generatedIdentity {
		headers[messaging.HeaderGeneratedID] = "true"
	}

	// id is always non-empty (generate fallback above); now is non-zero.
	// Paho's acknowledgement tracker retains the immutable wire-packet backing
	// until settlement, so the Envelope can share payload without a route copy.
	// Construction cannot fail here; the panic guards an impossible branch.
	env, err := messaging.NewEnvelopeFromImmutablePayload(messaging.EnvelopeInput{
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
// path since the Sender was routed through the seam.
//
// An optional MetricsExporter (variadic, mirroring NewSession) is used to
// count bridge-to-bridge / application headers dropped because their value
// is not a string: such a value cannot become an MQTT user
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
		// Correlation Data is binary on the wire. The retained bytes win when
		// present: they are the producer's actual Correlation Data for this
		// message, whereas x-bridge.correlation-id is the bridge's own logical
		// correlation value and is SYNTHESIZED for every envelope that arrives
		// without one (RouteRunner.injectHeaders). Preferring the string would
		// therefore replace a binary producer's identity bytes with a random
		// bridge id on every hop. A retention header that no longer decodes cannot
		// become wire bytes — it is counted as a dropped header rather than
		// silently vanishing, and the textual value still serves.
		binaryCorrelation, hasBinary := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationData)
		if hasBinary {
			raw, err := base64.RawURLEncoding.DecodeString(binaryCorrelation)
			switch {
			case err != nil || len(raw) == 0:
				// Never written this way by ingress. Do not let an undecodable or
				// empty value suppress the textual correlation id below.
				droppedNonString++
				hasBinary = false
			default:
				props.CorrelationData = raw
				hasProps = true
			}
		}
		if v, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID); ok && !hasBinary {
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
				// Count the drop so a lost idempotency-key
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
