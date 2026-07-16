# Adversarial MQTT Production-Readiness Audit

**Baseline audited revision:** `4d8d76d7fafcd88f21954de03a5176e6406b5a86`
(`main`, commit date 2026-07-13)

**Remediation revision:** `prod-ready-remediation`; the merge commit is the
authoritative release-candidate revision.

**Status:** **CONDITIONAL NO-GO** for public production release.

**Scope:** MQTT Paho adapter, runtime settlement, outbox and DLQ interaction,
session supervision, cluster leases, live reconfiguration, health and metrics,
deployment artifacts, tests, and MQTT/cluster/operator documentation.

Every confirmed in-repository finding from the baseline audit is now resolved
or narrowed to an explicit fail-closed contract. Focused regression tests and
independent adversarial, security, resilience, and test-proof re-reviews approve
the remediation. Public module tags/proxy resolution, published image evidence,
and credentialed AWS failover measurements remain mandatory external gates.

## Remediation status

| Finding | Status | Current contract |
|---|---|---|
| B1 | **Resolved** | Preserve producer identity; otherwise assign one UUID per received publish before fan-out, so equal-valued events stay distinct. |
| B2 | **Resolved** | Durable managed-subscription history reconciles removed wildcard/shared filters before traffic. |
| B3 | **Resolved** | Durable MQTT identity changes fail before replacement; operators use the documented migration procedure. |
| H1 | **Narrowed** | Lease observation survives holder loss and failover budgets are validated; actual 30-60 second percentiles still require credentialed deployment evidence. |
| H2 | **Resolved** | Unsafe clustered shared-subscription replica identity is rejected. |
| H3 | **Resolved** | Full readiness requires every planned receiver handler. |
| H4 | **Resolved** | SUBACK QoS downgrade fails reconciliation and keeps readiness below Full. |
| H5 | **Resolved** | Persistent/exclusive unsettled delivery triggers bounded, serialized recovery; broker-backed proofs cover redelivery and progress. |
| H6 | **Narrowed** | Paho requires a dedicated ingress session; isolation uses separate broker connections rather than a speculative scheduler. |
| H7 | **Resolved** | MQTT ingress has validated byte budgets for payload, receive, dispatch, and route windows. |
| H8 | **Narrowed** | Per-process clustered live reload is rejected; coordinated whole-cohort replacement is the supported rollout. |
| H9 | **External** | Release DAG, manifest validation, isolated proxy/direct smoke, and image gates are implemented; public tags and proxy/registry evidence require release execution. |
| H10 | **Narrowed** | A DynamoDB-coordinated ECS HA profile is shipped; live credentialed failover proof remains external. |
| D1-D6 | **Resolved** | Delivery, subscription, failover, settlement, ADR provenance, and container-input documentation match the supported contract. |

No review can prove “zero bugs.” The accurate statement is that no confirmed
in-repository blocker from this audit remains open after the final specialist
re-reviews.

## Direct answers

| Question | Verdict | Reason |
|---|---|---|
| Is the implementation ready to become a release candidate? | **Yes** | The code, tests, and docs implement the narrowed contract and all final specialist reviews approved it. |
| Is public production release approved now? | **No** | Public module, immutable image, and credentialed AWS evidence remains external. |
| Do we have zero bugs? | **No such guarantee is possible** | No confirmed finding from this audit remains open. |
| Can it run in a cluster? | **Yes, conditionally** | Use the DynamoDB-coordinated HA profile and satisfy its documented broker, identity, store, and failover requirements. |
| Do messages go missing or get lost? | **At documented boundaries** | QoS 0 before durable acceptance, configured drop, broker/session retention expiry, and explicitly forced destructive operations remain loss boundaries; no unconditional no-loss claim is made. |
| Can one process be reconfigured live and resiliently? | **For non-destructive standalone changes** | Unsafe durable MQTT identity, persistent-subscription removal, and durable-store orphaning fail closed. |
| Can the cluster be reconfigured live and resiliently? | **Not per process** | Per-process clustered reload is rejected; use coordinated whole-cohort replacement. |
| Does cluster failover complete in 30-60 seconds? | **Not generally guaranteed** | Validation checks declared budgets, but deployment-specific percentiles must pass the credentialed AWS gate. |

## Production contract used for this review

The code is judged against these requirements:

- Two legitimate input events must never be merged because their bytes happen
  to match.
- QoS 1/2 input is acknowledged only after durable handoff or successful
  downstream acceptance.
- A reconfiguration must not strand broker queues, durable records, or
  ownership state.
- A cluster must maintain one ownership domain and one coherent routing policy
  during change.
- Readiness must describe the actual ability of every configured receiver to
  process traffic at its requested delivery guarantee.
- Every intentional or forced message loss must be distinguishable from
  success through a durable record, metric, or broker-side operating contract.
- A stated failover SLO includes detection, acquisition, broker connection,
  subscription reconciliation, handler registration, and orchestrator delay.

## Severity definitions

| Severity | Meaning |
|---|---|
| **BLOCKER** | Can silently lose/strand legitimate messages or make a core production claim false under supported configuration. Must be fixed before production approval. |
| **HIGH** | Can cause cluster outage, prolonged traffic stall, false readiness, weaker-than-configured delivery, or an unbounded resource failure. |
| **MEDIUM** | Material operability, API-safety, observability, documentation, or assurance defect. |
| **LOW** | Release-quality or diagnostic weakness that does not independently break delivery. |

## Baseline findings and evidence

The sections below preserve evidence from the baseline revision. Their
present-tense descriptions are historical; the remediation table above is the
authoritative current status.

### B1. Distinct byte-identical MQTT publishes collapse in `shared_outbox`

**Evidence**

- When neither `mqtt.message-id` nor correlation data exists,
  `EnvelopeFromPublish` derives the envelope ID:
  `adapters/mqtt/transport/paho/acl_headers.go:197-207`.
