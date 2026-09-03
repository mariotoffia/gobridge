package messaging

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// Sentinel validation errors raised by NewEnvelope. They are plain
// stdlib errors because domain/messaging is stdlib-only by design
// (see DDD.md context map). Adapters and use cases that need a
// classified *shared.BridgeError MUST wrap these at their layer
// using shared.NewBridgeError(shared.ErrCodeInvalidPayload, ...).
var (
	ErrInvalidEnvelopeID    = errors.New("messaging: envelope ID must not be empty")
	ErrEnvelopeClockMissing = errors.New("messaging: clock is required when CreatedAt is zero")
	ErrExpiryBeforeCreated  = errors.New("messaging: expiry must not precede CreatedAt")
	ErrEnvelopeIDImmutable  = errors.New("messaging: envelope ID is already assigned")
)

// Envelope is the normalized message moving through the bridge.
//
// Subject and Headers are unexported to enforce controlled mutation:
// reserved-prefix headers (x-bridge.*) are stripped at construction so
// untrusted ingress cannot spoof bridge metadata. External callers MUST
// use NewEnvelope to construct an instance and the accessor methods
// (Subject / SetSubject / Headers / Header / SetHeader / DeleteHeader /
// ReplaceHeaders) to read or mutate state.
//
// Headers() returns the live underlying header set (typed
// messaging.Headers, a named map[string]any) by reference rather than
// a defensive deep copy. This is a deliberate trade-off: the runtime
// and adapter hot paths hand the map to helpers like GetHeaderString,
// ExtractTraceContext, and RenderAddress on every message, and a
// per-call deep copy would impose a heap allocation on the dispatch
// path. Callers MUST treat the returned Headers as read-only — use
// the mutator methods (or HeadersSnapshot for an isolated copy) when
// modification is required.
//
// aggregate-root
type Envelope struct {
	id        string
	subject   string
	payload   []byte
	headers   Headers
	createdAt time.Time
	expiresAt time.Time
}

// EnvelopeInput is the construction shape accepted by NewEnvelope. An
// input struct (rather than functional options) was chosen because the
// existing callers all build envelopes by listing the same core fields
// in field-literal form; a struct keeps that readability while routing
// through the validating constructor.
//
// IdempotencyKey, DeduplicationID and OrderingKey are the FIRST-CLASS,
// trusted path by which an external producer supplies the reserved
// idempotency / dedup / ordering keys WITHOUT being able to spoof the
// rest of the bridge namespace: NewEnvelope strips any x-bridge.* keys
// a caller smuggles in via Headers, then stamps these explicit fields
// into their reserved headers on the trusted side of that strip. All
// three are BRIDGE-TO-BRIDGE PROPAGATED (see the reserved-header
// classification in the package doc). Leave a field empty to stamp
// nothing.
type EnvelopeInput struct {
	ID              string
	Subject         string
	Payload         []byte
	Headers         map[string]any
	CreatedAt       time.Time
	ExpiresAt       time.Time
	IdempotencyKey  string
	DeduplicationID string
	OrderingKey     string
}

