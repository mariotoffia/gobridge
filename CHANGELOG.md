# Changelog

All notable changes to GoBridge are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**One version for everything.** Every published module in this repository
carries the same version, so a single entry below describes the whole release —
there is no per-module changelog. See [RELEASE.md](RELEASE.md#one-version-for-everything).

## [Unreleased]

### Changed — BREAKING (`ports.BackoffDef.Jitter`)

- `routes[].policy.backoff.jitter` is now a `*float64`, so an omitted field and
  an explicit `0` are distinguishable. Omitting it takes the recommended
  de-correlation default; writing `jitter: 0` opts out and keeps the
  deterministic exponential delay. YAML and JSON blueprints are unaffected —
  only Go code that builds a `ports.BackoffDef` literal needs the address-of.

  ```go
  // before
  ports.BackoffDef{Jitter: 0.2}
  // after
  jitter := 0.2
  ports.BackoffDef{Jitter: &jitter}
  ```

  The domain counterpart `routing.BackoffPolicy.JitterFactor` stays a `float64`
  and gains the same tri-state: zero means unset and is filled with
  `routing.DefaultJitterFactor`; `routing.JitterDisabled` is the explicit
  opt-out.

### Changed — BREAKING (`ports.OutboxStore`)

- `OutboxStore.Expire` now takes the caller's fencing token. The bulk expiry
  sweep terminally destroys pending records, but it was authorised only by a
  local lease check inside the drainer: an owner that passed that check and then
  lost its lease could still bulk-expire pending drop-policy records its
  successor was entitled to deliver. The token is now enforced at the store
  transaction.

  ```go
  // before
  Expire(ctx context.Context, before time.Time, partition string) (int, error)
  // after
  Expire(ctx context.Context, before time.Time, partition string, token persistence.LeaseToken) (int, error)
  ```

  **Migrating an out-of-tree `OutboxStore`.** Add the parameter and enforce
  three rules, all of which `Claim` already implements in the same store:

  1. Reject an invalid (zero-value) token with `shared.ErrStaleFencingToken`
     before any state transition.
  2. Reject a token whose `Version` is below the partition's fencing
     high-water-mark with `shared.ErrStaleFencingToken`, having expired nothing.
  3. On acceptance, raise the high-water-mark to `token.Version` — including
     when the sweep expires no records — and keep the check atomic against a
     concurrent raise.

  Existing fence records need no schema rewrite: the sweep reuses the same
  per-partition fence state `Claim` already maintains (`outbox_partition_fence`
  on SQLite, the `FENCE` item on DynamoDB, `latestVersion` in memory). The
  DynamoDB sweep now issues one `TransactWriteItems` per expired record instead
  of one `UpdateItem`, pairing the record write with a fence `ConditionCheck`.

### Added

- **`MQTTReceiverEmitRejected`.** Counts inbound deliveries the route pipeline
  refuses at emit — a shutting-down or wedged route runner — tagged
  `outcome=recovering` (durable QoS 1/2: left un-acked, redelivered by the
  bounded session recycle) or `outcome=lost` (QoS 0: no acknowledgement to
  withhold and no redelivery contract, so the message is gone). The QoS 0 loss
  previously produced only a Debug log, so with production logging it left no
  trace at all.

- **Crash-durable success boundary and `ports.CrashDurableStoreFactory`.** A nil
  error from `OutboxStore.Persist` or `DLQAdmin.Write` now explicitly means the
  record survives the loss of the process — the runtime settles the SOURCE on
  that nil, so a store returning it after mere local buffering converted a crash
  into silent loss of acknowledged work while conforming to the written
  contract. Stores declare their posture through the new OPTIONAL
  `CrashDurableStoreFactory` (`IsCrashDurable() bool`); a factory that does not
  implement it is treated as NOT crash-durable, mirroring
  `DistributedStoreFactory`. `sqlite` and `dynamodb` declare durable, `memory`
  declares volatile.

- **`acknowledge_volatile` on the `memory` store.** The in-memory OUTBOX and DLQ
  now fail closed at construction until the operator acknowledges that a
  restart loses accepted work and the terminal evidence of dropped work. This
  mirrors the lease store's existing `acknowledge_single_replica`; the two keys
  are independent and each guards only its own roles. On an acknowledged
  volatile outbox or DLQ the builder additionally warns at startup, naming every
  affected route.

- **Ordering-key head-of-line rule on `OutboxStore.Claim`.** A record carrying
  `x-bridge.ordering-key` is now claimable only when the partition holds no
  older non-terminal record on the same key that the same `Claim` will not also
  return. Per-key order was previously enforced only WITHIN one claimed batch,
  so a head left `Claimed` by a previous cycle — a failed `Release`, an
  abandoned batch, a crashed owner — was silently overtaken by its younger
  sibling. Enforced by every backend and pinned by the shared conformance suite.

  The key is denormalised so a claim never unmarshals a record to read it:
  SQLite gets an `ordering_key` column and the partial index
  `idx_outbox_ordering`, DynamoDB an `ordering_key` attribute stamped at
  `Persist`, and the in-memory store reads it off the aggregate. There is no
  data migration and no backfill: GoBridge has never been deployed, so no store
  holds a record written without it.

- **`ports.OutboxClaimedDepthReporter`** (OPTIONAL): `CountClaimed(ctx,
  partitionKey)` reports records currently CLAIMED. `CountPending` deliberately
  excludes them, so work stranded by a failed release was invisible — the
  backlog gauge read zero while messages sat undelivered. The drainer emits it
  as `OutboxClaimedDepth` on the drain cadence; all three in-tree backends
  implement it. Stores that do not are unaffected: no capability, no gauge.

- **`OutboxClaimedDepth`** and **`DLQDuplicateSuppressed`** metrics, plus
  **`DynamoDBOutboxClaimTruncated`** (adapter-owned).

### Fixed

- **A shutdown could acknowledge and discard in-flight messages.** When the
  bridge cancelled a delivery — a SIGTERM, a reconfiguration swap that outran
  the drain budget, or a receiver restarting its route — the cancellation
  reached the pipeline as an ordinary recoverable error and was fed to the
  replay-cap gate. For a message whose identity the source did not supply (the
  common MQTT publish) that gate reports "already at the cap" on the first
  failure, so under `on_permanent_failure: drop`, or on a route with no DLQ
  store, the message was dropped and acknowledged. Every rolling restart could
  discard whatever was in flight. Cancellation is now recognised at the entry of
  every recoverable dispatch branch (send, processor chain, destination resolve,
  outbox depth check, outbox build, outbox persist) and in the delivery-panic
  recovery: the delivery is left **unsettled**, so the source redelivers it. A
  send that merely exceeded its own `send_timeout` while the delivery context
  stayed live is unchanged — that is a genuine target failure.

- **A DLQ redrive that was never delivered deleted its own evidence.** The
  admin redrive path deletes an entry once the inject "succeeds", but an
  injected message is settled through a synthetic delivery whose acknowledgement
  always succeeds — so a replay the route DROPPED or wrote back to the DLQ read
  as a successful delivery, and the entry (the message's last copy) was deleted
  and counted on `DLQRedrives`. The route now reports a terminal settle that
  delivered nothing, `Runtime.Inject`/`InjectRedrive` surface it as an error,
  and the admin API answers 207 with the reason, counts `DLQRedriveFailures`,
  and leaves the entry in place. A redriven message also no longer inherits the
  original's redelivery history — neither the adapter-generated identity marker
  (`x-bridge.generated-id`) nor the source transport's redelivery counter
  (`sqs.ApproximateReceiveCount` and its siblings, which are usually what
  exhausted the replay cap in the first place). A redrive is operator-issued
  under a fresh bridge-minted ID, so it gets the route's normal retry budget
  instead of being sunk on the first downstream blip.

  `POST /api/v1/admin/routes/{routeID}/inject` gains the same honesty: a message
  the route processed but did not deliver now answers **422** with the reason and
  audits outcome `not_delivered`, instead of reporting `{"status":"injected"}`.
  Programmatically, `ports.RuntimeCommand.Inject` returns an error wrapping the
  new `ports.ErrInjectNotDelivered` for that case; a nil error now means the
  message was delivered.

- **Retries were charged and paced by the wrong counter.** The send path
  computed its backoff from the source transport's native redelivery count, so a
  source that supplies none (MQTT, AMQP 0-9-1) retried at `initial_interval`
  forever instead of backing off; it now uses the same attempt count every other
  transient path uses. Separately, a retry the message did not cause — a full
  outbox partition, a failed outbox depth query, or a DLQ store that refused the
  record — spent the message's replay budget, so a message that only queued
  behind a slow drainer could be poisoned (DLQ'd, or dropped under
  `on_permanent_failure: drop`) on its first genuine transient error. Those
  retries no longer charge the budget — nor does an outbox `Persist` that hit
  the bounded store-operation deadline, which is the same slow-store fault. See
  [routes-and-runtime-reference.md](docs/routes-and-runtime-reference.md) —
  "What counts as a retry attempt".

- **Routes loaded from a blueprint retried without the recommended jitter.** The
  20 % equal-jitter that de-correlates retries across replicas lived only in
  `NewDefaultBackoffPolicy`, which a config-loaded route never calls, so an
  entire replica set re-attempted a failed target on the same tick while
  programmatically built routes staggered. `WithDefaults` now fills the one
  shared default, and an operator who wants deterministic backoff writes
  `jitter: 0` (see the breaking note above).

- **A retry multiplier below one turned backoff into acceleration.** Nothing
  rejected `multiplier: 0.5`: each retry fired sooner than the one before it, so
  a failing target was hammered hardest exactly when it was least able to
  recover. The config validator, the builder and the runtime start validator now
  require `>= 1` (exactly `1` is a fixed retry interval).

- **Negative retry intervals and an invalid `broker_health_step_down` committed
  before failing.** `time.ParseDuration` accepts a leading `-`, and both fields
  were checked only where the builder consumes them — at apply or the next
  restart — so an invalid value passed the config transaction and its durable
  write, then failed on the rollback/divergence path. Both are now rejected
  before the commit, alongside the retry rules above.

- **`bridge.log_level` accepted values it then ignored.** The field is a closed
  enum, but nothing validated it, and a composition root keeps its current level
  for an unrecognised value — so an operator who set `DEUBG` to diagnose an
  incident saw a clean commit and no extra logging. One enum table now backs
  both validation and every root that applies the level, with `warning` an
  accepted alias of `warn`.

- **A shared-outbox route wired without an outbox store retried forever.** The
  branch handling a missing `OutboxStore` bypassed the replay cap, so every
  message looped on a one-second retry behind green liveness. A missing store is
  a wiring defect no redelivery can fix: the route now fails terminally and the
  supervisor escalates, leaving the delivery unsettled so nothing is acked or
  dropped. Startup validation still rejects the shape up front; only direct
  library composition could reach it.

- **A disabled send timeout wedged the route on its first send.** `SendTimeout: 0`
  means "no wedge bound", but the zero was handed to a timer, which fires
  immediately — so every send was classified as hung. A zero bound now arms no
  timer at all.

- **A broker's refusal reason code was thrown away with the SDK's generic
  error.** The MQTT client returns the acknowledgement *and* an error for any
  SUBACK / UNSUBACK / PUBACK reason code of `0x80` or higher, and the adapter
  kept only the error. Reason `0x87` (*Not authorized*) on a publish therefore
  classified as `UNAVAILABLE` — transient — so the route retried a message the
  broker will never accept until the replay budget ran out, then dead-lettered
  it as `max_retries` with the real cause lost. Reason codes are now classified
  first and the SDK error is the fallback for a call that produced no
  acknowledgement at all, so a denial is `FORBIDDEN` and terminal on the first
  attempt. A partially accepted SUBACK also keeps its grants: the filters the
  broker did grant are recorded as broker-observed state instead of being
  discarded with the call.

- **The MQTT client's hidden ten-second packet deadline overrode every
  configured budget.** The SDK bounds each acknowledgement wait *inside* the
  caller's context and defaults that bound to ten seconds, shorter than
  `connect_timeout`, `reconnect_timeout`, `reconcile_timeout` and the sender
  `timeout` alike. A healthy SUBACK at twelve seconds was abandoned mid-reconcile
  and a publish could not use the budget it was given. The session now derives
  the packet budget from the longest of those deadlines — including the `timeout`
  of every sender bound to it — so the adapter-owned bound is the one that
  governs. There is no new configuration key. One consequence: a SUBSCRIBE or
  UNSUBSCRIBE in flight when a session is closed now waits out
  `reconcile_timeout` instead of the client's old ten seconds. Cancelling the
  context passed to `Reconcile` still ends it at once, which is what the runtime
  does on shutdown.

- **Subscription filters and QoS were not validated before activation.** Only
  the first non-empty topic of a receiver was checked, and nothing checked QoS
  at all. A malformed filter failed after the process had started serving, and
  an out-of-range QoS did not fail: the SDK writes the level as `qos & 0x03`, so
  `qos: 4` reached the broker as `0` and the route believed it had subscribed
  at-least-once while the broker delivered at-most-once. Every filter and QoS is
  now validated at the factory seam and again at reconcile, through one shared
  validator, so a blueprint and a direct library caller fail identically. An
  empty topic is rejected rather than skipped — the session plan is built from
  the same list, so a topic the receiver dropped was still sent to the broker.

- **Legal `$` publish namespaces were terminalized inside the bridge.** Every
  `$`-prefixed publish topic was rejected as reserved, which is not what MQTT v5
  §4.7.2 says: the prefix is reserved for the *server* to define, and real
  brokers define legal write namespaces there — AWS IoT's `$aws/rules/<rule>`
  republish target most of all. Those messages never reached the broker. The
  blanket rejection is gone; `$share/` stays refused because it names a
  subscription group and can never be a publish destination. Whether a given
  namespace accepts a write is the broker's authorization decision, and its
  refusal now arrives as a classified reason code.

- **A negative MQTT duration was accepted and failed only at runtime.** Nothing
  checked the sign of `connect_timeout`, `reconnect_timeout`,
  `reconcile_timeout`, `reconnect_delay`, `reconnect_max_delay`,
  `unmatched_grace`, `sender.timeout` or `sender.throttle_retry_after`. A
  negative value became an already-expired context, so the session built
  successfully and then failed every attempt it made for a reason invisible in
  the configuration. All eight are now rejected by `Config.Validate`; `0` still
  selects the documented default.

- **MQTT configuration failures were reported as message-payload rejections.**
  A missing `client_id`, an empty `broker_urls`, an invalid `client_id_suffix`,
  an unpublishable `default_topic`, an out-of-range sender `qos`, a session or
  sender built against the wrong transport, and cleartext credentials on a
  non-TLS broker all returned `INVALID_PAYLOAD`. That code is `rejected` and
  reserved for a rejected message, while a configuration failure is `permanent`
  and needs a human, so automation and metrics attributed deployment errors to
  message traffic. All of them now return `INVALID_CONFIG`.

- **A CONNACK backlog delivered before `OnConnectionUp` was purged un-acked and
  wedged the live connection's acknowledgement order.** Paho starts delivering
  publishes from inside `Client.Connect`, while autopaho calls `OnConnectionUp`
  only after the connection is fully established, so a broker replaying a queued
  QoS 1/2 backlog reached the router first. Those publishes were stamped with the
  PREVIOUS connection generation (or hit the recycle window's discard flag) and
  dropped without acknowledgement — although their acknowledgement belonged to
  the LIVE client. The un-acked packet then sat at the head of paho's
  contiguous-prefix acknowledgement tracker: every later `Delivery.Ack` on the
  session reported success while no PUBACK was written, and after
  `receive_maximum` such packets QoS 1/2 ingress was dead until an unrelated
  disconnect. Health read clean throughout, because the same callback cleared the
  unsettled bookkeeping. The connection generation is now opened by whichever of
  the connection-up callback or the first packet from a Paho client the router
  has not seen arrives FIRST — the latter is proof the previous generation is
  dead, since autopaho only builds a replacement client after the old one has
  shut down.

- **A covered QoS 0 drop leaked one unit of the ingress admission budget, every
  time.** The branch that drops a still-covered QoS 0 publish the pending buffer
  cannot hold returned without releasing its reservation. Since release is the
  only decrement, `receive_maximum` such drops retired the whole budget: nothing
  could be admitted again and the connection died of keepalive starvation with
  the process still reporting connected.

- **Grace-buffered QoS 0 could starve QoS 1/2 admission and stall the MQTT
  connection.** Pending QoS 0 entries hold admission budget while sitting outside
  the broker's Receive-Maximum window, so with no handler registered for a topic
  set (a route in supervisor backoff, a late registration) a saturated budget
  parked the next QoS 1/2 publish inside paho's single publish-callback
  goroutine. That goroutine also reads PINGRESP, so keepalive killed the
  connection — and because the callback never returned, autopaho observed neither
  client shutdown nor a connection-down edge, leaving the session reporting
  connected. A QoS 1/2 admission now reclaims the oldest pending QoS 0's slot and
  waits only when the budget is entirely QoS 1/2. The QoS 0 drop log also names
  the refusing bound, so an exhausted budget is no longer reported as a full
  startup-grace buffer.

- **`Session.Close` disconnected the client before stopping the router, so a
  parked publish callback pinned the close for its whole deadline.** autopaho's
  `Disconnect` waits for the client's worker goroutines, one of which runs our
  publish callback; the only thing that releases a callback parked in the router
  is `router.shutdown()`, which Close ran afterwards. Close burned its context
  and returned a timeout, which the session manager reads as a wedged close —
  retaining an exclusive lease until its TTL instead of handing it to a standby.
  Close now stops the router first.

- **A source session Close received no deadline of its own.** The
  session-failure teardown raced `Close` against a hard ceiling but passed it an
  unbounded (only detached) context, so a cooperative-but-slow disconnect had
  nothing to abort on: it ran past the ceiling, was classified as a wedge,
  terminalized the process, and extended the outage to the lease TTL. It now
  carries the same bounded-teardown budget the sibling lease release uses. This
  is safe only because the MQTT close stops its router before any bounded
  network wait (above), so a close that returns its context error has still
  stopped that session from dispatching or acknowledging ingress — which is what
  the manager relies on when it releases the lease after Close returns.

- **A settlement-recovery drain was capped at five seconds, turning one slow
  target into a restart loop.** The drain waits for deliveries the runtime has
  already accepted, each bounded by its own route's send-wedge, processor and
  store ceilings — all of which legitimately exceed five seconds under the
  default 30-second send budget. Exceeding the cap was classified as an
  unrecoverable drain failure, which terminalizes the session and restarts every
  unrelated route in the process. The adapter no longer imposes a bound on that
  phase: `reconcile_timeout` bounds only the adapter-owned teardown that precedes
  it, and the recovery attempt budget is the outer bound.

- **The reconnect-acknowledgement metric missed the settlements it exists to
  measure.** `MQTTAckAfterReconnect` counted only paho's `ErrPacketNotFound`, but
  the acknowledgement tracker marks an ack and flushes the acknowledged prefix
  asynchronously — an ack marked just before the connection dropped returns
  success and is still redelivered, so the guaranteed duplicate went uncounted.
  Detection now compares the Paho CLIENT captured at receive against the live
  one; SDK errors stay reserved for classifying the operation. It is deliberately
  not the connection epoch, which also advances for a recycle on a still-live
  socket — that would report every settlement in a routine drain as a guaranteed
  redelivery and swallow a real acknowledgement failure on a connection that
  never cycled.

- **A settlement-recovery cooldown outlived the session by up to 30 seconds.**
  The rate-limit wait runs on a deliberately detached context so a route-scoped
  cancellation cannot abort a recycle, which left `Close` with nothing to wake
  it. It is now bound to the session lifetime and the timer is stopped on every
  exit.

- **A process-volatile lease store could regress a durable outbox's fencing
  version and wedge the partition forever.** The in-memory lease numbers fencing
  versions from a per-process counter that restarts at zero, while the SQLite
  and DynamoDB outboxes persist a per-partition fencing high-water-mark and
  reject every claim below it. Once the durable mark had passed 1 — one prior
  re-acquire is enough — a restart or a store-rebuilding reload handed the new
  owner a version below the mark, every `Claim` and `Expire` was rejected as
  stale, and the partition never drained again while ingress kept acknowledging
  into it. The builder now REJECTS the pairing at startup. There is no
  acknowledgement for it: it is a permanent loss of progress, not a tradeoff.
  The check is scoped to blueprints carrying a `shared_outbox` route — a
  fencing token only reaches the store from a drainer, so a durable outbox
  nothing drains cannot wedge. Existing `memory` lease + `sqlite`/`dynamodb`
  outbox configurations must move the LEASE to `dynamodb`; downgrading the
  outbox to `memory` instead abandons every record already in the durable
  store, so drain it first if it holds a backlog.

- `TestAutoExtendRetriesTransientThenSucceedsS15` (SQS auto-extend) gave its
  fake-clock sync points a 1s WALL-CLOCK budget. `Advance` only releases the
  auto-extend goroutine; it still has to be scheduled, and under a parallel
  `-race` integration run that slipped past 1s. Widened to the repository's
  2s default.
- `make test` / `make lint` walked every `go.mod` under the repository root,
  which swept any sibling git worktree under `.worktrees/` — another BRANCH's
  checkout. That says nothing about the branch under test, doubles the runtime,
  and failed outright on the sibling's `scripts/release` module, whose `GOWORK`
  override is keyed to the literal path `./scripts/release`. Module discovery now
  excludes `.worktrees/`.

### Removed

- **Every remaining backward-compatibility path.** GoBridge has never been
  deployed, so no store holds data written by an earlier build and none of this
  machinery could ever run. Removed as one sweep:

  - **DynamoDB `ClaimIndex` is now REQUIRED.** `Claim` no longer latches a
    missing or under-projected index and silently degrades to a whole-partition
    scan; preflight rejects such a table at startup and a claim-time index
    failure surfaces as an error naming the index. `CreateTable` and the CDK
    construct both provision it as `Projection: ALL`. Degrading turned an
    O(limit) drain into O(backlog) fleet-wide behind a WARN nobody reads, and
    `CountPending` / `CountClaimed` likewise reported a broken table as an
    unsupported capability — a plausible-looking gauge over a fault.
  - **The DynamoDB sort-key cross-scheme migration path.** Records written
    before the sort key was made injective cannot exist. The verify-on-conflict
    readback is KEPT — it is what stops a distinct message being acked and
    dropped when a foreign writer occupies a key — but it is no longer described
    as a migration state.
  - **The `sqlitedlq` `address` column migration** and its test.
  - **The replay-budget `CreatedAt` fallback.** A record reaching the poison
    gate has just been claimed, and every backend stamps `FirstAttemptedAt` on
    the first claim, so a zero value means the store broke that contract. The
    drainer now reports the budget UNSPENT and keeps retrying instead of
    guessing an age from `CreatedAt` — poisoning routes a message to the DLQ or
    drops it outright, so a store bug must not be able to destroy messages. This
    removes `runtime.WithOutboxPoisonMinAge`, `outbox.Config.PoisonMinAge` and
    the `PoisonMinAge` glossary term.
  - **The legacy fixed drain-batch ceiling.** `bridge.drain_timeout` fed two
    unrelated budgets: the supervisor's ceiling on `Runtime.Stop`, and — through
    `session.Config`/`outbox.Config` — a fixed per-batch outbox ceiling
    explicitly "retained for backward compatibility". The second meaning is
    gone; `ComputeBatchDeadline` is now always
    `min(batchCount * per_record_drain_timeout, max_drain_timeout)`. The YAML key
    survives with exactly one documented meaning, the stop budget.

    This also fixes a validation bug: `stale_claim_duration`'s in-flight ceiling
    read `bridge.drain_timeout` as if it were the batch ceiling, inflating the
    warning band from 10s to 30s of pure fiction.

    The shutdown FINAL drain is now bounded by `max_drain_timeout` too — it is
    a drain batch, so it takes the batch ceiling rather than the supervisor's
    stop budget. A deployment that set a long `bridge.drain_timeout` expecting a
    longer final drain should set `max_drain_timeout` instead; the supervisor's
    stop budget still bounds `Runtime.Stop` above it.

    An unconfigured batch ceiling changes from a flat 10s to
    `min(n * 3s, 10s)`. It is inert in practice — the ceiling may only RAISE a
    budget already floored at the sequential send depth times
    (`send_timeout` + complete margin), which is 30s+ on defaults — and that is
    now pinned by `TestBatchTimeout_CeilingOnlyRaisesNeverCuts` rather than
    asserted.

  - **`docs/runbooks/dynamodb-outbox-gsi-migration.md`**, replaced by
    [`dynamodb-outbox-table-schema.md`](docs/runbooks/dynamodb-outbox-table-schema.md):
    the required table and index shape, why `ClaimIndex` must be
    `Projection: ALL`, and how to read a preflight rejection — no migration
    steps, because there is nothing to migrate from.

- **In-place SQLite schema migration.** The outbox store carried column
  migrations (`claimed_at`, `first_attempted_at`, `seq`, fence `updated_at`), a
  table rebuild that dropped a legacy global `UNIQUE(envelope_id, binding_id)`,
  and a stamp for fence rows predating `updated_at` — all of it for databases no
  deployment has ever produced. `openSession` is now the DDL and nothing else,
  which also removes the failure mode where adding a column silently broke the
  rebuild's hard-coded copy list. GoBridge has never been deployed; a migration
  ships when there is deployed data to migrate.

### Changed

- **DLQ entry identity is derived, not random.** A DLQ entry's ID is now
  `sha256(envelope ID, route, binding, source)` instead of a fresh random value
  per write. A DLQ write is durable BEFORE the source delivery is settled, so a
  failed settle redelivered the message, it failed identically, and a SECOND
  distinct row was written — one terminal event accumulating duplicates for as
  long as settlement kept failing. The repeat write now lands on the same entry,
  which the stores refuse as a duplicate; the router reports that refusal as
  durable success (the evidence is already there) and counts
  `DLQDuplicateSuppressed`. The attempt counter is deliberately NOT part of the
  identity — a redelivery IS a later attempt, so including it would collapse
  nothing. The first write wins, so the earliest evidence is preserved.

  DLQ writes remain **at-least-once across distinct failures**: a message that
  is redriven and fails again on the same leg re-uses the identity and updates
  nothing, and a lease lost mid-write can still duplicate. Duplicates are
  reconcilable; loss is not.

- **`Claim` may return a SHORT batch with a nil error.** A backend that claims
  one record per remote transaction can fail after earlier records are durably
  claimed. Those records now come back to the caller instead of being discarded
  with the error. Out-of-tree `OutboxStore` implementations should adopt the
  same rule; callers already tolerate a short batch.

- **An explicit `stale_claim_duration` is now bounded from below.** At or below
  the largest route `send_timeout` it is REJECTED (a reclaim would re-send a
  record whose first delivery has not even timed out); at or below
  `send_timeout` plus the drain-batch ceiling it WARNS. Leaving the key unset
  keeps the derived default and applies no bound.

### Fixed

- The outbox expiry sweep and the depth query (`CountPending`) ran on the
  drainer's long-lived loop context. A black-holed store could park the single
  drain goroutine for a partition in housekeeping work indefinitely, stalling
  the send path with it. Both now run under the same per-operation deadline
  `Claim` uses.
- The DynamoDB `ClaimIndex` GSI cannot be read strongly consistent and
  propagates per item, so it could surface a younger same-key record before its
  older sibling — an ordering violation with no failure anywhere, and invisible
  to tests because DynamoDB Local's GSIs are synchronously consistent. A claim
  that sees any ordering key now abandons the index and re-runs through the
  `ConsistentRead` base-table scan. Keyless partitions keep the O(limit) fast
  path.
- A DynamoDB `Claim` that hit a throttle or its own deadline part-way through
  the batch discarded every record it had already claimed. Those records were
  durably claimed, hidden from `CountPending`, unreclaimable until the stale
  window, and charged a replay attempt per recovery cycle — so a short
  `send_timeout` relative to claim cost could poison a healthy backlog to the
  dead-letter queue without a single send. The claim now returns what it
  claimed, and its deadline scales with the batch
  (`max(send_timeout, limit x 100ms)`, capped at two minutes) instead of reusing
  the per-message send timeout.
- An unclassified per-record failure in the drain loop — a DLQ-store write
  error, or a post-send `Complete` the store refused — left the whole ordering
  group `Claimed`. The head keeps its claim (its send may already have landed),
  but the unattempted tail is now released back to pending.
- `DeliveryAttempt.Attempt` / `DeliveryOutcome.Attempt` were off by one on the
  outbox path: `OutboxRecord.Claim` already counts the claim being attempted, so
  the first delivery reported attempt 2 while the direct path reported 1 for the
  same message.

## [0.3.3] - 2026-07-27

Completes 0.3.2: same modules, plus the two release-pipeline fixes that
0.3.2's own train exposed. This is the first version whose train runs clean
end to end, including the container image association and `latest` promotion.

### Fixed

- `image-association` used a single variable both to re-resolve the tag being
  released and to locate the Release hosting the digest asset. Once Releases
  became root-only these are different commits — root is tagged first — so the
  job failed after the image had already built and passed its vulnerability
  scan. Split into `VERIFY_TAG` and `RELEASE_TAG`. In 0.3.2 this left the
  scanned image published to GHCR by digest but not recorded on the release,
  and `latest` unmoved.
- Waiting on a layer spawned one `gh run watch` per module, which for the
  26-module layer exhausted the GitHub API rate limit and failed the layer on
  `HTTP 403` even though every workflow had succeeded. A single `gh run list`
  per poll cycle now covers a whole layer, so polling cost is constant
  regardless of layer size.

## [0.3.2] - 2026-07-27

Re-release of the withdrawn 0.3.1. Same contents, plus the push fix below.

### Fixed

- Release trains pushed a layer's tags in one batch. GitHub does not create a
  workflow event for every ref in a bulk tag push — past roughly three tags the
  remainder silently get no run at all — so 26 layer-1 tags published without
  ever being verified. Tags are immutable, so they could not be re-triggered.
  Tags are now pushed one at a time; the concurrent part is the workflow wait,
  which is where the time actually goes.

## [0.3.1] - 2026-07-27 [WITHDRAWN]

**Do not use.** Only 27 of 31 modules were tagged, and 26 of those were never
verified by the release workflow. `cmd/gobridge`, `httpapi`,
`adapters/aws/store` and `adapters/native/store` have no 0.3.1 tag at all, so
the version is not a usable set. Superseded by 0.3.2, which contains
everything below.


Security patch, plus the release pipeline fixes that the 0.3.0 train exposed.

### Security

- `golang.org/x/net` raised to `v0.57.0` across every module.
  [CVE-2026-25681](https://github.com/advisories) (HIGH, arbitrary code
  execution in `golang.org/x/net/html`) affects `v0.52.0`, which 0.3.0 resolved
  transitively. The 0.3.0 container image was never published for exactly this
  reason: the release workflow's Trivy gate refused it. The Go modules were
  published, so anyone on 0.3.0 should move to 0.3.1.

### Fixed

- The release train raced the Go module proxy. A tag push starts the release
  workflow immediately, and its first act is to resolve from
  `proxy.golang.org` the module that push just created — before the proxy has
  fetched it. Both the published-module and internal-helper resolutions now
  wait on the observable state (module resolves, reports its `go.mod`, origin
  matches the tag commit) with time only as the failure budget. A wrong path,
  wrong version or mismatched origin still fails immediately; waiting cannot
  fix those.
- The release train could not be resumed. Tags are immutable, so a train that
  stopped part-way could only be continued, never restarted — re-running it
  aborted on `git tag` for an already-published module. Publishing now skips a
  tag that exists on the remote while still applying every gate to it.
- Five modules (`cmd/gobridge`, the CDK profile, `scripts/pluginsym`,
  `scripts/registrychk`, `tests/docsexamples`) could not `go mod tidy` outside
  the workspace: they reached `testutil/mqttlocal` through the paho adapter's
  tests but carried no `replace` for it. Every module in the repository now
  tidies standalone.

- A data race in `adapters/mqtt/transport/paho`: a test's ack counter was
  written from the router's grace-loop goroutine and read from the test body
  without synchronisation. Making it atomic exposed the real defect underneath
  — the assertion assumed the ack had already happened, when it can come from
  either `Reconcile`'s settle pass or the grace loop. It now waits on that
  state instead of assuming an ordering the router never promised.

### Changed

- One GitHub Release per version instead of one per module. All 31 modules
  still get tags, strict verification and proxy checks; only the root tag gets
  a human-facing Release page. The image digest asset attaches to it.
- Release trains publish each dependency layer concurrently instead of one
  module at a time. A layer means "these modules do not depend on each other",
  so waiting a full workflow round-trip between each of layer 1's 26 modules
  was pure queueing — about 80 minutes of it. Layers remain strictly
  sequential, because staging a layer runs `go mod tidy` against the previous
  layer's published versions. No gate changed: every tag still gets its own
  workflow, strict verification and proxy check.

## [0.3.0] - 2026-07-26

First release that can be installed from outside this repository. Earlier tags
(`v0.1.0`, `v0.2.0`) were root-only, had no nested module tags, and are not
consumable.

### Added

- **Transports**: MQTT v5 (shared sessions, QoS 0/1/2, wildcards, reconnect),
  AWS SQS (long polling, batch send, visibility extension, FIFO), Azure Service
  Bus (queues, topics/subscriptions, batch send, auto-extend lock), RabbitMQ /
  AMQP 0-9-1 (exchanges, bindings, publisher confirms, prefetch), AMQP 1.0
  (Artemis, Solace, Qpid), and HTTP (POST ingress, SSE egress).
- **Delivery guarantees**: `DirectHold` (send-then-ack) and `SharedOutbox`
  (persist-then-ack with a durable outbox drainer).
- **Stores**: lease, outbox, dead-letter, rollout and managed-subscription
  stores, with in-memory, SQLite and DynamoDB implementations.
- **Processor chain**: filter, transform, circuit breaker and tenant isolation.
- **Clustering**: lease-based exclusive ownership so multiple replicas process a
  stream exactly once, with automatic failover, plus coordinated configuration
  rollout across the cluster.
- **Dead-letter management**: poison messages are diverted rather than dropped
  or left to block the queue, and can be inspected and re-submitted.
- **Credentials**: URI-based resolution (`file://`, `pms://`) with scheme
  dispatch and caching.
- **HTTP APIs**: admin server for bridge lifecycle, route injection and DLQ
  management; monitor server for health probes and topology.
- **Observability**: OpenTelemetry metrics and tracing, CloudWatch metrics, and
  correlation-aware structured logging via `slog`.
- **Zero-dependency core**: the root module has no external dependencies; only
  the adapters you import pull anything in.
- 31 independently installable modules published under one shared version.

### Fixed

- The root module could not be released at all: its `bridge` rollout tests
  imported the `memorylease` and `memoryrollout` adapter modules, which the
  layer-0 root may not require. Both are now root-owned packages under
  `adapters/native/`, restoring the 31-module / 26-layer-1 set the release
  policy describes.
- `make test-long-running` and `make test-failover-gate` globbed
  `./tests/longrunning/...` from the repository root, which is a separate
  module. They resolved only through `go.work` and so never ran in CI. Both now
  use `go -C`.

### Changed

- Container test fixtures gate on protocol truth rather than on an open port. A
  published Docker port is bound by docker-proxy at container creation, so a TCP
  dial succeeds while the service is still starting or already dead. The Service
  Bus and Mosquitto fixtures now require a real message roundtrip before
  reporting ready.
- A fixture that fails to start is now a test failure wherever it could have
  started, instead of a skip. Previously any startup failure was swallowed by
  `t.Skipf`, which is how a permanently broken Service Bus emulator reported
  `ok` for its entire package. Skips remain only when Docker is absent, or for a
  declared prerequisite such as `LOCALSTACK_AUTH_TOKEN`.
- All container images used by tests are pinned by digest. Floating `:latest`
  tags meant CI could break on a day nobody changed anything.
- Fixture teardown drains containers (SIGTERM, wait for stop, remove, wait for
  removal) instead of `docker rm -f`, which killed brokers mid-flush and
  returned before Docker had finished.
- Images are fetched before `docker run` rather than through its implicit pull,
  which had to complete inside a 90-second timeout and therefore worked on a
  warm cache and failed on a cold one.
- `testutil/mqttlocal` is now its own module, so it can use a real MQTT client
  for its readiness probe without adding a dependency to the root module.
- The nightly scheduled CI run is paused; the full suite still runs on every
  push and pull request.

### Known limitations

- `cmd/gobridge` is a demonstration binary. It links only MQTT and the native
  stores and rejects configurations using anything else. Build a composition
  root for real deployments.
- The published container image is a release candidate. The production approval
  described in [RELEASE.md](RELEASE.md#image-publication) is a separate,
  credentialed step.
- The AWS SSM credential adapter and the CloudWatch metrics adapter have no
  integration coverage in CI: their tests depend on LocalStack, which requires a
  licence token that is not configured. Set `LOCALSTACK_AUTH_TOKEN` to run them.

[Unreleased]: https://github.com/mariotoffia/gobridge/compare/v0.3.3...HEAD
[0.3.3]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.3
[0.3.2]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.2
[0.3.1]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.1
[0.3.0]: https://github.com/mariotoffia/gobridge/releases/tag/v0.3.0