- The derivation hashes only topic and payload:
  `adapters/mqtt/transport/paho/acl_headers.go:365-387`.
- The code explicitly states that distinct events with equal topic and bytes
  collapse to one ID: `acl_headers.go:377-379`.
- DynamoDB outbox identity is partition plus envelope ID plus binding ID:
  `adapters/aws/store/dynamodboutbox/acl_store.go:178-187`,
  `acl_store.go:437-446`.
- SQLite enforces the same identity:
  `adapters/native/store/sqliteoutbox/acl_query.go:78-90`.
- The route treats an outbox duplicate as a successful prior persist and
  acknowledges the MQTT delivery:
  `runtime/route/dispatch.go:1064-1068`.

**Failure**

Publish these two legitimate events without an explicit message ID:

```text
PUBLISH topic=sensors/a payload={"state":"on"}
PUBLISH topic=sensors/a payload={"state":"on"}
```

MQTT distinguishes them as two publishes. The adapter does not. Both become the
same `Envelope.ID`; the first creates the outbox record, the second receives
`ErrDuplicateRecord`, and the runtime acknowledges it as already durable. Only
one reaches the destination.

This is not a theoretical hash collision. It is deterministic identity aliasing
for a normal event pattern such as heartbeats, repeated readings, button
presses, state snapshots, or periodic empty payloads.

**Impact**

- Silent acknowledged loss on `shared_outbox`.
- No DLQ entry.
- No loss metric.
- No way to recover the second event because its source delivery was
  acknowledged.
- Documentation transfers the burden to every non-GoBridge MQTT producer even
  though MQTT itself does not require an application message ID.

**Required fix**

Do not use topic plus payload as event identity. The fallback must be unique per
received publish while retaining a separate redelivery key when the transport
can prove redelivery identity. Acceptable designs include:

1. Require and validate a producer ID for `shared_outbox`, failing closed before
   subscription activation when the contract cannot be met.
2. Generate a unique envelope ID per publish and carry a separate, explicitly
   scoped dedup key only when a producer supplies one.
3. Persist a receive-side packet/session identity ledger that can distinguish a
   redelivery from a new equal-valued publish. MQTT packet ID alone is not
   globally sufficient and must be scoped to the broker session/connection
   semantics.

**Release proof**

An integration test must publish two byte-identical QoS 1 messages without
identity properties through a real broker and `shared_outbox`, then prove both
are persisted, sent, and acknowledged. A separate reconnect test must prove one
real broker redelivery is deduplicated without collapsing the two original
events.

### B2. Runtime replacement cannot remove prior persistent wildcard/shared subscriptions

**Evidence**

- Reconciliation can remove subscriptions only from the current `Session`'s
  `appliedPlan`: `adapters/mqtt/transport/paho/session_reconcile.go:34-55`.
- A Supervisor swap constructs a new runtime and therefore a new MQTT `Session`;
  no `appliedPlan` history crosses the replacement:
  `bridge/supervisor.go:958-1026`.
- A persistent/exclusive broker session is keyed by stable `client_id` and
  resumes its subscriptions: `adapters/mqtt/transport/paho/acl_session.go:334-354`.
- MQTT provides no subscription-list API. Orphan cleanup sees a concrete
  delivered topic and can only unsubscribe that exact topic, not the wildcard
  or `$share` filter that created it:
  `adapters/mqtt/transport/paho/session_reconcile.go:155-169`.
- After grace, a true orphan is acknowledged and dropped:
  `adapters/mqtt/transport/paho/acl_router.go:647-687`.

**Failure**

1. Old runtime subscribes to `$share/telemetry/old/#` with
   `clean_start=false`.
2. Live config removes that receiver but retains the same `client_id`.
3. Supervisor stops the old runtime and constructs a new `Session`.
4. Broker resumes the old subscription for the stable client ID.
5. New `Session.appliedPlan` is empty, so the new reconcile has no record of the
   removed filter and cannot issue `UNSUBSCRIBE $share/telemetry/old/#`.
6. The stale shared-group member receives a concrete topic such as
   `old/device-7`.
7. The new router has no handler, waits through grace, acknowledges and drops
   the message, then attempts to unsubscribe `old/device-7`. That does not
   remove `$share/telemetry/old/#`.

**Impact**

- A stale shared subscription can steal the broker's selected group delivery
  from a healthy member and then acknowledge/drop it: real cross-cluster message
  loss.
- An ordinary wildcard orphan continues consuming broker bandwidth and
  generating acknowledged drops until the broker session expires, is manually
  cleaned, or a clean start destroys it.
- Process restart does not fix the problem when the same persistent client ID
  resumes the same broker session.
- The drop metric reports the symptom, but it cannot reconstruct the lost
  shared-group messages.

**Required fix**

Persistent subscription ownership needs durable desired/applied history keyed
by broker identity and client ID. Before the replacement session resumes normal
traffic, it must know the exact previously managed filters and reconcile
`old filters - new filters`. If that history is unavailable, a filter-removing
reload must fail closed and require an explicit migration operation:

1. Drain and stop the old runtime.
2. Unsubscribe exact old filters while its applied history still exists.
3. Confirm UNSUBACK.
4. Start the new runtime.

The evidence-driven concrete-topic fallback can remain a last-resort alarm, but
it cannot be the primary cleanup mechanism for wildcard/shared subscriptions.

**Release proof**

Add broker-backed tests for runtime replacement with:

- removed `sensors/#`;
- removed `$share/group/sensors/#`;
- a shared-group peer proving no message is stolen/dropped after replacement;
- failed UNSUBACK followed by retry/recovery;
- process restart with the same `client_id`.

### B3. Live reload permits destructive MQTT identity migrations

**Evidence**

- MQTT broker/session identity includes broker URLs, client ID, session mode,
  clean start, and session expiry:
  `adapters/mqtt/transport/paho/config.go:55-90`,
  `adapters/mqtt/transport/paho/acl_session.go:334-354`.
