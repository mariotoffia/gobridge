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

1. Find the `code` field in your structured log line or DLQ entry
   (`DLQEntry.ErrorCode`). Error codes are **not** exposed as a metric
   tag; the runtime emits a single route-scoped counter, `RouteErrors`
   (tagged `route_id`), for the aggregate error rate per route.
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
* **Related metrics.** `RouteErrors{route_id="…"}`,
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
  `RouteErrors{route_id="…"}`.

### `UNAVAILABLE`

* **When you see it.** A dependency declared itself temporarily unable
  to serve (HTTP 5xx with retryable semantics, SDK "service unavailable"
  errors, store-side unavailability).
* **Likely cause.** Downstream service degradation, control-plane event,
  region-wide throttling.
* **Recovery.** Wait through the backoff window. Escalate to the
  dependency's status page if duration exceeds your SLO budget.
* **Related metrics.** `RouteErrors{route_id="…"}`,
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
* **Related metrics.** `RouteErrors{route_id="…"}`,
  outbox depth gauge (rises when egress is throttled).

### `BROKER_BUSY`

* **When you see it.** The transport reported broker-side overload
  (e.g. AMQP `resource-limit-exceeded`, MQTT broker reactive disconnect
  pattern).
* **Likely cause.** Broker queue saturated, shared infrastructure under
  high load, head-of-line blocking from another producer.
* **Recovery.** Verify broker dashboards. Reduce route `MaxInFlight` or
  enable `shared_outbox` delivery so persistence absorbs the burst.
* **AMQP 0-9-1 fail-fast.** When RabbitMQ raises a memory or disk resource
  alarm it sends `connection.blocked` and stops reading publisher sockets.
  The amqp091 sender refuses to publish while that alarm is engaged and
  returns `BROKER_BUSY` at once rather than blocking on a publish the SDK
  cannot cancel (which would wedge every sender on the connection past its
  deadline). It clears on its own when the broker lifts the alarm -- fix the
  broker resource (grow the node, drain a backed-up queue, raise the
  watermark) rather than tuning the bridge.
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
* **Related metrics.** `CredentialRefreshFailures` (poll resolve failures) and
  `CredentialResolveFailure{code=…}` (resolver fetch failures by code). If the
  secrets backend itself is briefly unavailable, the resolver serves the
  last-known-good credential and increments `CredentialStaleServed` rather than
  failing the rebuild.

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
  `RouteErrors{route_id="…"}`.

### `FORWARD_FAILED`

* **When you see it.** The inter-instance forwarder could not deliver an
  envelope to the lease holder (HTTP error, timeout, DNS).
* **Likely cause.** Owner instance is unreachable, endpoint discovery
  is stale, security group / network policy blocking peer traffic.
* **Recovery.** Verify peer connectivity (the endpoints stored on
  `LeaseInfo.Endpoints` must be routable). Check the cluster
  endpoint resolver configuration.
* **Related metrics.** `RouteErrors{route_id="…"}`,
  cluster forward latency.

### `PROCESSOR_TIMEOUT`

* **When you see it.** A processor exceeded its `ProcessorTimeout`.
* **Likely cause.** Slow external call inside a processor (HTTP enrich,
  database lookup), unbounded loop in custom processor logic.
* **Recovery.** Profile the offending processor; raise its timeout only
  if the latency is legitimate and bounded. Otherwise refactor to push
  the slow work asynchronously or behind a circuit breaker.
* **Related metrics.** Per-processor latency histograms,
  `RouteErrors{route_id="…"}`.

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
  `sqs:ChangeMessageVisibility`, `dynamodb:UpdateItem`, MQTT `subscribe` ACL).
* **Reactive recovery.** When a rotation apply is rejected as `NOT_AUTHORIZED`,
  the refresher forces an immediate out-of-band credential re-resolve
  (rate-limited to one per 5s per URI) instead of waiting for the poll interval,
  so a freshly rotated secret is picked up quickly. A permanent backend
  `NOT_AUTHORIZED` is never masked by stale credentials -- it propagates.
* **Related metrics.** `CredentialResolveFailure{code=NOT_AUTHORIZED}` (resolver
  permission denials), `CredentialRotationApplied` (successful live applies).

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

### `INVALID_CONFIG`

* **When you see it.** A config was rejected — at startup (the bootstrap
  refuses to build) or on an admin transaction commit (`422`, the merged
  config fails validation and the running runtime is left untouched).
* **Likely cause.** A structurally-invalid or semantically-inconsistent
  config: a missing required field, a bad enum, a store path outside the EFS
  mount, a route referencing an undeclared transport, or a plugin option that
  would be erased.
