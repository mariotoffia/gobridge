// Package transport is the HTTP transport adapter for gobridge. It
// implements ports.TransportFactory and provides two roles:
//
//   - an HTTP POST receiver (ports.Receiver) that converts an inbound
//     JSON request into a delivery, and
//   - a Server-Sent-Events sender (ports.Sender) that streams envelopes
//     to connected SSE subscribers.
//
// # Security contracts
//
// These contracts were hardened during the production-readiness review;
// they are the behaviour adapter callers and operators may rely on.
//
// Cluster forward trust (loop prevention without a spoofable bypass).
// A request carrying X-Bridge-Forwarded: true is treated as an
// already-forwarded, peer-originated message — and therefore processed
// locally instead of being re-forwarded — ONLY when it also proves the
// shared internal forwarding token in X-Bridge-Forward-Token (constant-
// time compared). The token is configured symmetrically:
// Factory.WithForwardToken on the receiving side and
// ForwarderConfig.ForwardToken on the sending side. When no token is
// configured the receiver NEVER trusts the marker, so a client cannot
// spoof X-Bridge-Forwarded to force local processing on a non-owner
// node; an untrusted marker simply degrades to a normal forward to the
// true route owner. Wiring an identical token into every peer is a
// composition-root responsibility (see SHARED-DEFER note below).
//
// SSE egress header hygiene. Before an envelope is serialised to an SSE
// subscriber (which a sender cannot distinguish from an external client)
// the INTERNAL-ONLY reserved headers — the bridge's own dispatch
// bookkeeping: route-id, route-override, source-id, content-type — are
// removed via messaging.StripInternalOnlyHeaders. BRIDGE-TO-BRIDGE
// propagated reserved headers (correlation/causation/idempotency/dedup/
// ordering/tenant/trace/forwarded-*) and application headers pass
// through unchanged.
//
// CAVEAT — public SSE endpoints. The BRIDGE-TO-BRIDGE pass-through above
// is safe only while every subscriber is internal. Those retained keys
// (x-bridge.tenant-id, x-bridge.forwarded-from, x-bridge.correlation-id,
// the W3C traceparent/tracestate, and the other x-bridge.* propagated
// headers) are serialised into the SSE event payload and streamed to
// every connected subscriber, and a sender cannot tell a peer bridge
// from an external client. If ANY SSE endpoint is publicly reachable the
// operator MUST either keep it internal behind a per-destination egress
// ACL, or strip the full x-bridge.* namespace (plus traceparent/
// tracestate) from the stream at the edge — otherwise tenant, routing,
// correlation, and trace metadata leaks to external clients. Only the
// INTERNAL-ONLY subset is stripped here by design (H2).
//
// SSE per-write deadline. The SSE handler re-arms a per-frame write
// deadline (Config.WriteTimeout, default 15s) via http.ResponseController
// before every frame. Re-arming on each write (a) overrides a fronting
// HTTP server's global WriteTimeout, which would otherwise kill a
// healthy long-lived stream, and (b) bounds any single frame so a
// stalled subscriber is evicted — the next Write fails and the handler
// returns — instead of pinning the broadcast goroutine. Deadline support
// is best-effort: a writer without it (e.g. httptest.ResponseRecorder, or
// a fronting middleware that wraps the ResponseWriter without Unwrap) is
// tolerated, and the sender logs one warning at stream start so the
// disabled eviction is visible rather than silent.
//
// Ingress body limits and strict decoding. The request body is capped by
// Config.MaxBodySize (default 1 MiB). A breach maps to 413
// (http.MaxBytesError), distinct from the 400 used for malformed JSON.
// The decoder accepts exactly one JSON value: trailing tokens (a second
// object, an array, or garbage) are rejected with 400 rather than
// silently ignored. Reserved x-bridge.* keys supplied by an external
// producer are stripped at ingress (messaging.NewEnvelope); the first-
// class idempotency / dedup / ordering keys are accepted only through
// their dedicated non-reserved headers (Idempotency-Key, X-Dedup-Id,
// X-Ordering-Key) and re-stamped on the trusted side.
//
// Authentication. When Config.APIKey is set, requests must present the
// key in X-API-Key or "Authorization: Bearer <key>" (constant-time
// compared). A 401 always carries a RFC 7235 WWW-Authenticate: Bearer
// challenge. An inline api_key shorter than the enforced minimum (16
// chars) is rejected at decode time; credential-resolved keys are
// validated at the credential layer, not re-validated post-resolution.
//
// API key length floor (release note). The 16-character inline api_key
// minimum above is enforced at Config.Validate / decode time, and the
// rejection error names the 16-character minimum as the cause. A short
// inline key that an earlier build accepted is a breaking change and
// must be lengthened to >=16 characters; credential-resolved keys are
// unaffected.
//
// WARNING — the api_key and the forward token MUST be distinct secrets.
// Every client presents the api_key on each request, so reusing that
// same value as the forward token (Factory.WithForwardToken /
// ForwarderConfig.ForwardToken) would let any authenticated caller send
// a valid X-Bridge-Forward-Token and thereby spoof X-Bridge-Forwarded —
// reopening the H1 header-spoofing class the token exists to close (see
// "Cluster forward trust" above). Provision two independent secrets.
//
// # Deferred cross-cutting work (tracked outside this package)
//
// The following require edits to shared trees and are intentionally NOT
// made here:
//
//   - Composition-root wiring of an identical forward token into every
//     peer's Factory.WithForwardToken and ForwarderConfig.ForwardToken.
//     The token authenticates a peer: a token-proven marker is processed
//     locally to terminate the forward chain, and a spoofed marker is
//     rejected. It is NOT required for loop safety — an untrusted marker
//     on a route this node does not own is refused with 508 (neither
//     processed nor re-forwarded), so even an untokened cluster fails
//     closed instead of entering an A->B->A forwarding loop. Wiring the
//     token additionally lets a trusted peer's forward terminate locally
//     across a transient routing disagreement.
//   - A fronting server's global WriteTimeout
//     (deployment bootstrap) should be split or cleared for SSE
//     listeners; the adapter's per-write deadline mitigates the common
//     case but cannot change a server it does not own.
//   - Full authenticated bridge-to-bridge envelope propagation (tenant,
//     correlation, causation, forwarded-from/hop) across an HTTP hop.
//     This adapter forwards the first-class idempotency / dedup /
//     ordering keys losslessly via trusted HTTP headers; the remaining
//     BRIDGE-TO-BRIDGE metadata needs a signed/authenticated envelope
//     contract in domain/messaging before the forwarder may carry it
//     without trusting client-supplied reserved headers.
package transport