- The Supervisor explicitly blocks durable-store identity changes and
  lease-bearing `session_id` changes:
  `bridge/supervisor.go:684-725`, `bridge/supervisor.go:1170-1270`.
- There is no equivalent preflight for MQTT `client_id`, broker URLs,
  persistent/ephemeral mode, `clean_start`, or session expiry.
- MQTT always triggers serialized prepare/commit because the factory advertises
  `CapExclusiveIdentity`:
  `adapters/mqtt/transport/paho/factory.go:33-49`,
  `bridge/supervisor.go:1101-1131`.
- Prepare/commit stops the old runtime before creating and starting replacement
  sessions: `bridge/supervisor.go:958-1026`.

**Failure modes**

- **Change `client_id`:** queued QoS 1/2 messages remain under the old broker
  session. The new client cannot access them.
- **Change broker URL:** queued messages and subscriptions remain on the old
  broker.
- **Change Persistent/Exclusive to Ephemeral or enable clean start:** reconnect
  can discard the retained broker session and its backlog.
- **Reduce session retention:** the migration can expire old broker-side state
  before it is drained.
- **Roll the change through a cluster:** old and new identities coexist while
  different replicas apply independently, splitting consumption and queue
  ownership.

**Impact**

The current store/session-ID reload guards create an impression that destructive
identity changes are comprehensively protected, but MQTT's own durable identity
is outside those guards. A syntactically valid live reload can strand messages
without an outbox or DLQ record because the bridge never received them.

**Required fix**

Define a non-secret MQTT `StorageIdentity`/migration identity containing at
least broker set, effective client ID, session mode, clean-start behavior, and
session-expiry policy. Refuse live changes that can strand broker state unless a
dedicated migration proves the old session drained and subscriptions removed.
Clustered changes require a versioned external cutover, not a per-process escape
hatch.

**Release proof**

Tests must seed broker-side offline QoS 1 backlog, attempt each identity change,
and prove the reload is refused with the old runtime still serving. Separate
migration tests must prove an explicit drain/delete/cutover procedure preserves
every message.

## High-severity findings

### H1. The 30-60 second failover target is conditional, not guaranteed

**Evidence**

- Clustered sessions default to `HAConfig`: 45-second TTL, 10-second renewal,
  1-second jitter, 3-second call timeout, and 5-second step-down grace:
  `runtime/session/config.go:108-170`,
  `bridge/convert.go:103-143`.
- Standby acquisition polls at up to 5 seconds:
  `runtime/session/config.go:218-235`.
- DynamoDB takeover requires a locally observed, unchanged liveness tuple for a
  full TTL. A standby that starts observing after death can need about two TTLs:
  `adapters/aws/store/dynamodblease/acl_store.go:171-217`,
  `acl_store.go:324-369`.
- The scenario documentation calculates the default paths as:
  - warm: `~57.5s + broker connect`;
  - cold: `~102.5s + broker connect`.
  See `docs/scenarios/08-clustered-exclusive-sessions.md:512-535`.

**Why this fails the requested SLO**

- The warm estimate leaves only about 2.5 seconds for TCP/TLS/MQTT connect,
  CONNACK, SUBSCRIBE/SUBACK, route startup, and `Full` readiness before crossing
  60 seconds.
- The cold/replacement path exceeds 60 seconds before connecting to MQTT.
- Orchestrator scheduling, image pull, process startup, DNS, credential
  retrieval, DynamoDB latency, and broker latency are outside the lease figure.
- The configured reconnect attempt timeout is 30 seconds by default, and broker
  reconnect backoff can grow to 2 minutes.
- The implementation emits a warning only when explicitly configured
  `lease_ttl > 60s`; it does not validate the end-to-end failover budget:
  `bridge/builder_complete.go:544-586`.

**Required fix**

- Define the SLO endpoint as failure detection to `ServiceLevelFull`.
- Persist standby observations, or replace the in-memory observation algorithm,
  so a replacement does not pay a second TTL.
- Add a production preflight that budgets TTL, acquire poll, broker connect,
  reconcile timeout, and startup allowance.
- Make warm standby count/health an enforced deployment invariant for any
  advertised <=60-second profile.
- Alert on measured failure-to-Full duration, not only lease-transfer count.

### H2. Cluster validation allows a known shared-subscription self-DOS

**Evidence**

- Cluster validation requires either `$share/...` or an Exclusive session but
  does not require an effective per-replica client-ID strategy:
  `validate/blueprint_graph.go:299-340`.
- The MQTT factory offers `client_id_suffix` but leaves it optional:
  `adapters/mqtt/transport/paho/factory.go:67-90`.
- Detection happens after deployment. A non-Ephemeral `$share` session logs a
  warning during reconcile:
  `adapters/mqtt/transport/paho/session_reconcile.go:56-79`.
- Duplicate live client IDs cause repeated Session-Taken-Over disconnects.
  Reconnect damping reduces broker load but does not restore service.

**Impact**

A normal Kubernetes Deployment or ECS service shares one config among replicas.
Without `client_id_suffix`, all replicas connect with the same ID, repeatedly
evict each other, and do not form a shared-consumer group. The cluster can pass
configuration validation and deploy directly into an outage.

**Required fix**

For clustered, non-Exclusive MQTT receivers using `$share`, require one of:

- `client_id_suffix: hostname|nonce`;
- a deployment-provided per-instance identity expression that validation can
  verify;
- an explicit risk override with a startup error-level event and failed
  readiness until uniqueness is proven.

### H3. `ServiceLevelFull` does not prove every receiver handler is registered

**Evidence**

`Session.Health` reports Full when:

```text
active subscription count == desired subscription count
AND total handler count > 0
AND pending buffer count == 0
```

