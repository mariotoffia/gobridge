# Changelog

All notable changes to GoBridge are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**One version for everything.** Every published module in this repository
carries the same version, so a single entry below describes the whole release —
there is no per-module changelog. See [RELEASE.md](RELEASE.md#one-version-for-everything).

## [Unreleased]

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
