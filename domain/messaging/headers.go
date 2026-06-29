package messaging

import "strings"

// HeaderPrefix is the reserved prefix for all bridge-internal headers.
// Transport adapters must strip headers with this prefix from external
// sources at ingress to prevent header injection.
const HeaderPrefix = "x-bridge."

// Well-known header constants.
//
// Every reserved Header* constant added here MUST be classified into
// exactly one of IsInternalOnlyHeader / IsBridgeToBridgeHeader below.
// TestHeaderClassificationExhaustive parses this block and fails if a
// constant is left unclassified — update the predicate, not a test list.
const (
	HeaderCorrelationID   = "x-bridge.correlation-id"
	HeaderCausationID     = "x-bridge.causation-id"
	HeaderIdempotencyKey  = "x-bridge.idempotency-key"
	HeaderContentType     = "x-bridge.content-type"
	HeaderSourceID        = "x-bridge.source-id"
	HeaderRouteID         = "x-bridge.route-id"
	HeaderOrderingKey     = "x-bridge.ordering-key"
	HeaderDeduplicationID = "x-bridge.dedup-id"
	HeaderTraceParent     = "traceparent"
	HeaderTraceState      = "tracestate"
	HeaderTenantID        = "x-bridge.tenant-id"
	HeaderRouteOverride   = "x-bridge.route-override"
	HeaderForwardedFrom   = "x-bridge.forwarded-from"
	HeaderForwardedHop    = "x-bridge.forwarded-hop"
)

// IsReservedHeader returns true if the key uses the reserved x-bridge. prefix.
// The check is case-insensitive to prevent bypass via mixed-case keys.
// Uses EqualFold on the prefix slice to avoid allocating a lowered copy.
func IsReservedHeader(key string) bool {
	return len(key) >= len(HeaderPrefix) &&
		strings.EqualFold(key[:len(HeaderPrefix)], HeaderPrefix)
}

// Reserved-header classification.
//
// Every reserved header falls into exactly one of two categories that
// govern whether it may be observed OUTSIDE the bridge. The predicates
// below report a header's category (case-insensitive, consistent with
// IsReservedHeader); the full table lives in the package doc.
//
//   - INTERNAL-ONLY (IsInternalOnlyHeader): stamped by the bridge for
//     its own dispatch bookkeeping. MUST NOT be exposed to a non-bridge
//     consumer — route-id, route-override, source-id, content-type.
//   - BRIDGE-TO-BRIDGE PROPAGATED (IsBridgeToBridgeHeader): part of the
//     cross-hop chaining / idempotency / tracing contract; MAY cross a
//     bridge-to-bridge boundary so a receiving bridge can correlate,
//     deduplicate and continue a trace.
//
// The two reserved categories are exhaustive and disjoint over the
// well-known header constants. A non-reserved (application) key belongs
// to neither: both predicates return false.

// IsInternalOnlyHeader reports whether key is a reserved header the
// bridge stamps for internal dispatch bookkeeping and that MUST NOT be
// exposed to a non-bridge consumer. Case-insensitive.
func IsInternalOnlyHeader(key string) bool {
	return equalsAnyFold(key,
		HeaderRouteID, HeaderRouteOverride, HeaderSourceID, HeaderContentType)
}

// IsBridgeToBridgeHeader reports whether key is a reserved header that
// may legitimately cross a bridge-to-bridge boundary as part of the
// chaining / idempotency / tracing contract. Case-insensitive.
func IsBridgeToBridgeHeader(key string) bool {
	return equalsAnyFold(key,
		HeaderCorrelationID, HeaderCausationID, HeaderIdempotencyKey,
		HeaderDeduplicationID, HeaderOrderingKey, HeaderTenantID,
		HeaderForwardedFrom, HeaderForwardedHop,
		HeaderTraceParent, HeaderTraceState)
}

// equalsAnyFold reports whether key case-insensitively equals any
// candidate. The candidates are a compile-time literal that does not
// escape equalsAnyFold, so the variadic call stays allocation-free.
func equalsAnyFold(key string, candidates ...string) bool {
	for _, c := range candidates {
		if strings.EqualFold(key, c) {
			return true
		}
	}
	return false
}

// StripReservedHeaders returns a new map with all reserved-prefix headers removed.
// Returns nil if the input is nil.
func StripReservedHeaders(headers map[string]any) map[string]any {
	if headers == nil {
		return nil
	}
	out := make(map[string]any, len(headers))
	for k, v := range headers {
		if !IsReservedHeader(k) {
			out[k] = v
		}
	}
	return out
}