See `adapters/mqtt/transport/paho/session_health.go:34-71`.

There is no relationship between a desired subscription/receiver and its
registered handler. With two receivers, one registered handler is enough for
Full until traffic for the missing handler creates a pending entry.

**Impact**

- `/ready?level=full` can return 200 while a configured receiver is absent.
- Deployments can receive production traffic before every route is capable of
  handling it.
- QoS 0 messages for the absent handler can overflow and be lost.
- The implementation contradicts the documented meaning "every route handler is
  registered": `docs/deployment-guide.md:497-515`.

**Required fix**

Track expected handler IDs or subscription-to-handler coverage in the session
plan. Full must require every expected receiver registration, every requested
subscription active at its required QoS, and no retained pending backlog.

### H4. Broker QoS downgrade is accepted as healthy

**Evidence**

After SUBACK, the reconcile path stores the requested QoS in `activeSubs` even
when the broker granted a lower QoS. It emits a warning and
`MQTTQoSDowngraded`, but reconciliation succeeds:
`adapters/mqtt/transport/paho/session_reconcile.go:459-495`.

`Session.Health` then sees the requested subscription count as active and can
report Full: `session_health.go:59-71`.

**Impact**

A route configured for QoS 1 or 2 can operate at QoS 0 while reporting full
readiness. Disconnect/offline delivery guarantees are weaker than configuration,
and messages can be lost during broker or bridge outages.

**Required fix**

Make a granted QoS below requested QoS one of:

- a reconcile failure that keeps readiness below subscribed/full; or
- an explicitly configured `allow_qos_downgrade` policy, with health exposing
  the granted QoS and the degraded guarantee.

A warning is not an adequate substitute for enforcing the declared delivery
contract.

### H5. MQTT transient failure plus DLQ outage can wedge ingress on a live connection

**Evidence**

- MQTT `Delivery.Retry` always returns `ErrNotSupported`:
  `adapters/mqtt/transport/paho/delivery.go:119-125`.
- A transient processing/resolve/persist failure falls back to the DLQ. If that
  write fails, the route returns without acknowledging the source:
  `runtime/route/dispatch.go:658-692`.
- Delivery-processing errors are logged and discarded by the route goroutine;
  they do not fail the receiver or force a connection cycle:
  `runtime/route/runner.go:572-589`.
- Paho manual acknowledgements flush in receive order. An old unacknowledged
  publish blocks later acknowledgements, and the broker eventually exhausts the
  configured Receive Maximum:
  `adapters/mqtt/transport/paho/delivery.go:14-27`,
  `acl_router.go:1002-1028`.

**Failure**

1. Processing fails transiently.
2. MQTT cannot schedule a per-message retry.
3. DLQ persistence is unavailable.
4. The message remains unacknowledged, which is correct for durability.
5. The MQTT connection remains up and the packet is not redelivered on that
   connection.
6. Later packets can be processed, but their protocol acks cannot pass the old
   unsettled packet.
7. The Receive Maximum fills and ingress stalls until a reconnect or process
   restart happens for some other reason.

**Impact**

The bridge can remain live, connected, and potentially ready while a route
ceases making protocol progress. This is an outage-recovery gap, not message
loss by itself; the message should redeliver after a persistent-session
reconnect. Recovery time is unbounded because the code does not initiate that
reconnect.

**Required fix**

On an MQTT delivery that cannot be settled or durably parked:

- mark the session/route degraded immediately;
- close/reload the MQTT connection after bounded drain so the broker redelivers;
- preserve the persistent session;
- rate-limit reconnects to avoid a DLQ-outage reconnect storm;
- expose oldest-unsettled age and unacknowledged-window utilization.

### H6. One blocked publish stalls every route sharing the session

**Evidence**

- Each MQTT session has one serialized dispatch queue and one worker:
  `adapters/mqtt/transport/paho/acl_router.go:254-258`,
  `acl_router.go:871-910`.
- Dispatch starts matching handlers concurrently for one publish, then blocks
  until every handler returns before processing the next publish:
  `acl_router.go:1118-1204`.

**Impact**

- A slow processor, destination, DLQ call, or handler on one topic blocks
  unrelated topics and routes on the same MQTT connection.
- A QoS 0 burst fills the 1024-item queue and is dropped.
- QoS 1/2 eventually blocks the Paho callback when the queue is full, widening
  keepalive/reconnect blast radius.
- Sharing one connection, presented as an efficiency feature, creates
  session-wide head-of-line failure coupling.

**Required fix**

Partition dispatch by receiver/route or ordering key, with:

- independent bounded queues;
- fair scheduling;
- per-route concurrency and backpressure;
- protocol-ack aggregation across fan-out;
- explicit ordering semantics.

If global ordering is intentional, document and validate that every handler
timeout is below the session liveness budget and expose queue age/depth.

### H7. Default inbound memory is not bounded by message size

**Evidence**

- `receive_maximum` defaults effectively to 1024:
  `adapters/mqtt/transport/paho/config.go:417-426`.
- The dispatch queue is another 1024 publishes:
  `adapters/mqtt/transport/paho/acl_router.go:254-258`.
- `max_payload_bytes` defaults to zero, which advertises no packet-size limit:
  `adapters/mqtt/transport/paho/config.go:149-163`.
- The documentation estimates roughly
  `(1024 + receive_maximum) * payload`:
  `docs/transports/mqtt.md:381-395`.

**Impact**

The count is bounded but the bytes are controlled only by broker policy. A
broker configured for large MQTT packets can cause an out-of-memory process
failure with a comparatively small number of queued messages. Container memory
limits turn this into deterministic OOMKill and redelivery churn.

**Required fix**

- Set a conservative non-zero production default for `max_payload_bytes`.
- Make the deployment profile derive `receive_maximum` from container memory,
  max payload, and concurrency.
- Reject configurations whose calculated upper bound exceeds a configured
  memory budget.
