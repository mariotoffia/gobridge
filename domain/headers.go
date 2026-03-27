package domain

import "strings"

// HeaderPrefix is the reserved prefix for all bridge-internal headers.
// Transport adapters must strip headers with this prefix from external
// sources at ingress to prevent header injection.
const HeaderPrefix = "x-bridge."

// Well-known header constants.
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
	HeaderForwardedFrom  = "x-bridge.forwarded-from"
	HeaderForwardedHop   = "x-bridge.forwarded-hop"
)

// IsReservedHeader returns true if the key uses the reserved x-bridge. prefix.
// The check is case-insensitive to prevent bypass via mixed-case keys.
// Uses EqualFold on the prefix slice to avoid allocating a lowered copy.
func IsReservedHeader(key string) bool {
	return len(key) >= len(HeaderPrefix) &&
		strings.EqualFold(key[:len(HeaderPrefix)], HeaderPrefix)
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
