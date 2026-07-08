# Release notes

Operator-facing changes. Each entry names the behavior that changed and what an
operator must do about it. Entries link to an ADR where one records the decision.

## Unreleased

Behavior changes from the production-readiness work. Review before upgrading —
several are breaking at the wire or observable in operations.

### Routing

- **Filtered messages now default to `drop`, not the DLQ.** Silent
  behavior-change, observable in operations. A message a processor intentionally
  discards (`shared.ErrMessageFiltered` — e.g. a filter allow-list/deny-list
  drop) now has its own route policy `on_filtered`, which defaults to `drop`
  (`domain/routing/policy.go:166-172`). Before this change a filtered message
  inherited the permanent-failure sink: `on_permanent_failure` defaults to
  `dlq`, so intentionally-filtered messages accumulated in the DLQ by default.
  After: filtered messages are dropped and counted in the `MessagesFiltered`
  metric instead of landing in the DLQ (`runtime/route/dispatch.go:359-382`).
  Any route that relied on filtered messages being retained in the DLQ must now
  set `on_filtered: dlq` explicitly, and a DLQ store must be configured — the
  runtime validator rejects the config and startup fails when `on_filtered=dlq`
  is set without one (`runtime/validator.go:308-311`). Otherwise filtered messages
  are dropped;
  watch the `MessagesFiltered` metric to confirm expected filter volume. The
  decoupling is deliberate: a high-volume allow-list filter no longer floods the
  DLQ or drives redrive loops through the permanent-failure default.

### DLQ

- **DLQ list ordering is now deterministic oldest-first.** The DLQ list and
  `/dlq/messages` endpoints return entries by ascending `FailedAt` with the
  entry ID as a stable tiebreaker, so operators triage the oldest failures first
  and paginate (`since` / `before`) without entries shifting between calls.
  Contract: `ports/stores.go:156-162`; enforced by every backend
  (`adapters/native/store/memorydlq/store.go:125`,
  `adapters/aws/store/dynamodbdlq/acl_store.go:373`). Tooling that assumed a
  different or unspecified order must adjust.

- **`/dlq/messages` reports `has_more`, not `total`.** The old `total` reported
  `min(matched, limit+offset)` and lied once the backlog exceeded the page
  window. The response now carries a truthful `has_more` boolean
  (`httpapi/admin_dlq.go:211-262`). Clients paginating on `total` must switch to
  `has_more`.

- **DLQ redrive is at-most-once and binding-scoped.** Redrive claims each entry
  by deleting it before injecting, so retries and concurrent admin instances
  never double-deliver; a crash in the delete→inject window loses the entry
  rather than duplicating it. A fan-out route redrives only the binding that
  failed, not the healthy N-1. A redrive returns HTTP 207 when any entry fails —
  inspect the per-entry body. See [ADR 0006](adr/0006-dlq-redrive-at-most-once.md).

### HTTP / SSE

- **SSE frames no longer emit an `id:` field.** Breaking wire change. Emitting
  `id:` made `EventSource` clients send `Last-Event-ID` on reconnect and expect a
  replay window the bridge does not provide (SSE egress is at-most-once). Frames
  now carry `event:` and `data:` only; the envelope ID stays in the JSON payload
  (`adapters/http/transport/doc.go:146-148`,
  `adapters/http/transport/sender_sse.go:439-442`). Clients relying on
  `Last-Event-ID` resume must stop — there was never a replay window to resume
  into.

### Admin API auth

- **Auth throttle keys on `RemoteAddr`, ignoring `X-Forwarded-For`.** The
  auth-failure limiter keys on the transport peer (`RemoteAddr` host), never
  `X-Forwarded-For`, because XFF is client-spoofable and would let an attacker
  rotate values to evade the limiter or exhaust its tracked-client map
  (`httpapi/admin.go:262-280`). Deployments behind a proxy will see the limiter
  key on the proxy's address unless per-client isolation is handled at the edge.
  XFF is still used for audit attribution only, never as a security control.

- **`Retry-After` is derived from the auth-failure window.** A throttled client's
  `Retry-After` hint tracks `auth_failure_window` (rounded up, floored at 1s)
  instead of a fixed constant (`httpapi/auththrottle.go:149-163`). Tuning
  `auth_failure_window` moves the advertised retry hint with it.

### Transports

- **MQTT `unmatched_grace` defaults to 30s.** A persistent session
  (`clean_start=false`) buffers a publish that matches no binding for a 30s
  startup grace, then acks and drops it and best-effort unsubscribes the orphan
  topic (`adapters/mqtt/transport/paho/config.go:190`). Shorten it for
  latency-sensitive startup at the cost of a higher chance of dropping an early
  publish for a still-reconciling subscription. See
  [ADR 0003](adr/0003-mqtt-persistent-session-hygiene.md).

- **SQS receiver no longer signals `Started()` on an init failure.** A receiver
  whose client creation or queue-URL resolution fails returns `Run`'s error
  without closing `Started()`, so a readiness probe never briefly observes a
  ready route for a receiver that failed to start
  (`adapters/aws/transport/sqs/receiver.go:105-108`, `:162`). A supervisor must
  watch `Run`'s error, not select solely on `Started()`.

## Upgrade checklist

- Set `on_filtered: dlq` (and configure a DLQ store) on any route that must keep
  intentionally-filtered messages in the DLQ — the new default is `drop`.
- Repoint any DLQ tooling from `total` to `has_more`, and expect oldest-first
  order.
- Drop any `Last-Event-ID` reliance in SSE consumers.
- Confirm auth-throttle keying against your proxy topology.
- Supervise SQS receivers on `Run`'s error, not `Started()` alone.