- Add a load test that holds the downstream path and fills the entire receive
  and dispatch window with maximum-size messages under the container limit.

### H8. Cluster reconfiguration is eventually consistent and can diverge indefinitely

**Evidence**

The documentation accurately states that each process loads and swaps
independently, with no cluster version barrier and no coordinated rollback:
`docs/scenarios/10-dynamic-reconfiguration.md:211-237`.

Local guards cover store identity and exclusive `session_id`, but not all MQTT
identity fields (B3), and they do not make routing/policy changes atomic:
`bridge/supervisor.go:684-765`.

**Impact**

- The same event class can be processed under different filters, transforms,
  destinations, retry policies, or drop policies at the same time.
- One failed instance can remain on the old version indefinitely.
- A rollback can produce another mixed-version interval.
- The local `WithAllowDestructiveReload` escape hatch can bypass guards without
  cluster consensus.
- There is no "all replicas accepted version N" commit point.

**Required fix**

Implement or integrate a cluster rollout protocol:

1. Stage and validate a version on every member.
2. Reject changes to cluster identities without an explicit migration plan.
3. Quiesce affected partitions.
4. Commit through a version barrier.
5. Verify every member reaches the version and required readiness.
6. Roll back the whole cohort on timeout.

Until then, describe cluster reconfiguration as externally orchestrated
drain/stop/deploy/start, not resilient live cluster reconfiguration.

## Message delivery and loss matrix

| Situation | Result | Recorded/reported | Recovery |
|---|---|---|---|
| QoS 0 while connected, dispatch queue full | Message dropped | `MQTTRouterDropped` plus warning | None |
| QoS 0 while handler absent and pending buffer full | Message dropped | `MQTTRouterDropped` or `MQTTRouterCoveredDropped` | None |
| QoS 0 during any disconnect | Message never reaches bridge | Usually broker-side only; bridge has no per-message evidence | None |
| Ephemeral session during disconnect/reload | Offline messages not retained; in-flight unacked input does not survive clean start | Connection/readiness gap is visible, missing messages are not | None |
| Persistent/Exclusive QoS 1/2, outage shorter than broker queue/session limits | Broker queues/redelivers | Connection and reconnect metrics/logs; bridge sees messages only after recovery | Automatic if same broker/client identity resumes |
| Outage exceeds `session_expiry_interval` (24h default) | Broker may delete session and queued messages | Broker-side; bridge cannot enumerate what was deleted | None |
| Broker offline queue quota or message expiry reached | Broker may discard messages before delivery | Broker-side; bridge has no per-message record | None |
| Broker grants lower QoS than requested | Disconnect-gap loss can replace expected offline retention | `MQTTQoSDowngraded` and warning, but readiness can remain Full | Operator intervention |
| Legitimate equal topic/payload publishes without producer ID on `shared_outbox` | Later events silently collapsed and acknowledged | No loss/DLQ signal; handled as ordinary outbox duplicate | None |
| Removed exact subscription in same live `Session` | Reconcile issues UNSUBSCRIBE | Reconcile metrics/logs | Automatic if broker operation succeeds |
| Removed wildcard/shared subscription across runtime replacement | Old filter can survive; unmatched messages acknowledged/dropped | `MQTTRouterUnmatchedDropped`; exact concrete-topic unsubscribe does not remove old filter | Manual broker cleanup, clean start, expiry, or fixed migration |
| Transient processing failure and working DLQ | Delivery stored in DLQ then acknowledged | DLQ entry and `DLQEntries` | Operator redrive |
| Transient processing failure and no DLQ configured | Delivery intentionally dropped and acknowledged because retry is unsupported | `MessagesDropped{reason=retry_unsupported}` and warning | None |
| Transient processing failure and DLQ write outage | Delivery stays unacknowledged; ordered ack stream can stall until reconnect | `DLQWriteFailures` and processing warning; no forced recovery | Incidental reconnect/process restart |
| Permanent failure with `on_permanent_failure: drop` | Delivery intentionally dropped and acknowledged | `MessagesDropped` and warning | None |
| QoS 1/2 pending overflow caused by broker violating Receive Maximum | Adapter acknowledges and drops to keep ack stream moving | `MQTTRouterOverflowDropped` and explicit loss log | None |
| Outbound QoS 1/2 process crash before PUBACK/PUBCOMP | Paho's in-memory outbound packet is lost; source/outbox should resend | Duplicate-risk/send metrics, not protocol-state recovery | `direct_hold` needs a redeliverable source; `shared_outbox` needs successful prior persist |
| Send timeout after broker accepted but before response observed | Outcome ambiguous; retry can duplicate | Publish failure/latency; no end-to-end broker transaction ID | At-least-once retry and downstream idempotency |

## Timeout and recovery boundaries

| Control | Default | What it bounds | What it does not guarantee |
|---|---:|---|---|
| Initial connect timeout | 30s | First `Start` wait | Cluster failure-to-Full |
| Reconnect attempt timeout | 30s | One dial/TLS/CONNACK attempt | Time until next attempt |
| Reconnect backoff | 10s to 2m, jittered | Retry pressure | Recovery inside 60s after a prolonged outage |
| Reconcile timeout | 30s per SUBSCRIBE/UNSUBSCRIBE | One broker operation | Total plan reconciliation with many operations |
| Sender timeout | 30s via config | Publish/PUBACK/PUBCOMP wait | Whether a timed-out publish reached the broker |
| Startup unmatched grace | 30s per connect | Handler-registration race | Removal of wildcard/shared orphan filters |
| Persistent session expiry | 24h | Broker-side session retention request | Broker queue capacity, broker persistence, or retention beyond 24h |
| HA lease TTL | 45s | Ownership liveness window | Cold failover, broker connection, subscription readiness, or orchestrator startup |

### H9. The MQTT Go module is not externally consumable through normal Go module resolution

**Evidence**