// StripInternalOnlyHeaders returns a new map with every INTERNAL-ONLY
// reserved header (IsInternalOnlyHeader) removed, leaving BRIDGE-TO-
// BRIDGE PROPAGATED and application headers intact. It is the egress
// counterpart an operator ACL applies before handing an envelope's
// headers to a NON-bridge consumer, since a transport sender cannot
// otherwise tell a peer bridge from an external subscriber. The runtime
// does not call this today — see the package doc for why no
// destination-type egress hook exists. Returns nil if headers is nil.
func StripInternalOnlyHeaders(headers map[string]any) map[string]any {
	if headers == nil {
		return nil
	}
	out := make(map[string]any, len(headers))
	for k, v := range headers {
		if !IsInternalOnlyHeader(k) {
			out[k] = v
		}
	}
	return out
}

// MergeHeaders merges overlay into a copy of base. Overlay values take
// precedence on key collision. When protectReserved is true, reserved-prefix
// keys already present in base cannot be overridden by overlay — the check
// is case-insensitive so that "X-Bridge.Route-Id" is treated as already
// present when "x-bridge.route-id" exists in base.
func MergeHeaders(base, overlay map[string]any, protectReserved bool) map[string]any {
	size := len(base) + len(overlay)
	if size == 0 {
		return nil
	}
	out := make(map[string]any, size)
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		if protectReserved && IsReservedHeader(k) {
			if hasKeyCaseInsensitive(out, k) {
				continue
			}
		}
		out[k] = v
	}
	return out
}

// hasKeyCaseInsensitive returns true if m contains a key that matches
// target in a case-insensitive comparison. Used to prevent mixed-case
// bypass of reserved header protection. Uses EqualFold to avoid
// allocating lowered copies.
func hasKeyCaseInsensitive(m map[string]any, target string) bool {
	if _, ok := m[target]; ok {
		return true
	}
	for k := range m {
		if strings.EqualFold(k, target) {
			return true
		}
	}
	return false
}