// clonePayload returns nil for empty input, else an independent copy so a
// caller's slice cannot alias aggregate state.
func clonePayload(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

// checkEnvelopeID enforces the single envelope-identity invariant shared
// by every construction and rehydration path: the ID must be non-empty.
// NewEnvelope, AssignID and UnmarshalJSON all route through this one
// function (returning the shared ErrInvalidEnvelopeID sentinel) so the
// emptiness rule lives in exactly one place rather than three divergent
// copies.
func checkEnvelopeID(id string) error {
	if id == "" {
		return ErrInvalidEnvelopeID
	}
	// Every durable record — DLQ entry, outbox row — is keyed by this ID and
	// written through MarshalJSON, and encoding/json replaces a byte sequence
	// that is not valid UTF-8 with U+FFFD. An ID that is not valid UTF-8 would
	// therefore come back from the store as a DIFFERENT identity than the one
	// that went in, so a redrive would inject a message the replay ledger
	// cannot match to the original. Refuse it here, where it is created,
	// rather than corrupt it where it is stored. Transports that carry binary
	// identity encode it first: the MQTT adapter base64-encodes binary
	// Correlation Data before using it as an identity.
	if !utf8.ValidString(id) {
		return fmt.Errorf("%w: identity is not valid UTF-8, so it cannot survive "+
			"the persisted form", ErrInvalidEnvelopeID)
	}
	return nil
}

// NewEnvelope constructs and validates an Envelope.
//
//   - ID must be non-empty and valid UTF-8 (ErrInvalidEnvelopeID).
//   - Headers are normalised via StripReservedHeaders so the canonical
//     bridge metadata namespace (x-bridge.*) cannot be supplied by the
//     caller; it is owned by the runtime and stamped later in the
//     pipeline.
//   - IdempotencyKey, DeduplicationID and OrderingKey, when non-empty,
//     are stamped into their reserved headers (HeaderIdempotencyKey,
//     HeaderDeduplicationID, HeaderOrderingKey) AFTER the strip above —
//     the controlled, anti-spoof-safe path for an external producer to
//     supply these keys. Caller-supplied x-bridge.* in Headers is still
//     stripped and can never reach them.
//   - CreatedAt, when zero, is replaced with the supplied now value;
//     a non-zero caller value is preserved (rehydration / replay paths).
//     A zero now combined with a zero CreatedAt returns
//     ErrEnvelopeClockMissing — the caller (typically an adapter holding
//     a clock.Clock) is responsible for sourcing the timestamp. We pass
//     a time.Time rather than a clock interface so domain/messaging
//     stays stdlib-only (no edge to domain/clock — see .go-arch-lint.yml).
func NewEnvelope(in EnvelopeInput, now time.Time) (*Envelope, error) {
	return newEnvelope(in, now, true)
}

// NewEnvelopeFromImmutablePayload constructs and validates an Envelope while
// sharing the input payload backing. It is reserved for trusted transport
// ingress whose SDK guarantees that payload bytes remain immutable for the
// Envelope's lifetime. General callers must use NewEnvelope, which defensively
// copies its input. Payload() still returns a copy and SetPayload remains
// copy-on-write, so this operation does not expose mutable aggregate state.
func NewEnvelopeFromImmutablePayload(in EnvelopeInput, now time.Time) (*Envelope, error) {
	return newEnvelope(in, now, false)
}

func newEnvelope(in EnvelopeInput, now time.Time, copyPayload bool) (*Envelope, error) {
	if err := checkEnvelopeID(in.ID); err != nil {
		return nil, err
	}
	created := in.CreatedAt
	if created.IsZero() {
		if now.IsZero() {
			return nil, ErrEnvelopeClockMissing
		}
		created = now
	}
	payload := in.Payload
	if copyPayload {
		payload = clonePayload(payload)
	}
	e := &Envelope{
		id:        in.ID,
		subject:   in.Subject,
		payload:   payload,
		headers:   NewHeadersFromMap(in.Headers),
		createdAt: created,
		expiresAt: in.ExpiresAt,
	}
	// First-class external idempotency / dedup / ordering keys are
	// stamped AFTER NewHeadersFromMap has stripped any caller-supplied
	// x-bridge.* keys, so an untrusted Headers map can never spoof bridge
	// metadata yet an external producer still has an explicit, controlled
	// path into the reserved namespace. SetHeader is the trusted per-key
	// setter (no strip) and lazily initialises the header map.
	if in.IdempotencyKey != "" {
		e.SetHeader(HeaderIdempotencyKey, in.IdempotencyKey)
	}
	if in.DeduplicationID != "" {
		e.SetHeader(HeaderDeduplicationID, in.DeduplicationID)
	}
	if in.OrderingKey != "" {
		e.SetHeader(HeaderOrderingKey, in.OrderingKey)
	}
	return e, nil
}

// ID returns the immutable envelope identity.
func (e *Envelope) ID() string { return e.id }

// CreatedAt returns the creation timestamp.
func (e *Envelope) CreatedAt() time.Time { return e.createdAt }

// ExpiresAt returns the absolute expiry (zero = no expiry).
func (e *Envelope) ExpiresAt() time.Time { return e.expiresAt }

// Payload returns a defensive copy of the message body — the byte slice is
// reference-typed and the aggregate must not hand out an aliasable view of
// its own state. Returns nil for an empty payload.
func (e *Envelope) Payload() []byte {
	if len(e.payload) == 0 {
		return nil
	}
	cp := make([]byte, len(e.payload))
	copy(cp, e.payload)
	return cp
}

// Subject returns the logical message subject.
func (e *Envelope) Subject() string { return e.subject }

// SetSubject replaces the logical subject. Subject mutation is allowed
// because it is the documented processor extension point (see
// processor_chain — processors may rewrite the routing subject).
//
//aggcheck:allow-unguarded
func (e *Envelope) SetSubject(s string) { e.subject = s }

// SetExpiry stamps an absolute expiry from an adapter ingress path (broker
// TTL / message-expiry properties) onto an already-constructed envelope.
// Returns ErrExpiryBeforeCreated when t precedes CreatedAt so a malformed
// broker timestamp cannot produce an envelope that is born expired. A zero t
// clears the expiry.
func (e *Envelope) SetExpiry(t time.Time) error {
	if !t.IsZero() && t.Before(e.createdAt) {
		return ErrExpiryBeforeCreated
	}
	e.expiresAt = t
	return nil
}

// AssignID assigns the envelope identity when it has none. Identity is
// immutable once set: AssignID returns ErrEnvelopeIDImmutable if the envelope
// already carries an ID, and ErrInvalidEnvelopeID if id is empty.
func (e *Envelope) AssignID(id string) error {
	if err := checkEnvelopeID(id); err != nil {
		return err
	}
	if e.id != "" {
		return ErrEnvelopeIDImmutable
	}
	e.id = id
	return nil
}

// SetPayload replaces the message body. This is the documented processor
// extension point (transform processors rewrite the payload). The input is
// defensively copied so the caller's slice cannot alias aggregate state.
//
//aggcheck:allow-unguarded
func (e *Envelope) SetPayload(b []byte) { e.payload = clonePayload(b) }

// Headers returns the live typed Headers value. Callers MUST treat it
// as read-only; use SetHeader / DeleteHeader / ReplaceHeaders to
// mutate or HeadersSnapshot for an isolated copy. The returned value
// may be nil; the typed accessors (Get, Has, Range, Len, GetString,
// CorrelationID, …) are nil-safe by design.
//
// Headers is a named map type, so the value remains directly assignable
// to legacy map[string]any helper signatures (RenderAddress,
// ExtractTraceContext, MergeHeaders, …) without explicit conversion.
func (e *Envelope) Headers() Headers { return e.headers }

// Header returns a single header value and whether it was present.
func (e *Envelope) Header(key string) (any, bool) { return e.headers.Get(key) }

// SetHeader assigns key=value, initialising the underlying Headers
// lazily when nil.
//
//aggcheck:allow-unguarded
func (e *Envelope) SetHeader(key string, value any) {
	if e.headers == nil {
		e.headers = NewHeaders()
	}
	e.headers[key] = value
}

// DeleteHeader removes key from the underlying Headers. No-op when nil.
//
//aggcheck:allow-unguarded
func (e *Envelope) DeleteHeader(key string) { e.headers.Delete(key) }

// ReplaceHeaders swaps the entire header map. Reserved-prefix headers
// (x-bridge.*) supplied by the caller are stripped defensively because
// the most common producer of a wholesale-replacement map is an
// adapter ingress path translating untrusted broker properties — and
// even when the caller is trusted, going through this method is the
// project's last line of defence against accidentally ingesting
// spoofed bridge metadata. Trusted runtime stamping that legitimately
// needs to write the reserved namespace MUST use SetHeader (per-key)
// or StampHeaders (whole-map, no strip).
//
//aggcheck:allow-unguarded
func (e *Envelope) ReplaceHeaders(h map[string]any) {
	e.headers = NewHeadersFromMap(h)
}

// StampHeaders is the trusted whole-map setter used by the runtime
// dispatch path (RouteRunner / OutboxDrainer) to install a fully
// merged header set that may contain bridge-reserved keys (correlation
// ID, route ID, …) which were stamped by the bridge itself. Because
// the input is constructed from internal sources, no reserved-prefix
// strip is applied. External / adapter-ingress code MUST go through
// ReplaceHeaders.
//
//aggcheck:allow-unguarded
func (e *Envelope) StampHeaders(h map[string]any) { e.headers = headersFromTrustedMap(h) }

// HeadersSnapshot returns a deep copy of the headers map. Use when the
// caller needs to mutate or hand the map to an untrusted boundary
// without affecting the envelope state.
func (e *Envelope) HeadersSnapshot() map[string]any { return e.headers.Snapshot() }

// HasExpiry returns true if the envelope has a non-zero expiry timestamp.
func (e *Envelope) HasExpiry() bool {
	return !e.expiresAt.IsZero()
}

// IsExpired returns true if the envelope has expired according to clk.
func (e *Envelope) IsExpired(clk interface{ Now() time.Time }) bool {
	return e.HasExpiry() && clk.Now().After(e.expiresAt)
}

// RemainingTTL returns the time remaining before expiry according to clk.
// Returns 0 if the envelope has no expiry or is already expired.
func (e *Envelope) RemainingTTL(clk interface{ Now() time.Time }) time.Duration {
	if !e.HasExpiry() {
		return 0
	}
	now := clk.Now()
	if !e.expiresAt.After(now) {
		return 0
	}
	return e.expiresAt.Sub(now)
}

// Clone returns an isolated envelope copy. Headers are recursively copied.
// Payload backing is shared because Envelope never exposes it mutably:
// Payload returns a copy and SetPayload installs a new owned backing instead of
// modifying the existing bytes. This copy-on-write contract keeps clones
// mutation-safe without multiplying large immutable message bodies.
func (e *Envelope) Clone() *Envelope {
	c := *e
	if e.headers != nil {
		c.headers = Headers(deepCopyHeaders(e.headers))
	}
	return &c
}

// SharePayloadFrom replaces the receiver's payload with src's immutable
// backing. It never exposes the bytes and remains copy-on-write safe because
// SetPayload replaces rather than mutates backing storage.
//
//aggcheck:allow-unguarded
func (e *Envelope) SharePayloadFrom(src *Envelope) {
	if src == nil {
		e.payload = nil
		return
	}
	e.payload = src.payload
}