- The MQTT module requires repository siblings at `v0.0.0` and replaces them
  with local relative paths:
  `adapters/mqtt/transport/paho/go.mod:5-8,24-26`.
- Only core tags `v0.1.0` and `v0.2.0` exist at the audited revision; there are
  no path-prefixed MQTT/testutil module tags.
- The README explicitly states that clean external `go get` currently fails and
  consumers must clone the repository and work inside its `go.work`:
  `README.md:43-55`.

**Impact**

The library cannot yet be consumed by a conventional independent Go service
using `go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho`.
That blocks the expected "one standard process embeds the library" path unless
the consumer adopts this repository's workspace or copies/builds a composition
root from the monorepo.

**Required fix**

Complete the first multi-module release:

- create path-prefixed semver tags in dependency order;
- replace internal `v0.0.0` requirements with released versions;
- remove local `replace` directives from published module manifests;
- prove installation and build from an empty external module in CI.

### H10. The shipped AWS `GoBridgeCluster` is scale-out, not HA failover

**Evidence**

- The shipped container is the AWS file-based composition root:
  `README.md:18-29`, `Dockerfile:17-19`.
- The reference `GoBridgeCluster` forces `filesystem_replicated`, rejects
  `shared_outbox` and route-session leases, and explicitly provides no
  coordinated failover SLO:
  `deployment/aws-filebased-config/README.md:202-235`.
- DynamoDB-backed coordinated HA is described as future work in that deployment
  profile.

**Impact**

The runtime primitives can form a coordinated cluster when a custom deployment
wires distributed lease/outbox stores, but the named and shipped cluster
construct does not do that. An operator selecting `GoBridgeCluster` does not get
active/standby takeover, durable shared outbox coordination, or a 30-60 second
failover target.

**Required fix**

Ship a separate, unambiguously named HA deployment profile that provisions:

- DynamoDB lease and outbox tables with alarms/capacity guidance;
- stable MQTT identity and warm standbys;
- required IAM;
- failure-to-Full SLO metrics;
- integration tests that kill the current leaseholder.

Keep replicated independent scale-out as a separate topology.

## Documentation defects

### D1. Absolute "neither wired delivery mode loses a message" claims are false

`docs/transports/mqtt.md:264-290` and
`docs/adr/0009-durable-outbound-mqtt-session-state.md:39-62` say both current
delivery modes are loss-safe across process failure.

Counterexamples in the same product:

- B1 acknowledges a distinct equal-valued event as an outbox duplicate.
- An Ephemeral MQTT source cannot redeliver its unacknowledged packet across a
  clean-start process restart.
- QoS 0 has no source redelivery.
- A source broker can expire/drop its offline queue before the bridge sees it.
- An explicit configured failure/drop policy acknowledges loss by design.

The correct contract is conditional:

- `shared_outbox` protects records that acquired a unique durable identity and
  were successfully persisted;
- `direct_hold` protects only when the source transport/session will redeliver;
- neither mode proves that a broker-retained message existed before the bridge
  received it;
- ambiguous send outcomes can duplicate.

Replace absolute claims with a guarantee matrix keyed by source QoS/session,
delivery mode, producer identity, store durability, and outage duration.

### D2. The persistent-subscription ADR gives a false restart recovery

`docs/adr/0003-mqtt-persistent-session-hygiene.md:71-81` says a wildcard orphan
and failed exact-topic cleanup survive until process restart.

With the same persistent `client_id`, process restart resumes the broker session
and both subscriptions survive. A fresh in-memory `Session` also loses the
applied-plan history needed to remove them. The real boundaries are successful
UNSUBSCRIBE, clean start/session deletion, session expiry, changed client ID, or
broker administration.

The ADR also says empty-plan reconcile is a no-op
(`0003:57-61`), while current code/tests intentionally unsubscribe all managed
subscriptions:
`adapters/mqtt/transport/paho/bug_c7_reconnect_owner_test.go:303-333`.

### D3. Failover runbook overstates both timing and duplicate prevention

- `docs/runbooks/node-down-failover.md:37-54` presents HA takeover as roughly
  45-60 seconds without the cold-observer second-TTL path documented in Scenario
  8.
- `node-down-failover.md:64-67` says outbox fencing prevents duplicate
  destination sends, then immediately says destination duplicates are possible.

Fencing prevents an old owner from continuing to mutate the fenced record. It
cannot undo a send that reached the destination before the sender lost the
response or before the old owner died. The runbook must say "prevents stale
owners from committing/continuing outbox work" and require downstream
idempotency.

### D4. Identity and settlement documentation contradicts current production code

- `docs/timing-audit.md:101-108` says MQTT uses a random fallback envelope ID;
  production uses deterministic topic+payload hashing.
- `adapters/mqtt/transport/paho/analysis_receiver_test.go:107-175` says MQTT Ack
  is a no-op because Paho acknowledges before the handler.

Production enables manual acknowledgment in
`adapters/mqtt/transport/paho/acl_session.go:376-397`; the production delivery
Ack calls the Paho ack function. The test drives the legacy router seam, whose
synthetic delivery lacks that callback. Its comments and name are not a valid
production conformance statement.

### D5. Accepted ADR chronology is not trustworthy at the audited revision

The audited commit is dated 2026-07-13, but accepted ADRs 0009, 0010, and 0011
and the ADR 0003 addendum are dated 2026-08-14. Future-dated accepted decisions
make it impossible to determine whether the decision predates, documents, or
postdates the implementation under audit.

Correct the dates or record explicit planned/effective dates and link each ADR
to the implementing commit/release.

### D6. Container build documentation admits unreproducible base images

`Dockerfile:22-27,49-56` correctly says release images should pin base images by
digest, but the actual `FROM` lines use mutable tags. A rebuild of the same
source can therefore produce different binaries/root filesystems and pull
unreviewed upstream changes.

