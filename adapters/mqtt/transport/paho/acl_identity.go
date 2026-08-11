package paho

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"

	pahov5 "github.com/eclipse/paho.golang/paho"
)

// correlationDataPrefix namespaces an envelope identity derived from BINARY
// MQTT v5 Correlation Data, keeping it distinct from a producer's own textual
// mqtt.message-id in the same source's identity space.
const correlationDataPrefix = "mqtt-bin:"

// correlationClass is the single ingress classification of MQTT v5 Correlation
// Data. Header extraction and identity resolution both switch on it so the
// retained header and the derived envelope ID can never disagree about which
// bytes were usable.
type correlationClass int

const (
	// correlationAbsent means the publish carries no Correlation Data.
	correlationAbsent correlationClass = iota
	// correlationText means the bytes are a safe header string and round-trip
	// as messaging.HeaderCorrelationID.
	correlationText
	// correlationBinary means the bytes are legal MQTT Correlation Data that
	// cannot round-trip as a header string (NUL, control characters or
	// non-UTF-8). They are retained encoded under HeaderMQTTCorrelationData and
	// yield a stable, redelivery-invariant identity.
	correlationBinary
	// correlationUnusable means the bytes exceed the header bound, so they are
	// neither retained nor usable as identity. The publish falls back to an
	// adapter-minted identity and the loss is counted.
	correlationUnusable
)

// classifyCorrelationData decides how inbound Correlation Data is handled.
func classifyCorrelationData(raw []byte) correlationClass {
	switch {
	case len(raw) == 0:
		return correlationAbsent
	case len(raw) > maxHeaderValueLen:
		return correlationUnusable
	case isSafeHeaderValue(string(raw)):
		return correlationText
	default:
		return correlationBinary
	}
}

// encodeCorrelationData renders binary Correlation Data as unpadded URL-safe
// base64: a safe header string that survives JSON persistence and decodes back
// to the exact wire bytes.
func encodeCorrelationData(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

// publishIdentity returns the producer identity carried by pub, preserving
// the ingress precedence used by EnvelopeFromPublish. Invalid or unsafe values
// are ignored exactly as they are during header extraction.
//
// Binary Correlation Data is a stable producer identity, not a missing one: the
// broker redelivers the same bytes, so the encoded form is redelivery-invariant
// and the message stays countable by the runtime's replay ledger.
func publishIdentity(pub *pahov5.Publish) string {
	if pub == nil || pub.Properties == nil {
		return ""
	}
	var messageID string
	for _, property := range pub.Properties.User {
		if property.Key == HeaderMessageID && len(property.Value) <= maxHeaderValueLen && isSafeHeaderValue(property.Value) {
			messageID = property.Value
		}
	}
	if messageID != "" {
		return messageID
	}
	raw := pub.Properties.CorrelationData
	switch classifyCorrelationData(raw) {
	case correlationText:
		return string(raw)
	case correlationBinary:
		return correlationDataPrefix + encodeCorrelationData(raw)
	case correlationAbsent, correlationUnusable:
	}
	return ""
}

// publishWithIdentity returns the publish the router dispatches: ingress-owned
// provenance, plus one generated identity when the producer supplied none.
//
// Two things happen here, and both are ingress's job alone:
//
//   - Every inbound headerMQTTGeneratedID user property is removed. That key is
//     adapter-reserved; honouring a publisher's copy would let it label its own
//     stable identity as adapter-minted, which makes the runtime treat each
//     redelivery as uncountable and terminalize the first transient failure.
//   - When no producer identity remains, one generated mqtt.message-id is
//     stamped once — before fan-out, so every handler derives the same envelope
//     ID — together with the adapter's own marker.
//
// pub is Paho's callback packet and stays live in the SDK's acknowledgement
// tracker until settlement, so changes are made on a shallow copy with a copied
// UserProperty slice; payload and string backing remain shared.
//
// Apply EXACTLY ONCE per inbound packet, at the ingress entry point
// (onPublishReceived and the legacy Route path). The marker is indistinguishable
// from a publisher's copy — that is the whole reason it is stripped — so a
// second application would strip the adapter's own marker and leave a minted,
// per-redelivery identity looking stable. Nothing downstream re-enters here:
// buffering, fan-out and the pending flush all carry the publish this returns.
func publishWithIdentity(pub *pahov5.Publish) *pahov5.Publish {
	if pub == nil {
		return pub
	}
	identity := publishIdentity(pub)
	if identity != "" && !carriesGeneratedMarker(pub) {
		return pub
	}

	owned := *pub
	var properties pahov5.PublishProperties
	if pub.Properties != nil {
		properties = *pub.Properties
		properties.User = make(pahov5.UserProperties, 0, len(pub.Properties.User)+2)
		for _, property := range pub.Properties.User {
			if property.Key == headerMQTTGeneratedID {
				continue
			}
			properties.User = append(properties.User, property)
		}
	}
	if identity == "" {
		properties.User = append(properties.User,
			pahov5.UserProperty{Key: HeaderMessageID, Value: newIngressEnvelopeID()},
			// Marker so EnvelopeFromPublish records the identity as adapter-minted.
			// Consumed there; never emitted as a header or sent to a peer.
			pahov5.UserProperty{Key: headerMQTTGeneratedID, Value: "1"},
		)
	}
	owned.Properties = &properties
	return &owned
}

// carriesGeneratedMarker reports whether pub carries the adapter-reserved
// provenance marker, which on an inbound publish can only be publisher-supplied.
func carriesGeneratedMarker(pub *pahov5.Publish) bool {
	if pub == nil || pub.Properties == nil {
		return false
	}
	for _, property := range pub.Properties.User {
		if property.Key == headerMQTTGeneratedID {
			return true
		}
	}
	return false
}

// newIngressEnvelopeID returns an RFC 4122 UUIDv4 using the standard-library
// cryptographic random source. Entropy failure is unrecoverable: continuing
// without a unique identity can silently collapse a distinct outbox event.
func newIngressEnvelopeID() string {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		panic("paho: crypto/rand unavailable generating MQTT ingress identity: " + err.Error())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80

	var id [36]byte
	hex.Encode(id[0:8], raw[0:4])
	id[8] = '-'
	hex.Encode(id[9:13], raw[4:6])
	id[13] = '-'
	hex.Encode(id[14:18], raw[6:8])
	id[18] = '-'
	hex.Encode(id[19:23], raw[8:10])
	id[23] = '-'
	hex.Encode(id[24:36], raw[10:16])
	return string(id[:])
}
