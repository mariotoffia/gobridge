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
  (`domain/routing/policy.go`). Before this change a filtered message
  inherited the permanent-failure sink: `on_permanent_failure` defaults to
  `dlq`, so intentionally-filtered messages accumulated in the DLQ by default.
  After: filtered messages are dropped and counted in the `MessagesFiltered`
  metric instead of landing in the DLQ (`runtime/route/dispatch.go`).
  Any route that relied on filtered messages being retained in the DLQ must now
  set `on_filtered: dlq` explicitly, and a DLQ store must be configured — the
  runtime validator rejects the config and startup fails when `on_filtered=dlq`
  is set without one (`runtime/validator.go`). Otherwise filtered messages
  are dropped;
  watch the `MessagesFiltered` metric to confirm expected filter volume. The
  decoupling is deliberate: a high-volume allow-list filter no longer floods the
  DLQ or drives redrive loops through the permanent-failure default.

### DLQ

- **DLQ list ordering is now deterministic oldest-first.** The DLQ list and
  `/dlq/messages` endpoints return entries by ascending `FailedAt` with the
  entry ID as a stable tiebreaker, so operators triage the oldest failures first
  and paginate (`since` / `before`) without entries shifting between calls.
  Contract: `ports/stores.go`; enforced by every backend
  (`adapters/native/store/memorydlq/store.go`,
  `adapters/aws/store/dynamodbdlq/acl_store.go`). Tooling that assumed a
  different or unspecified order must adjust.

- **`/dlq/messages` reports `has_more`, not `total`.** The old `total` reported
  `min(matched, limit+offset)` and lied once the backlog exceeded the page
  window. The response now carries a truthful `has_more` boolean
  (`httpapi/admin_dlq.go`). Clients paginating on `total` must switch to
  `has_more`.

- **DLQ redrive is at-least-once and binding-scoped.** Redrive injects each
  entry first — under a fresh envelope ID with a causation link to the
  original — and deletes it only after the inject is confirmed, so a failed or
  refused inject never loses the message or its evidence; a crash between a
  confirmed inject and the delete re-drives the entry on the next attempt (a
  bounded duplicate, never a loss). A fan-out route redrives only the binding
  that failed, not the healthy N-1. A redrive returns HTTP 207 when any entry
  fails — inspect the per-entry body. See
  [ADR 0015](adr/0015-dlq-redrive-inject-then-delete.md), which supersedes
  ADR 0006.

### HTTP / SSE

- **SSE frames no longer emit an `id:` field.** Breaking wire change. Emitting
  `id:` made `EventSource` clients send `Last-Event-ID` on reconnect and expect a
  replay window the bridge does not provide (SSE egress is at-most-once). Frames
  now carry `event:` and `data:` only; the envelope ID stays in the JSON payload
  (`adapters/http/transport/doc.go`,
  `adapters/http/transport/sender_sse.go`). Clients relying on
  `Last-Event-ID` resume must stop — there was never a replay window to resume
  into.

### Admin API auth

- **Auth throttle keys on `RemoteAddr`, ignoring `X-Forwarded-For`.** The
  auth-failure limiter keys on the transport peer (`RemoteAddr` host), never
  `X-Forwarded-For`, because XFF is client-spoofable and would let an attacker
  rotate values to evade the limiter or exhaust its tracked-client map
  (`httpapi/admin.go`). Deployments behind a proxy will see the limiter
  key on the proxy's address unless per-client isolation is handled at the edge.
  XFF is still used for audit attribution only, never as a security control.

- **`Retry-After` is derived from the auth-failure window.** A throttled client's
  `Retry-After` hint tracks `auth_failure_window` (rounded up, floored at 1s)
  instead of a fixed constant (`httpapi/auththrottle.go`). Tuning
  `auth_failure_window` moves the advertised retry hint with it.

### Transports

- **MQTT `unmatched_grace` defaults to 30s.** A persistent session
  (`clean_start=false`) buffers a publish that matches no binding for a 30s
  startup grace; past the window a publish on a topic no still-desired
  subscription covers is acked, dropped, and best-effort unsubscribed as an
  orphan (covered topics are retained un-acked instead — see
  `MQTTRouterCoveredRetained`). Shorten it for
  latency-sensitive startup at the cost of retaining an early backlog longer
  for a still-reconciling subscription. See
  [ADR 0003](adr/0003-mqtt-persistent-session-hygiene.md).