Pin both build and runtime images by digest in source and automate digest
refresh through reviewed pull requests.

## Test-assurance gaps

The test volume is substantial, but several test names and scenarios do not
prove the production properties the release decision needs.

| Claimed area | What the test actually proves | Missing production proof |
|---|---|---|
| Reconnect preserves subscriptions | `integration_bugfix_test.go:27-145` closes a clean-start Ephemeral session, creates another with a **different client ID**, reapplies the plan, and publishes a new message | Same client ID, `clean_start=false`, broker-side queued QoS 1/2 message, real disconnect/reconnect, Session Present, no explicit re-create shortcut |
| Reconnect timeout | `integration_high_fixes_test.go:159-228` starts once and checks health | Force a reconnect, stall/reject reconcile beyond the configured timeout, and prove degraded health plus bounded retry |
| Three-instance cluster failover | `tests/longrunning/uc3_cluster_failover_test.go:50-187` uses SQS ingress, distinct MQTT client IDs, and a 2-second lease profile (`longrunning_test.go:359-367`) | Stable MQTT source identity, persistent broker queue, current holder killed, production 45-second profile, measured failure-to-Full |
| Correct leader failure | UC3 stops A first and B second using start-order heuristic (`uc3:154-165`) | Query and kill the actual leaseholder; fail the test if the intended owner did not die |
| Identity dedup | Unit tests deliberately assert equal topic+payload yields the same ID; outbox tests assert same envelope ID is idempotent | Publish two legitimate equal-valued events with no producer ID through MQTT to `shared_outbox` and assert two outputs |
| Persistent orphan cleanup | Unit tests cover same-Session plan removal and exact-topic orphan behavior | Create broker-side wildcard and `$share` filters, replace the whole runtime with the same client ID, then prove the old filters are gone |
| QoS downgrade | `bug_c4_prodready_test.go:112-170` asserts the downgrade is warned once and the requested QoS is recorded | Assert required QoS affects readiness or causes reconcile failure |
| Handler readiness | Tests cover zero handlers and pending backlog | Two planned receivers with only one handler registered must not report Full |
| Retry fallback | Tests cover working DLQ fallback | MQTT Retry unsupported + DLQ store outage must force bounded degraded/reconnect recovery rather than an indefinite ordered-ack stall |
| Broker durability | Several long-running tests correctly opt into Mosquitto persistence, but the default test broker disables it | Make persistence/restart tests mandatory release gates, not opt-in long-running coverage only |
| `shared_outbox` broker restart (UC42) | The test failed repeatedly at its post-restart probe, but debug/trace runs showed the bridge reconnecting and completing all 3,001 outbox sends, including the probe. The collector callback appends each delivery and returns without calling `Ack` (`longrunning_helpers_test.go:608-620`). Production Paho uses manual acknowledgement (`delivery.go:1-20`), and this collector uses the default Receive Maximum of 1,024 (`config.go:417-426`). It therefore exhausts the broker's in-flight window at exactly the observed boundary and cannot receive the probe. A controlled run changing only the callback to `return del.Ack(ctx)` passed in 30.37 seconds with 3,001 unique messages and no DLQ entries. | Make every MQTT collector settle each delivery and fail its goroutine error visibly. Assert bridge outbox completion independently from the test oracle. UC42's current checked-in failure is a fixture defect, not evidence of a `shared_outbox` recovery defect. |
| Cluster reload | Unit/scenario coverage proves local prepare/commit semantics | Concurrent multi-member version staging, one-member rejection, rollback, and no mixed-version processing |

Add chaos tests for:

- broker outage before and after CONNACK;
- DNS/TLS/credential failures;
- broker restart with persisted session and queued messages;
- DLQ, outbox, and lease-store outages independently and together;
- duplicate client-ID takeover storms;
- SIGKILL at every source-ack/outbox/send boundary;
- maximum-size payloads at full Receive Maximum;
- rolling configuration updates with delayed and failed members.

Every loss test must compare producer IDs end to end and report missing,
duplicate, reordered, DLQ, and intentionally dropped sets separately.

### Verification snapshot

The following gates and focused checks were run against the audited revision:

| Check | Result | Qualification |
|---|---|---|
| `make lint` | Passed | Full repository static gate. |
| `make test` | Passed | Go reported cached results for most packages; this is not an uncached release proof. |
| Fresh MQTT race suite | Passed | `cd adapters/mqtt/transport/paho && go test -race -count=1 ./...` |
| Fresh runtime race suites | Passed | `go test -race -count=1 ./runtime/... ./bridge ./validate` |
| UC3 cluster failover | Passed | Uses the non-production 2-second lease profile and does not kill a verified MQTT leaseholder, so H1's gap remains. |
| UC43 `direct_hold` broker restart | Passed | Broker-backed focused run. |
| UC51 persistent-session recovery | Passed | Broker-backed focused run. |
| UC42 `shared_outbox` broker restart | Checked-in test failed repeatedly; controlled correction passed | The bridge drained all 3,001 records after reconnect. Its collector never acknowledges manual-ack deliveries and stalls at Receive Maximum 1,024. Changing only the callback to acknowledge each delivery made the test pass in 30.37 seconds with 3,001 unique messages and no DLQ entries. |

The long-running set is therefore not wholly green. UC42 must be repaired before
it can serve as a release gate, then rerun uncached. The failure does not reduce
the severity of the independently confirmed production findings in this report.

## Docker, Kubernetes, and ECS consumption

### What is usable

- A real production binary and published-image contract exist:
  `gobridge-filebased`, not the demo `cmd/gobridge`.
- The image is static, distroless, non-root, contains a self-healthcheck, and
  documents a 60-second stop timeout: `Dockerfile:3-15,58-69`.
- The AWS profile supplies ECS Fargate/EFS/SSM/CloudWatch CDK constructs and
  registers MQTT, SQS, HTTP, memory, SQLite, and DynamoDB-backed services.