* **Recovery.** The error wraps a `BlueprintValidationError` /
  `config.ValidationError` whose `Errors` list names the offending field paths
  (the commit response surfaces them as `validation_errors`). Read that list,
  fix the named field(s), re-validate, and re-apply — at startup, correct the
  bootstrap/config and restart; on a commit, PATCH the fixed values into the
  transaction and re-commit (see [Config Rollback](runbooks/config-rollback.md)).

### `INVALID_PAYLOAD`

* **When you see it.** A processor or sender rejected the envelope's
  payload as malformed.
* **Likely cause.** Producer changed its schema without coordination,
  encoding mismatch (UTF-8 vs binary), required field missing.
* **Recovery.** Fix at the producer or add a transform processor to
  normalise. The DLQ entry contains the original payload — replay after
  the producer is corrected.

**Special case — MQTT plaintext-credential startup failure.** A likely
first-run failure: the build (or a credential rotation) fails closed with

```
mqtt: username/password are sent in the MQTT CONNECT packet in cleartext but
not all broker_urls use a TLS scheme; use ssl:// (or mqtts://, tls://,
mqtt+ssl://, tcps://, wss://), or set allow_plaintext_credentials=true to
send credentials in cleartext anyway
```

MQTT sends CONNECT credentials in cleartext, and autopaho selects TLS purely
from the URL **scheme** — `tls.enable` on a `tcp://` URL is a silent no-op.
Recovery: change every `broker_urls` entry to a TLS scheme (usually just
`tcp://` → `ssl://` plus the broker's TLS port), or — only on a trusted
transport such as a private mesh or localhost test broker — set
`options.session.allow_plaintext_credentials: true`. The same gate re-runs on
credential rotation, so a rotation that first introduces credentials can trip
it at runtime too.

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
* **Recovery.** Treat as a bug — the DLQ entry's `LastError` carries
  the panic message and stack. Fix the processor and replay the affected
  DLQ entries.
* **Related metrics.** `RouteErrors{route_id="…"}`.

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
  suite](../adapters/native/store/sqliteoutbox).

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

## Adapter & runtime diagnostic metrics

These counters were added to make specific failure and degradation modes
observable. The names below are the **verbatim** OTel instrument names emitted
by the transport adapters (namespace `GoBridge/Runtime`) -- the bridge does not
add a `gobridge_` prefix or a `_total` suffix; a Prometheus backend may apply
its own normalization downstream. Each has a real emission site; alert on a
sustained non-zero rate.

### MQTT (`adapters/mqtt/transport/paho`)

