# Troubleshooting by `shared.ErrorCode`

Every error returned across a GoBridge port carries a `*shared.BridgeError`
with a stable `ErrorCode` (defined in [`domain/shared/errors.go`](../domain/shared/errors.go))
and an `ErrorClass` (`transient` / `permanent` / `expired` / `rejected`)
that drives runtime routing decisions (retry, DLQ, drop). This page is the
operator-facing index: when a code shows up in logs, metrics, or DLQ
entries, find the section here for what it means and how to recover.

For the architectural model, see [ARCHITECTURE.md §15 — Error
Classification](../ARCHITECTURE.md#15-error-classification). For the
canonical names, see the **shared kernel** rows in
[UBIQUITOUS.md](../UBIQUITOUS.md).

> **Recovery actions** below are operator-side. The runtime already
> retries `transient` codes per the route's `BackoffPolicy`, sends
> `permanent` codes to the DLQ (when configured), routes `expired` codes
> through the route's `ExpiredAction`, and drops `rejected` codes silently.
> Manual recovery only applies after the automated path is exhausted or
> when the underlying cause must be fixed at the source.

## How to use this page

1. Find the `code` field in your structured log line, DLQ entry
   (`DLQEntry.ErrorCode`), or metric tag (`gobridge_errors_total{code="…"}`).
2. Jump to the matching section below.
3. Apply the recovery action; if a metric is listed under "Related metrics",
   verify the fix by watching it return to baseline.

---

## Transient codes (auto-retried)

These represent recoverable conditions. The runtime retries with backoff;
they only need operator attention when the rate stays elevated or the
route hits its `MaxRetries` and the envelope lands in the DLQ.

### `TIMEOUT`

* **When you see it.** A transport, store, or processor exceeded its
  configured deadline (publish, claim, processor execution, HTTP call).
* **Likely cause.** Slow broker / database, network blip, undersized
  per-call timeout, downstream consumer applying back-pressure.
* **Recovery.** Confirm the dependency is reachable and not throttled
  upstream. If chronic, raise the operation's timeout in the relevant
  config block (transport `options`, processor `timeout`, store driver
  config) — but only after confirming the latency is legitimate, not a
  symptom of a broken peer.
* **Related metrics.** `gobridge_errors_total{code="TIMEOUT"}`,
  per-route latency histograms.

### `CONNECTION_LOST`

* **When you see it.** A long-lived transport session (MQTT, AMQP 0-9-1,
  AMQP 1.0, Service Bus) was disconnected mid-operation.
* **Likely cause.** Broker restart, network partition, idle-timeout
  enforcement at the broker, TLS handshake failure on reconnect.
* **Recovery.** Adapters reconnect automatically (autopaho, amqp091
  reconnect loop, ASB SDK). If reconnects loop, check broker logs for
  rejected credentials, duplicate `client_id`, or TLS issues.
* **Related metrics.** Session reconnect counters,
  `gobridge_errors_total{code="CONNECTION_LOST"}`.

### `UNAVAILABLE`

* **When you see it.** A dependency declared itself temporarily unable
  to serve (HTTP 5xx with retryable semantics, SDK "service unavailable"
  errors, store-side unavailability).
* **Likely cause.** Downstream service degradation, control-plane event,
  region-wide throttling.
* **Recovery.** Wait through the backoff window. Escalate to the
  dependency's status page if duration exceeds your SLO budget.
* **Related metrics.** `gobridge_errors_total{code="UNAVAILABLE"}`,
  upstream/downstream success ratios.

### `THROTTLED`

* **When you see it.** A broker or AWS/Azure SDK returned a rate-limit
  error (SQS `ThrottlingException`, ASB throttle, MQTT broker
  rate-limit).
* **Likely cause.** Sustained throughput above the provisioned quota,
  burst above token-bucket capacity, noisy-neighbour tenant.
* **Recovery.** Inspect the `RetryAfter` hint on the error; the runtime
  honours it. If chronic, request a quota increase, enable batching, or
  reduce per-route `MaxInFlight` to smooth the burst pattern.
* **Related metrics.** `gobridge_errors_total{code="THROTTLED"}`,
  outbox depth gauge (rises when egress is throttled).

### `BROKER_BUSY`

* **When you see it.** The transport reported broker-side overload
  (e.g. AMQP `resource-limit-exceeded`, MQTT broker reactive disconnect
  pattern).
* **Likely cause.** Broker queue saturated, shared infrastructure under
  high load, head-of-line blocking from another producer.
* **Recovery.** Verify broker dashboards. Reduce route `MaxInFlight` or
  enable `shared_outbox` delivery so persistence absorbs the burst.
* **Related metrics.** Broker queue depth, outbox depth gauge.

### `TEMPORARY_AUTH_FAILURE`

* **When you see it.** Credential resolution succeeded but the broker
  responded "auth not yet propagated" or similar (typically during IAM
  propagation, key rotation in flight, or token refresh races).
* **Likely cause.** Just-rotated credential not yet active; clock skew
  between issuer and broker.
* **Recovery.** Usually resolves within seconds. If it persists, force a
  credential refresh (admin endpoint or restart the credential resolver)
  and verify clock sync on the bridge host.
* **Related metrics.** Credential refresh counters.

### `NO_ROUTE_OWNER`

* **When you see it.** In a clustered deployment, an envelope arrived
  on an instance that does not currently own the lease for the route's
  exclusive session.
* **Likely cause.** Lease just transferred between instances; the cluster
  forwarder is in the middle of routing the message to the new owner.
* **Recovery.** Self-heals as the forwarder retries against the new
  owner. If chronic, inspect the `LeaseStore` (DynamoDB) for stuck or
  stale lease records and verify all instances see the same store.
* **Related metrics.** Lease churn counters,
  `gobridge_errors_total{code="NO_ROUTE_OWNER"}`.

### `FORWARD_FAILED`

* **When you see it.** The inter-instance forwarder could not deliver an
  envelope to the lease holder (HTTP error, timeout, DNS).
* **Likely cause.** Owner instance is unreachable, endpoint discovery
  is stale, security group / network policy blocking peer traffic.
* **Recovery.** Verify peer connectivity (the endpoints stored on
  `LeaseInfo.Endpoints` must be routable). Check the cluster
  endpoint resolver configuration.
* **Related metrics.** `gobridge_errors_total{code="FORWARD_FAILED"}`,
  cluster forward latency.

### `PROCESSOR_TIMEOUT`

* **When you see it.** A processor exceeded its `ProcessorTimeout`.
* **Likely cause.** Slow external call inside a processor (HTTP enrich,
  database lookup), unbounded loop in custom processor logic.
* **Recovery.** Profile the offending processor; raise its timeout only
  if the latency is legitimate and bounded. Otherwise refactor to push
  the slow work asynchronously or behind a circuit breaker.
* **Related metrics.** Per-processor latency histograms,
  `gobridge_errors_total{code="PROCESSOR_TIMEOUT"}`.

---

## Permanent codes (DLQ-bound)

Retry will not help. The runtime sends these to the DLQ (when configured)
or drops per the route's `FailureAction`. Operator action is required at
the source of the failure.

### `NOT_AUTHORIZED`

* **When you see it.** The broker / store / API rejected the request
  with an authentication failure (bad credentials, expired token, missing
  permission).
* **Likely cause.** Credential rotated without updating the bridge,
  IAM policy missing required action, role assumption misconfigured.
* **Recovery.** Verify the credential resolver returned the expected
  identity (`pms://`, `file://`, role chain). Ensure the IAM / RBAC policy
  grants the action the failing call needs (e.g. `sqs:ReceiveMessage`,
  `dynamodb:UpdateItem`, MQTT `subscribe` ACL).

### `FORBIDDEN`

* **When you see it.** Authentication succeeded but the principal lacks
  permission to perform the action on the specific resource (topic,
  queue, key).
* **Likely cause.** Resource-level policy gap; topic/queue ACL forbids
  the bridge's principal; cross-account access not granted.
* **Recovery.** Update the resource-side policy to grant the action.
  Verify the ARN / topic name in the policy matches the configured
  destination exactly.

### `NOT_FOUND`

* **When you see it.** A queue, topic, key, or stored record does not
  exist.
* **Likely cause.** Misconfigured `Address` or `queue_url`, resource
  was deleted, store table was not provisioned.
* **Recovery.** Verify the destination spelling (case-sensitive on most
  brokers). For DynamoDB stores, confirm the table exists and the bridge
  has `DescribeTable` permission. For MQTT, check that the broker creates
  topics on demand vs. requiring pre-provisioning.

### `INVALID_PAYLOAD`

* **When you see it.** A processor or sender rejected the envelope's
  payload as malformed.
* **Likely cause.** Producer changed its schema without coordination,
  encoding mismatch (UTF-8 vs binary), required field missing.
* **Recovery.** Fix at the producer or add a transform processor to
  normalise. The DLQ entry contains the original payload — replay after
  the producer is corrected.

### `PAYLOAD_TOO_LARGE`

* **When you see it.** The envelope exceeded the transport's maximum
  payload size (SQS 256 KB, MQTT broker-configured, ASB 1 MB tier).
* **Likely cause.** Producer batching too aggressively, embedded blob in
  the payload, headers grew unboundedly.
* **Recovery.** Move large payloads to object storage and pass a
  reference, or split at the producer. The bridge does not silently
  truncate.

### `INVALID_TOPIC`

* **When you see it.** The address resolves to a topic / routing key the
  transport cannot accept (illegal characters, wildcards in publish,
  empty path segment).
* **Likely cause.** Templated address produced an empty or invalid
  string at runtime.
* **Recovery.** Inspect the `DispatchPlan.Address` in the DLQ entry's
  context; fix the destination resolver or binding template that
  produced it.

### `PROTOCOL_ERROR`

* **When you see it.** The broker reported a protocol-level violation
  (malformed frame, unsupported version handshake, illegal sequence).
* **Likely cause.** Adapter / broker version mismatch, broker
  enforcing a stricter dialect, corrupted message in transit.
* **Recovery.** Check the adapter version against the broker version
  matrix. Capture the adapter debug log to identify which frame the
  broker rejected.

### `SCHEMA_VIOLATION`

* **When you see it.** A processor with schema validation rejected the
  envelope (registered schema validator returned non-conformance).
* **Likely cause.** Producer changed payload schema; schema registry
  drift; wrong schema selected for the subject.
* **Recovery.** Reconcile the producer schema with the validator's
  expectation. Use the DLQ entry to bisect when the schema changed.

### `QOS_NOT_SUPPORTED`

* **When you see it.** The configured MQTT/AMQP QoS level is not
  supported by the broker or the subscription's grant.
* **Likely cause.** Broker capped at QoS 1 while the route asked for
  QoS 2; subscription grant downgraded silently.
* **Recovery.** Lower the route's QoS to a level the broker grants, or
  upgrade the broker. Verify subscription grants in broker logs.

### `NOT_SUPPORTED`

* **When you see it.** A capability is requested that the adapter or
  underlying technology does not implement (e.g. transactional outbox
  on a store driver that only supports eventual consistency).
* **Likely cause.** Wiring an adapter that does not implement an
  optional port; configuration enables a feature outside the adapter's
  envelope.
* **Recovery.** Disable the unsupported feature in config or swap the
  adapter for one that supports it. See [PLUGIN.md](../PLUGIN.md) for
  port-implementation matrices.

### `VERSION_MISMATCH`

* **When you see it.** Optimistic concurrency check failed on a config
  commit, an outbox record's expected version did not match, or a
  schema-version negotiation rejected the value.
* **Likely cause.** Two operators committed config concurrently; an
  outbox record was modified out-of-band; mid-rolling-deploy with mixed
  schema versions.
* **Recovery.** Reload, rebase the change onto the latest version, and
  re-submit. For outbox version mismatches, inspect the record state
  before retrying — this often signals a fencing-token problem (see
  `STALE_FENCING_TOKEN`).

### `ALREADY_EXISTS`

* **When you see it.** A create operation hit a uniqueness constraint
  (duplicate session ID, duplicate route ID, store-side primary-key
  conflict).
* **Likely cause.** Re-run of an idempotent operation without supplying
  a deterministic key, or an actual duplicate definition in config.
* **Recovery.** Inspect the conflicting object; either reuse the
  existing one or assign a fresh ID. For store-side conflicts, this
  usually points at a missing `Idempotency key` header upstream.

### `STALE_FENCING_TOKEN`

* **When you see it.** A guarded write (outbox claim/complete, lease
  renewal, route forward) was rejected because the caller's
  `LeaseToken.Version` is older than the current owner's.
* **Likely cause.** Lease was reassigned (peer instance won the
  election) while this instance still had work in flight.
* **Recovery.** Self-heals — the affected instance steps down and
  re-acquires. Investigate only when the rate is high enough to suggest
  thrashing leases (clock skew, undersized `LeaseTTL`,
  network partition between instances and the `LeaseStore`). See
  [ARCHITECTURE.md §16 — Clustered Deployment](../ARCHITECTURE.md#16-clustered-deployment).
* **Related metrics.** Lease churn, outbox-claim rejection counters.

### `DUPLICATE_RECORD`

* **When you see it.** The store rejected an insert/update because the
  record (envelope ID, idempotency key) is already present.
* **Likely cause.** Producer re-published the same envelope; outbox
  drainer's at-least-once delivery overlapped with a successful prior
  send.
* **Recovery.** Usually safe to ignore — the dedup is doing its job.
  Investigate only if accompanied by elevated `THROTTLED` or
  `STALE_FENCING_TOKEN` rates (suggests an instance is double-claiming).

### `MESSAGE_EXPIRED`

* **When you see it.** The envelope's `ExpiresAt` passed before delivery
  could complete.
* **Likely cause.** Backpressure (outbox depth) exceeded the message's
  TTL; downstream slowness; clock skew between producer and bridge.
* **Recovery.** Determined by the route's `ExpiredAction` (`drop` or
  `dlq`). If the rate is high, reduce upstream rate, scale egress, or
  raise the producer's TTL.
* **Related metrics.** Outbox depth, route end-to-end latency.

### `PROCESSOR_PANIC`

* **When you see it.** A processor panicked. The runtime recovered the
  panic and classified it as `permanent` because panics indicate a bug.
* **Likely cause.** Nil-pointer or out-of-range access in custom
  processor code; assumption about envelope shape that does not hold.
* **Recovery.** Treat as a P1 bug — the DLQ entry's `LastError` carries
  the panic message and stack. Fix the processor and replay the affected
  DLQ entries.
* **Related metrics.** `gobridge_errors_total{code="PROCESSOR_PANIC"}`.

### `INTERNAL`

* **When you see it.** An invariant violation deep in the runtime or
  an adapter (e.g. an adapter forgot to inject its clock; an envelope
  constructor returned a missing-dependency sentinel).
* **Likely cause.** Programmer error in wiring or adapter
  implementation. Should never appear in a correctly built bridge.
* **Recovery.** File a bug. Capture the structured log entry in full —
  the `Context` map contains the diagnostic breadcrumb.
* **Related metrics.** Any non-zero rate is anomalous.

### `INVALID_OUTBOX_RECORD`

* **When you see it.** An attempt to construct an `OutboxRecord` was
  missing required identity fields (envelope ID, partition key,
  address).
* **Likely cause.** Misconfigured binding (no resolved `Address`),
  destination resolver returning empty values, or admin-API caller
  posting an incomplete record.
* **Recovery.** Validate inputs at the calling layer. The error's
  `Context` lists which fields were empty.

### `OUTBOX_NOT_CLAIMABLE`

* **When you see it.** A `Claim` call hit a record that is in a
  terminal state (`completed` / `expired`) or under a newer fencing
  token.
* **Likely cause.** Race between two drainers (resolved by fencing); the
  record was already drained by the previous lease owner.
* **Recovery.** No action — the runtime moves on to the next record.
  If chronic, investigate lease churn (see `STALE_FENCING_TOKEN`).

### `OUTBOX_NOT_IN_CLAIMED_STATE`

* **When you see it.** `Complete` was called against a record that is
  not in the `claimed` state.
* **Likely cause.** Drainer tried to complete a record it did not
  claim; state-machine bug in a custom store implementation.
* **Recovery.** Indicates a logic error. For native / managed stores
  this should not occur; for custom store adapters, verify the store
  implements the [`OutboxStore` contract test
  suite](../adapters/native/store/sqlite).

### `OUTBOX_ALREADY_TERMINAL`

* **When you see it.** `Expire` was invoked on a record that has
  already reached `completed` or `expired`.
* **Likely cause.** Race between expiry sweeper and drainer; benign
  ordering effect.
* **Recovery.** No action required. The runtime treats it as an
  idempotent no-op.

---

## Rejected codes (silent drop)

The runtime drops these envelopes without sending them to the DLQ (the
envelope is being deliberately filtered or rejected as malformed input
the bridge cannot meaningfully retain).

### `MESSAGE_FILTERED`

* **When you see it.** A filter processor explicitly rejected the
  envelope based on its rules.
* **Likely cause.** The envelope did not match the route's filter
  predicate — this is the intended behaviour of the filter.
* **Recovery.** No action unless the filter rule itself is wrong.
  Audit filter expressions against representative payloads.
* **Related metrics.** `gobridge_filter_dropped_total`.

---

## Runtime-only codes

### `NO_BINDING_MATCH`

* **When you see it.** The destination resolver could not match the
  envelope to any binding configured for the route.
* **Likely cause.** Dynamic-routing predicate fell through; the route
  has no catch-all binding; binding `Address` template returned empty.
* **Recovery.** Add a default binding or refine the resolver. See
  [Scenario 12: Dynamic destination routing](scenarios/12-dynamic-destination-routing.md).
* **Class.** Permanent — acted on per the route's `FailureAction`.

### `POISON_MESSAGE`

* **When you see it.** A message could not be processed after exhausting
  retries and is classified as a "poison pill" by the runtime.
* **Likely cause.** Persistent processor or sender failure on the same
  envelope; combined with `MaxRetries` / `MaxReplayAttempts` exhaustion.
* **Recovery.** Inspect the DLQ entry. The `LastError` field carries
  the underlying cause; treat that as the real failure to fix.
* **Class.** Permanent.

---

---

## Adapter & runtime diagnostic metrics

These counters were added to make specific failure and degradation modes
observable. The names below are the **verbatim** OTel instrument names emitted
by the transport adapters (namespace `GoBridge/Runtime`) -- the bridge does not
add a `gobridge_` prefix or a `_total` suffix; a Prometheus backend may apply
its own normalization downstream. Each has a real emission site; alert on a
sustained non-zero rate.

### MQTT (`adapters/mqtt/transport/paho`)

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `MQTTRouterBuffered` | A publish arrived before any matching handler was registered (e.g. a persistent-session backlog delivered on CONNACK before `Receiver.Run`) and was held in the router's bounded pending buffer instead of being dropped (`acl_router.go:188`). | Normal in small bursts at reconnect; a large or growing value means handlers register too slowly or the backlog exceeds the buffer. |
| `MQTTSessionTakeover` | A server disconnect with reason code `0x8E`/`0x8F` (*session taken over*): another client connected with the same ClientID (`session_lifecycle.go:189,223`). | Two instances share a `client_id` and keep kicking each other -- give each replica a distinct ClientID or use an exclusive session. |

### AMQP 0-9-1 (`adapters/amqp/transport/amqp091`)

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `AMQP091DelayedRetryUnhonored` | A `Retry(after > 0)` was requested but AMQP 0-9-1 has no native delayed-redelivery primitive, so the nack requeues immediately and the requested backoff spacing is lost (`acl_delivery.go:130`). | Poison messages can hot-loop on a classic queue. Add an `x-delivery-limit` / dead-letter-exchange guard, or move retry spacing to the broker. |
| `AMQP091ReconnectRaceRetried` | A permanent-classified consume failure (403 `ACCESS_REFUSED` on an exclusive consumer, or 404 `NOT_FOUND` mid-topology-redeclare) was retried as a transient reconnect race (`receiver.go:132`). | Expected briefly after a reconnect/partition; climbing past the retry budget means a genuine misconfiguration, not a race. |

### AMQP 1.0 (`adapters/amqp/transport/amqp10`)

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `AMQP10DelayedRetryUnhonored` | A `Retry` with a positive delay was handed back to the broker (`ModifyMessage`) because AMQP 1.0 has no portable client-side delayed-redelivery primitive; the broker decides when to redeliver (`acl_delivery.go:202`). | The configured retry backoff is effectively broker-driven on this transport. |

### HTTP (`adapters/http/transport`)

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `HTTPIngressDuplicates` | An ingress request was short-circuited by the receiver's bounded idempotency window: the presented `Idempotency-Key` / `X-Dedup-Id` was already processed, so the request is acked (200) without re-emitting (`receiver.go:288`). | Healthy dedup of producer retries; a spike may indicate an aggressively retrying client. |
| `HTTPForwardLoopRefused` | A request already carrying `X-Bridge-Forwarded` resolved to a remote route and was refused with 508 instead of re-forwarded (`receiver.go:251`). | A cluster routing disagreement (A->B->A). Reconcile ownership/routing or wire the forward token. |
| `HTTPForwardBreakerOpen` | A cluster forward was rejected by the forwarder's circuit breaker without a network attempt (peer considered down) (`forwarder.go:235`). | A peer node is down or flapping; the breaker is failing fast. |
| `SSENoSubscribers` | A `Send` completed with zero connected SSE clients -- at-most-once, so the source is still acked but nobody received it (`sender_sse.go:215`). | Alert when subscribers are expected; the event is gone. |
| `SSEAllDropped` | A `Send` where every connected client's buffer was full, so the event was dropped for 100% of subscribers (`sender_sse.go:241`). | All SSE clients are too slow; the broadcast reached nobody. |
| `SSEDeadlineUnsupported` | An SSE stream whose `ResponseWriter` chain cannot set per-write deadlines (`sender_sse.go:358`). | Slow-client eviction is inert for those streams; a stalled reader can pin a goroutine. Front SSE with a writer that supports deadlines. |

## Quick reference

| Code | Class | DLQ? |
|---|---|---|
| `TIMEOUT`, `CONNECTION_LOST`, `UNAVAILABLE`, `THROTTLED`, `BROKER_BUSY`, `TEMPORARY_AUTH_FAILURE`, `NO_ROUTE_OWNER`, `FORWARD_FAILED`, `PROCESSOR_TIMEOUT` | `transient` | Only after retries exhausted |
| `NOT_AUTHORIZED`, `FORBIDDEN`, `NOT_FOUND`, `PROTOCOL_ERROR`, `QOS_NOT_SUPPORTED`, `NOT_SUPPORTED`, `VERSION_MISMATCH`, `ALREADY_EXISTS`, `STALE_FENCING_TOKEN`, `DUPLICATE_RECORD`, `PROCESSOR_PANIC`, `INTERNAL`, `INVALID_OUTBOX_RECORD`, `OUTBOX_NOT_CLAIMABLE`, `OUTBOX_NOT_IN_CLAIMED_STATE`, `OUTBOX_ALREADY_TERMINAL`, `NO_BINDING_MATCH`, `POISON_MESSAGE` | `permanent` | Yes (per route `FailureAction`) |
| `INVALID_PAYLOAD`, `PAYLOAD_TOO_LARGE`, `INVALID_TOPIC`, `SCHEMA_VIOLATION`, `MESSAGE_FILTERED` | `rejected` | No (silent drop) |
| `MESSAGE_EXPIRED` | `expired` | Per route `ExpiredAction` |

The authoritative source is [`domain/shared/errors.go`](../domain/shared/errors.go);
this page must stay in lockstep with that file.