- The deployment guide documents restart policy, liveness/readiness, graceful
  termination, ConfigMap update behavior, and a Kubernetes manifest template.
- Terminal runtime states exit non-zero so a real orchestrator can rebuild
  single-use transports.

### What is not turnkey

- The stock image is AWS-bound. It requires an SSM admin-key parameter and
  unconditionally constructs a DynamoDB client:
  `docs/deployment-guide.md:8-19`,
  `deployment/aws-filebased-config/lib/bootstrap/app.go:258-271`.
- A plain Docker/Kubernetes consumer must own a custom Go composition root,
  image, adapter selection, credential backend, telemetry wiring, manifests,
  updates, and release process:
  `docs/deployment-guide.md:524-632`.
- There is no checked-in Helm chart or deployable Kubernetes manifest; the docs
  provide templates.
- The shipped AWS cluster facade is independent scale-out, not coordinated HA
  (H10).
- External library installation is not released correctly (H9).
- The stock Dockerfile is not bit-reproducible because its base images are not
  digest-pinned (D6).

**Verdict:** ECS single-instance/independent-scale deployment has a credible
reference path. Portable Docker/Kubernetes embedding is documented but requires
material product engineering by the consumer. Coordinated MQTT HA needs a
custom DynamoDB-backed topology and does not have a turnkey reference
deployment.

## Positive controls worth preserving

The baseline NO-GO was not a claim that the transport lacked
production-quality work. These controls remain useful:

- production Paho manual acknowledgement is enabled;
- exact subscription deltas are reconciled and SUBACK failures propagate;
- retained replay is suppressed on persistent reconnects;
- connection attempts and broker operations have explicit timeouts;
- reconnect backoff is jittered and capped;
- password/TLS rotation rebuilds the connection manager;
- QoS 0 and orphan drops have dedicated metrics;
- source settlement waits for all matching handlers in fan-out;
- outbox records are version-fenced;
- exclusive sessions step down on lease loss and terminal states exit for
  orchestrator recovery;
- liveness, deep health, and readiness levels exist;
- the production image runs non-root and has graceful-stop guidance.

These controls reduce risk. They still do not make absolute no-loss or
universal 60-second claims true.

## Baseline remediation order

### P0: release blockers

1. Replace topic+payload identity with a contract that never collapses distinct
   events. Require producer IDs for dedup-sensitive routes or persist broker
   packet/session identity that distinguishes publishes.
2. Persist managed subscription intent outside each `Session`, and reconcile
   broker state during replacement/migration. Do not use concrete delivered
   topics to clean wildcard/shared filters.
3. Reject destructive MQTT identity reloads by default. Add an explicit,
   observable migration workflow.
4. Make required QoS and complete handler coverage part of Full readiness.
5. Force bounded connection recycle when an MQTT delivery cannot be settled or
   durably parked.

### P1: HA and isolation

1. Redesign lease takeover so cold standbys do not wait a second TTL.
2. Enforce unique client IDs for active `$share` replicas.
3. Isolate dispatch queues/concurrency by route or ordering key.
4. Add a byte-based memory admission budget and safe defaults.
5. Ship a real DynamoDB-backed HA deployment profile.
6. Add version-barrier cluster reconfiguration or limit live reload to one
   process and orchestrate cluster rollouts externally.

### P2: release and operational truth

1. Publish all Go submodules correctly.
2. Correct no-loss, orphan, failover, settlement, identity, and fencing claims.
3. Make persistence/failover/chaos tests required release gates.
4. Pin container bases by digest.
5. Require broker-side queue/session-expiry/discard monitoring in production
   runbooks because the bridge cannot report messages the broker never delivers.

## Production release gates

Do not label MQTT production-ready until all of these are green:

- **Identity:** 100,000 distinct equal-valued MQTT publishes without producer IDs
  produce 100,000 outputs or are rejected before acknowledgment with a clear
  configuration/runtime error.
- **Subscription migration:** exact, wildcard, and `$share` removals survive
  runtime replacement and process restart with the same persistent client ID;
  zero stale shared deliveries are stolen/dropped.
- **Reload safety:** broker URL, client ID, mode, clean start, and session expiry
  changes are refused without an explicit tested migration.
- **Settlement outage:** loss/duplicate accounting is exact when processor,
  destination, DLQ, outbox, and broker fail at each settlement boundary; no
  healthy-looking live connection remains permanently stalled.
- **Readiness:** Full proves every handler is registered and every subscription
  has at least its requested QoS.
- **Failover:** kill the verified current leaseholder under production timings
  and measure failure-to-Full. Publish warm and cold p50/p95/p99/max separately.
  Advertise 30-60 seconds only if the chosen percentile and all environmental
  assumptions fit it.
- **Cluster identity:** a duplicate `$share` client-ID deployment fails before
  traffic.
- **Cluster reload:** per-process live reload is rejected; the external
  whole-cohort runbook proves the version/readiness barrier or rolls the cohort
  back before ingress resumes.
- **Memory:** maximum payload at full receive/dispatch windows stays below the
  container memory limit with measured headroom.
- **Packaging:** a clean external Go module can import the MQTT adapter; the
  release image is digest-pinned and SBOM/provenance-scanned.
- **Repository gates:** `make lint`, `make test`, required Docker-backed
  integration tests, and long-running MQTT/cluster chaos suites pass uncached on
  the release revision.

## Final answer

The remediated implementation is a **release candidate for its narrowed,
documented contract**. The baseline blockers and all confirmed in-repository
high-risk findings are resolved or fail closed.

Public production release remains **NO-GO** until the external sequence proves
public module resolution, immutable image evidence, and credentialed AWS
failover percentiles on the exact release revision. GoBridge does not claim
zero bugs, unconditional no-loss delivery, atomic live cluster
reconfiguration, or a universal 30-60 second failover.
