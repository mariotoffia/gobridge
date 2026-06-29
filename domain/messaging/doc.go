// Package messaging defines the messaging-context domain types of the
// GoBridge bridge: the canonical Envelope value object, the reserved
// header vocabulary, and the W3C Trace Context parse/format/extract/
// inject helpers.
//
// This package is part of the innermost (Layer 1) ring of the
// architecture. It carries no project dependencies and no external
// dependencies — imports must remain stdlib-only.
//
// # Reserved-header classification
//
// Every reserved header is classified for EGRESS visibility. Ingress is
// unconditional: an untrusted producer's x-bridge.* keys are always
// stripped (StripReservedHeaders / NewHeadersFromMap). The
// classification instead governs which reserved headers may be visible
// OUTSIDE the bridge once the bridge itself has stamped them.
//
//	Header constant         Wire key                  Class
//	----------------------  ------------------------  --------------------------
//	HeaderRouteID           x-bridge.route-id         INTERNAL-ONLY
//	HeaderRouteOverride     x-bridge.route-override   INTERNAL-ONLY
//	HeaderSourceID          x-bridge.source-id        INTERNAL-ONLY
//	HeaderContentType       x-bridge.content-type     INTERNAL-ONLY
//	HeaderCorrelationID     x-bridge.correlation-id   BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderCausationID       x-bridge.causation-id     BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderIdempotencyKey    x-bridge.idempotency-key  BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderDeduplicationID   x-bridge.dedup-id         BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderOrderingKey       x-bridge.ordering-key     BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderTenantID          x-bridge.tenant-id        BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderForwardedFrom     x-bridge.forwarded-from   BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderForwardedHop      x-bridge.forwarded-hop    BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderTraceParent       traceparent               BRIDGE-TO-BRIDGE PROPAGATED
//	HeaderTraceState        tracestate                BRIDGE-TO-BRIDGE PROPAGATED
//
// INTERNAL-ONLY headers MUST NOT reach a non-bridge consumer; an egress
// ACL delivering to an external subscriber strips them with
// StripInternalOnlyHeaders. BRIDGE-TO-BRIDGE PROPAGATED headers MAY
// cross a bridge-to-bridge hop so the receiving bridge can correlate,
// deduplicate and continue a trace; that bridge still strips them at
// ingress (anti-spoof) and re-stamps the ones it owns.
//
// gobridge has no destination-type signal at a transport sender — it
// cannot tell a peer bridge from an external subscriber — so the
// runtime does NOT auto-strip INTERNAL-ONLY headers on egress today.
// The classification is the documented contract; the predicates
// (IsInternalOnlyHeader / IsBridgeToBridgeHeader) and the
// StripInternalOnlyHeaders helper make it enforceable at an operator
// ACL or a future destination-aware egress hook.
package messaging