- **SQS receiver no longer signals `Started()` on an init failure.** A receiver
  whose client creation or queue-URL resolution fails returns `Run`'s error
  without closing `Started()`, so a readiness probe never briefly observes a
  ready route for a receiver that failed to start
  (`adapters/aws/transport/sqs/receiver.go`, `:162`). A supervisor must
  watch `Run`'s error, not select solely on `Started()`.

### MQTT adversarial-review remediation

- **Ingress cap violations no longer kill the session.** A publish
  violating a local representational cap (`max_payload_bytes`, metadata
  bytes, User Property count) while fitting the advertised Maximum Packet
  Size — which a compliant broker forwards from any authorized publisher —
  previously latched the session TERMINAL, and broker redelivery on every
  resume made that a permanent, publisher-triggerable restart loop. It is now
  **acked-and-dropped**: an acknowledged, counted loss
  (`MQTTIngressPoisonDropped`, Error log per violation class). Alert on the
  new metric; see [the runbook](runbooks/mqtt-ingress-poison.md). Malformed
  packets and totals above the advertised maximum (broker bugs) remain
  session-terminal.
- **Pre-first-reconcile backlog is retained, never orphan-dropped
  ** Before the first `Reconcile` of a process lifetime every topic
  counts as covered, so a CONNACK backlog replayed ahead of the first plan
  can no longer be PUBACK-dropped and its live topic unsubscribed under a
  delayed startup. Genuine orphans converge one reconcile later.
- **An emit error now requests bounded session recovery.** A
  stranded un-acked delivery (route runner refused it; MQTT does not
  redeliver on a live connection) previously pinned Receive-Maximum slots
  until an unrelated teardown. Durable sessions now recycle via the same
  rate-limited recovery a `Delivery.Retry` uses (Warn-logged,
  `MQTTSessionRecoveryRecycle`); expect redelivery-duplicates for in-flight
  siblings on `direct_hold` routes.
- **Two silent windows are now metered.** Recycle-window discards count on
  `MQTTRouterStalePurged` (previously the router's only silent drop); settlements whose connection cycled count on the new
  `MQTTAckAfterReconnect` (each is a guaranteed broker redelivery).
- **Reload success now has a convergence watchdog.** A committed
  reload whose sessions never reach the broker (ACL-denied topic, rotated
  credentials) flips `ConfigDegraded` to 1 with an `applied but ... not
  converged` deep-health reason after the transport's activation budget,
  clearing when sessions converge. Alert on `ConfigDegraded`; reload success
  alone no longer implies a working transport.
- **Worst-case failover budget is disclosed at every build.**
  Exclusive sessions without `failover_slo` log their computed budget
  (`≈336s` with HA-profile + MQTT defaults). See
  [failover timing](transports/mqtt.md#exclusive-mode-lease-store-and-failover-timing).
- **New construction-time warnings.** `session_mode: persistent` +
  `clean_start: true` (wipes the offline backlog every restart) and
  persistent + `client_id_suffix: hostname` (strands broker queues on every
  Deployment/ECS rollout, — see
  [Deployment identity](transports/mqtt.md#deployment-identity)).
- **Circuit-breaker outcomes are generation-safe under concurrency
  ** The MQTT sender and HTTP forwarder admit requests through the
  new `ports.CircuitBreakerAdmitter` surface, so an outcome landing after a
  breaker state transition is discarded as stale instead of releasing a
  half-open probe it never held.
- **The SIGTERM drain shares the shutdown budget (AWS file-based
  deployment).** The runtime drain now derives from the app shutdown context
  instead of stacking a second fresh budget after it, so worst-case shutdown
  fits the documented 60s termination grace.

## Upgrade checklist

- Set `on_filtered: dlq` (and configure a DLQ store) on any route that must keep
  intentionally-filtered messages in the DLQ — the new default is `drop`.
- Repoint any DLQ tooling from `total` to `has_more`, and expect oldest-first
  order.
- Drop any `Last-Event-ID` reliance in SSE consumers.
- Confirm auth-throttle keying against your proxy topology.
- Supervise SQS receivers on `Run`'s error, not `Started()` alone.