All MQTT metrics carry a `session_id` tag (the effective `client_id`) unless
noted. Code references name the emitting function; the authoritative
per-metric contract lives on the constants in
`adapters/mqtt/transport/paho/metrics.go`.

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `MQTTPublishLatency` | Timer around every egress publish, success or failure (`Sender.Send`). | Broker acceptance (PUBACK/PUBCOMP for QoS 1/2) is slowing down; correlate with broker load and `MQTTPublishFailures`. |
| `MQTTPublishFailures` | An egress publish failed or the broker rejected it with a non-success reason code (`Sender.Send`); circuit-open rejections also count here with a `reason=circuit_open` tag (`CircuitBreakerSender.Send`). | The primary egress error counter — alert on a sustained non-zero rate. Failures surface to the route runner for retry/outbox handling, so a rising value is broker/topic trouble, not silent loss. |
| `MQTTConnectLatency` | Timer around a successful initial `Session.Start` connect (dial + CONNACK + connection-up callback). | Broker connection establishment is slowing (TLS handshake, auth backend, broker load). Not emitted for background reconnect attempts. |
| `MQTTReconcileLatency` | Timer around each successful subscription reconcile (`Session.reconcile`: SUBSCRIBE/UNSUBSCRIBE convergence). | The broker is slow to answer SUBACK/UNSUBACK; contributes directly to startup, reload, and failover time. |
| `MQTTHandlerPanics` | A router dispatch handler panicked; the panic is recovered and the delivery is left un-acked for broker redelivery (`router.fanout` / `router.emitOne`). No `session_id` tag. | A bug in the receive pipeline. The un-acked publish is redelivered, so a panicking handler can loop the same message — find and fix the panic. |
| `MQTTRouterBuffered` | A publish arrived before any matching handler was registered (e.g. a persistent-session backlog delivered on CONNACK before `Receiver.Run`) and was held in the router's bounded pending buffer during the `unmatched_grace` window instead of being immediately acked-and-dropped (`router.dispatchCore`). No `session_id` tag. | Normal in small bursts at reconnect; a large or growing value means handlers register too slowly or the backlog exceeds the buffer. Publishes still unmatched when the grace window closes are settled as covered-retained or orphan-dropped. |
| `MQTTRouterDropped` | A **QoS 0** publish was dropped under backpressure: the serialized dispatch queue was full under a flood, or the pre-registration pending buffer hit its entry/byte ceiling (`defaultPendingBytesLimit=64 MiB`), or an older QoS 0 entry was evicted to make room for QoS 1/2 (`router.enqueueDispatch`, `router.dropQoS0Overflow`, `router.evictOldestQoS0Locked`). The dispatch queue is sized to the effective `receive_maximum` (default **192**, not a fixed constant — see `docs/transports/mqtt.md`). QoS 1/2 is never dropped here — it blocks on the dispatch queue or evicts an older QoS 0 entry. Also incremented ONCE by a raw pre-decode terminal reject (`Session.rejectPredecodeIngress`: malformed packet or total size above the advertised maximum — a broker bug that also fails the session closed). | A QoS 0 flood is outrunning handler dispatch, or a large CONNACK backlog exceeded the pending cap before handlers registered. QoS 0 carries no delivery contract, so drops are expected under overload; a sustained rate means the consumer cannot keep up — add capacity or shed load upstream. A single count coinciding with a session terminal error is the pre-decode reject, not backpressure. |
| `MQTTRouterUnmatchedDropped` | A publish still matched **no** registered topic filter after the `unmatched_grace` window elapsed AND its topic is not covered by any subscription the session still wants: it was acked, dropped, and its exact topic UNSUBSCRIBEd (deduped, one warn per topic) to converge broker state (`router.dropOrphan`). Before the FIRST reconcile of a process lifetime nothing is ever counted here — the pre-plan backlog is retained as covered instead. | The signature of an orphan broker-side subscription — a route removed from config whose subscription survived on the resumed `clean_start=false` session. Expected as a one-shot right after a route removal; a continuously rising value means the broker keeps delivering for a subscription no configured route covers — investigate the removed route (a surviving **wildcard** subscription cannot be cleared by the concrete-topic UNSUBSCRIBE; configure the managed subscription store to converge it). |
| `MQTTRouterCoveredRetained` | A publish on a topic a still-desired subscription covers was RETAINED un-acked past the grace window because its receiver handler had not registered yet (`router.settlePending` / `router.retainCovered`). NOT loss. | A receiver registers later than `unmatched_grace` (or never). Investigate the slow route start; the retained backlog pins broker in-flight slots until the handler registers or the broker redelivers. |
| `MQTTRouterCoveredDropped` | A covered **QoS 0** publish was dropped past the grace window because the bounded pending buffer could not hold it (`router.retainCovered`). Covered QoS 1/2 is never counted here. | Best-effort loss on a live route during slow startup. Any non-zero value: speed up receiver registration or lengthen `unmatched_grace`. |
| `MQTTRouterOverflowDropped` | A QoS 1/2 publish was acked-and-dropped because the pending buffer's count cap (== `receive_maximum`) was hit with no evictable QoS 0 (`router.overflowAckDrop`). Unreachable with a spec-compliant broker. | The broker delivered more un-acked QoS 1/2 than the Receive Maximum it was granted — a protocol violation. MESSAGE LOST; investigate the broker. |
| `MQTTRouterStalePurged` | An old-connection publish was discarded: pending entries purged on reconnect (their acks died with the old connection) or old-socket ingress released during a recovery/managed-cleanup recycle window (`router.purgeStalePendingLocked`, `router.enqueueDispatch`/`router.dispatchCore` discard branches). | QoS 1/2 counted here is redelivered by the resumed session (not lost); QoS 0 is a best-effort loss across a disconnect. A steadily rising value means frequent reconnects/recycles while traffic is in flight. |
| `MQTTIngressPoisonDropped` | An inbound publish violated a local representational cap the broker cannot enforce (`max_payload_bytes`, metadata byte cap, User Property count cap) while fitting the advertised Maximum Packet Size; it was ACKED-and-DROPPED to prevent a permanent redelivery/terminal loop (`router.dropPoisonIngress`). | An authorized publisher is sending packets this bridge is configured to refuse; each count is a deliberate, acknowledged loss. Alert on any non-zero value and follow `docs/runbooks/mqtt-ingress-poison.md`. |
| `MQTTAckAfterReconnect` | A delivery settled after the underlying connection cycled; the protocol ack could not be delivered (paho `ErrPacketNotFound`) and the settlement was mapped to success (`router.ackWithReconnectMapping`). | Each count is a guaranteed broker redelivery — a burst after a reconnect storm predicts duplicates on routes without downstream dedup (`direct_hold`). Verify downstream idempotency. |
| `MQTTIngressHeaderDropped` | An inbound MQTT user property was dropped because its key/value is unsafe (invalid UTF-8, control characters) or over-long (`EnvelopeFromPublish`). No `session_id` tag. | A peer publishes spec-legal-but-rejected headers; routes filtering on those headers misroute. Find the publisher. |
| `MQTTNonStringHeaderDropped` | An egress header value was dropped because it is not a string and cannot become an MQTT user property (`PublishFromEnvelope`). No `session_id` tag. | Bridge-to-bridge metadata (idempotency key, tenant id) is being lost on egress — fix the producing route's header types. |
| `MQTTEventDropped` | The bounded session lifecycle-event channel was full and an event was evicted (`Session.pushEvent`). No `session_id` tag. | Under an event storm a `SessionConnected` may be evicted, deferring a reconcile one connect edge. Alert if non-zero in steady state. |
| `MQTTSessionRecoveryRecycle` | A durable QoS 1/2 `Delivery.Retry` (or an emit-error recovery request) started an actual settlement-recovery session recycle (`Session.recordRecoveryRecycleStart`). | The downstream is failing deliveries hard enough to force session recycles; every recycle redelivers ALL unsettled deliveries on the session (duplicates for innocent in-flight messages). |
| `MQTTUnsettled` / `MQTTOldestUnsettledAge` / `MQTTReceiveWindowUtilization` | Gauges snapshotting the un-acked QoS 1/2 window per health sweep (`Session.Health`). | Rising unsettled count/age means settlement (outbox persist / target accept) is stalling; utilization → 1.0 means ingress is about to block on the broker's Receive-Maximum window. An emit-error stranded delivery now triggers a bounded recovery recycle instead of wedging here permanently. |
| `MQTTSessionTakeover` | A server disconnect with reason code `0x8E` (*Session taken over*): another client connected with the same ClientID (`Session.noteSessionTakeover`). Reason code `0x8F` is *Topic Filter Invalid* — a different condition, NOT counted here. | Two instances share a `client_id` and keep kicking each other — give each replica a distinct ClientID (`client_id_suffix`) or use an exclusive session. One takeover during exclusive failover is normal; a climbing streak is a collision. |
| `MQTTQoSDowngraded` | The broker granted a subscription at a LOWER QoS than requested (`Session.reconcile` on SUBACK). | The delivery guarantee is weaker than the route assumes; readiness stays below Full. Investigate a broker QoS-cap policy. |