// GetHeaderString extracts a string value from a headers map.
func GetHeaderString(headers map[string]any, key string) (string, bool) {
	if headers == nil {
		return "", false
	}
	v, ok := headers[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// SetHeader sets a header value, initialising the map if nil.
// It returns the (possibly newly created) map.
func SetHeader(headers map[string]any, key string, value any) map[string]any {
	if headers == nil {
		headers = make(map[string]any, 1)
	}
	headers[key] = value
	return headers
}

// Headers is the typed value object wrapping the raw header map.
//
// Design notes:
//
//   - Headers is defined as a NAMED MAP TYPE (not a struct) so that the
//     hot dispatch path stays allocation-free: a Headers value is
//     directly assignable to a map[string]any parameter (and vice versa)
//     under Go's "at least one of source/target is unnamed" rule. That
//     means existing helper signatures that take map[string]any
//     (RenderAddress, ExtractTraceContext, MergeHeaders, transport
//     ACLs that hand the map to broker SDKs, …) continue to compile
//     without any per-call conversion. New code is expected to use the
//     typed accessors (Get, GetString, Has, Range, Merge, …) instead
//     of direct indexing — those are the supported, nil-safe surface.
//
//   - Headers is the VALUE TYPE (not *Headers). The underlying map is
//     already a reference type, so copying a Headers value just copies
//     the map descriptor; mutating methods (Set, Delete) modify entries
//     in place. Receivers are therefore value receivers and are nil-
//     safe for read-only methods (Get, GetString, Has, Len, IsEmpty,
//     Range, Snapshot, AsMap). Set panics on a nil receiver — callers
//     who may hold a nil Headers should go through Envelope.SetHeader,
//     which lazily allocates.
//
//   - The reserved-prefix protection (x-bridge.*) lives at the
//     constructor boundary: NewHeadersFromMap strips reserved keys
//     defensively, mirroring the historic StripReservedHeaders contract
//     used by Envelope ingress. headersFromTrustedMap (unexported) is
//     the runtime escape hatch for stamping paths (RouteRunner,
//     OutboxDrainer) that legitimately install bridge metadata.
type Headers map[string]any

// NewHeaders returns an empty, non-nil Headers ready for mutation.
func NewHeaders() Headers { return make(Headers) }

// NewHeadersFromMap constructs Headers from a caller-supplied map,
// applying StripReservedHeaders semantics so that ingress / untrusted
// callers cannot spoof bridge metadata. Returns nil if m is nil
// (preserving the historical behaviour of StripReservedHeaders).
func NewHeadersFromMap(m map[string]any) Headers {
	if m == nil {
		return nil
	}
	out := make(Headers, len(m))
	for k, v := range m {
		if !IsReservedHeader(k) {
			out[k] = v
		}
	}
	return out
}

// headersFromTrustedMap wraps an existing map without stripping
// reserved-prefix entries. It is package-internal because only the
// runtime stamping pipeline (and JSON rehydration) is allowed to skip
// the strip. Callers MUST treat the input map as ownership-transferred
// — no defensive copy is taken.
func headersFromTrustedMap(m map[string]any) Headers { return Headers(m) }

// Get returns the value associated with key and a presence flag.
// Nil-safe.
func (h Headers) Get(key string) (any, bool) {
	if h == nil {
		return nil, false
	}
	v, ok := h[key]
	return v, ok
}

// GetString returns a string-typed value or ("", false) if the key is
// missing or the value is not of type string. Nil-safe.
func (h Headers) GetString(key string) (string, bool) {
	if h == nil {
		return "", false
	}
	v, ok := h[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// Has reports whether key is present. Nil-safe.
func (h Headers) Has(key string) bool {
	if h == nil {
		return false
	}
	_, ok := h[key]
	return ok
}

// Len returns the number of headers. Nil-safe (returns 0).
func (h Headers) Len() int { return len(h) }

// IsEmpty reports whether the headers are nil or empty.
func (h Headers) IsEmpty() bool { return len(h) == 0 }

// Set assigns key=value. Panics if h is nil — callers should hold a
// constructed Headers (via NewHeaders / NewHeadersFromMap) or go
// through Envelope.SetHeader (which lazily initialises).
func (h Headers) Set(key string, value any) {
	if h == nil {
		panic("messaging: Headers.Set on nil receiver; use NewHeaders or Envelope.SetHeader")
	}
	h[key] = value
}

// Delete removes key. Nil-safe.
func (h Headers) Delete(key string) {
	if h == nil {
		return
	}
	delete(h, key)
}

// Range invokes f for each header. Iteration order is unspecified
// (map order). Returning false from f stops iteration early. Nil-safe.
func (h Headers) Range(f func(key string, value any) bool) {
	for k, v := range h {
		if !f(k, v) {
			return
		}
	}
}

// Merge produces a NEW Headers value by overlaying overlay on top of h.
// When protectReserved is true, reserved-prefix keys already present in
// h (case-insensitive) cannot be overridden by overlay — mirrors the
// MergeHeaders package function. Returns nil if both inputs are empty.
func (h Headers) Merge(overlay Headers, protectReserved bool) Headers {
	merged := MergeHeaders(map[string]any(h), map[string]any(overlay), protectReserved)
	if merged == nil {
		return nil
	}
	return Headers(merged)
}

// Snapshot returns a deep copy of the headers as a plain
// map[string]any, suitable for handing to untrusted boundaries
// (audit log, DLQ store, transport serialisation) without exposing
// the live map. Returns nil when the receiver is nil.
func (h Headers) Snapshot() map[string]any {
	if h == nil {
		return nil
	}
	return deepCopyHeaders(h)
}

// AsMap returns the underlying map[string]any. This is a TRANSPORT
// ESCAPE HATCH for legacy helper signatures and broker SDKs that
// require the unnamed map type explicitly when type inference does not
// suffice (typed-nil generics, reflection-based serialisers). Callers
// MUST treat the returned reference as read-only; use Snapshot when
// isolation is required.
func (h Headers) AsMap() map[string]any { return map[string]any(h) }

// CorrelationID returns the bridge correlation ID header (if present
// and a string). Convenience accessor for the runtime hot path.
func (h Headers) CorrelationID() (string, bool) { return h.GetString(HeaderCorrelationID) }

// RouteID returns the bridge route ID header (if present and a string).
func (h Headers) RouteID() (string, bool) { return h.GetString(HeaderRouteID) }

// RouteOverride returns the route-override header used by processors
// to redirect dispatch (if present and a string).
func (h Headers) RouteOverride() (string, bool) { return h.GetString(HeaderRouteOverride) }