### AMQP 0-9-1 (`adapters/amqp/transport/amqp091`)

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `AMQP091DelayedRetryUnhonored` | A `Retry(after > 0)` was requested but AMQP 0-9-1 has no native delayed-redelivery primitive, so the nack requeues immediately and the requested backoff spacing is lost (`acl_delivery.go:130`). | Poison messages can hot-loop on a classic queue. Add an `x-delivery-limit` / dead-letter-exchange guard, or move retry spacing to the broker. |
| `AMQP091ReconnectRaceRetried` | A permanent-classified consume failure (403 `ACCESS_REFUSED` on an exclusive consumer, or 404 `NOT_FOUND` mid-topology-redeclare) was retried as a transient reconnect race (`receiver.go:132`). | Expected briefly after a reconnect/partition; climbing past the retry budget means a genuine misconfiguration, not a race. |

### AMQP 1.0 (`adapters/amqp/transport/amqp10`)

| Metric | When it increments | What a rising value means |
|--------|--------------------|---------------------------|
| `AMQP10DelayedRetryDeferred` | A `Retry` with a positive delay was handed back to the broker (`ModifyMessage` + `x-opt-delivery-time`) because AMQP 1.0 has no portable client-side delayed-redelivery primitive; the broker decides when to redeliver (`acl_delivery.go:203`). Increments once per deferred retry; a Warn fires once per link. | Broker-delegated retry scheduling, **not** a failure -- a honoring broker (Artemis) applies the requested spacing, a non-honoring one falls back to its own redelivery policy. Expected under retry load; correlate with delivery-attempt exhaustion rather than alerting on the counter alone. |

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
| `NOT_AUTHORIZED`, `FORBIDDEN`, `NOT_FOUND`, `INVALID_CONFIG`, `PROTOCOL_ERROR`, `QOS_NOT_SUPPORTED`, `NOT_SUPPORTED`, `VERSION_MISMATCH`, `ALREADY_EXISTS`, `STALE_FENCING_TOKEN`, `DUPLICATE_RECORD`, `PROCESSOR_PANIC`, `INTERNAL`, `INVALID_OUTBOX_RECORD`, `OUTBOX_NOT_CLAIMABLE`, `OUTBOX_NOT_IN_CLAIMED_STATE`, `OUTBOX_ALREADY_TERMINAL`, `NO_BINDING_MATCH`, `POISON_MESSAGE` | `permanent` | Yes (per route `FailureAction`) |
| `INVALID_PAYLOAD`, `PAYLOAD_TOO_LARGE`, `INVALID_TOPIC`, `SCHEMA_VIOLATION`, `MESSAGE_FILTERED` | `rejected` | No (silent drop) |
| `MESSAGE_EXPIRED` | `expired` | Per route `ExpiredAction` |

The authoritative source is [`domain/shared/errors.go`](../domain/shared/errors.go);
this page must stay in lockstep with that file.
