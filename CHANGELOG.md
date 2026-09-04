# Changelog

All notable changes to GoBridge are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**One version for everything.** Every published module in this repository
carries the same version, so a single entry below describes the whole release —
there is no per-module changelog. See [RELEASE.md](RELEASE.md#one-version-for-everything).

## [Unreleased]

### Fixed — a plan-driven ingress session that only its receiver names now runs

- **The receiver's own binding manages its session.** An MQTT or AMQP 0-9-1
  receiver subscribes only when a session manager reconciles the session plan,
  and a session got a manager in exactly two ways: a route `session` block (a
  lease-held session whose outbox partition the route drains) or a binding
  `session_id` (the partition a binding's records live in). A `direct_hold`
  route can use neither — it holds no lease and persists no records — so the
  shape that mode exists for, an MQTT ingress on a durable session holding the
  broker delivery until the destination accepts it, built, reported the right
  mode and carried nothing. The builder now registers such a session with the
  runtime as an **ingress session** (`Runtime.RegisterIngressSession`): a plain
  manager that starts it, reconciles its plan and follows reconnects, with no
  lease and no outbox partition. A session has exactly one manager, so the
  registration refuses an id already held by a route session block or a
  session sender, and the lease-bearing paths keep precedence. A session
  declared `exclusive` is lease-held by definition and cannot be managed this
  way; the refusal for that shape now names both lease-bearing ways out and
  `persistent` as the lease-less one.
- **The deployed proof.** The local deployment matrix's MQTT topology runs its
  ingress on a persistent session on `direct_hold`, with its SQLite
  managed-subscription store on the deployed config mount, and asserts the
  mode, the lease-less session, the messages crossing and the store file on the
  mount. That also stands up the matrix's SQLite-on-a-deployed-task row.

### Added — a durable session's baseline, seeded by the task that owns the mount

- **`managed_subscription_baselines` in the bootstrap document, stamped from
  `GoBridgeSingle`'s `ManagedSubscriptionBaselines` prop.** A persistent or
  exclusive MQTT session does not start until its managed-subscription baseline
  exists, and nothing but the task can write a SQLite store on the config
  mount. The facade requires the attestation for every such session that
  subscribes (an empty list for a new broker identity), validates the filters
  at synth, and the runtime seeds them before it builds the bridge on every
  apply — not only at boot, because a task routinely boots on the start-empty
  config and the durable session arrives with the first real one. Seeding is
  idempotent and additive: an established baseline is kept and the listed
  filters are added, so a filter the running session later removed is re-added
  until the attestation is redeployed without it. The store keeps its database
  in a directory of its own
  under the mount, which it owns with mode `0700`; see
  [SQLite stores on the config mount](docs/aws-deployment/storage-and-secrets.md#sqlite-stores-on-the-config-mount).

### Added — secure-broker evidence, injected network faults, and a release gate that names its proofs

- **An authenticated, certificate-validating broker fixture.**
  `testutil/mqttlocal` grew `WithAuth`, `WithTLS` and `WithMutualTLS`: a
  Mosquitto with anonymous access disabled, a CA-signed server certificate and
  an optional client-certificate requirement, plus `ws`/`wss` listeners. The
  Mosquitto password entry is rendered in Go (PBKDF2-SHA512), so a secure
  fixture still costs one container. Every deployment reaches its broker over
  one of these paths and none of them had real-broker evidence: direct TLS,
  mutual TLS, a refused certificate, a wrong password surfacing as
  `ErrNotAuthorized`, live credential rotation, and authenticated `ws`/`wss`
  traffic are now proved against a broker that actually refuses. Each proof
  carries its negative — the connection that must fail — because a permissive
  broker would make the positives green on its own.
- **`testutil/netfault`, a bounded TCP fault-injection proxy.** Reconnect and
  settlement behaviour under partial network failure was inferred rather than
  tested; stopping a container is one failure mode, and the mildest. The proxy
  drives a partition, a half-open connection that stays writable and delivers
  nothing, injected latency, and an endpoint that serves no new connections
  while its live ones keep working. Each MQTT proof requires recovery inside
  the bound the session's own reconnect policy declares. Per-segment packet
  loss is deliberately not modelled, and the package says why.
- **Multi-URL failover, Last Will and server limits, proved.** A session now
  demonstrably leaves an endpoint that stops carrying sessions for a healthy
  one; a configured Last Will fires on ungraceful death and is suppressed by a
  graceful DISCONNECT; a broker inflight quota far below the bridge's own
  loses nothing; and an oversized publish fails rather than vanishing.
- **[MQTT broker support](docs/transports/mqtt-broker-support.md)** states what
  is proved and against what — Mosquitto 2.0.22, MQTT v5, pinned by digest —
  and, explicitly, what is not: other broker products, AWS IoT Core, broker-side
  high availability, MQTT v3.1.1. Every claim names the test that fails if it
  regresses, and `tests/docsexamples` pins those names against the source.
- **A release gate that cannot silently select nothing.** `make
  test-release-gate` runs the long-running proofs a release rests on, named
  test by test; `make test-soak` runs the published 60-minute soak profile
  (the ordinary suite runs the same test at a 5-minute smoke profile); `make
  fuzz` mutates every fuzz target. `go test -run` treats a pattern that matches
  nothing as success, so each name is pinned against the suite from the default
  build. Two proofs were added for that gate: the failover objective on the
  lease profile operators actually deploy, and message conservation at a volume
  derived from the receive window with duplicates reported rather than
  forbidden.
- **The long-running module is compiled on every pull request.** It has no
  default-tag packages, so every module walk skipped it and a refactor could
  break every production proof in it while the branch stayed green. `make lint`
  and CI now vet it under its own tag. Nothing carrying the tag runs in the
  cloud; that is unchanged and deliberate.
- **Two fuzz targets covering the durable wire form.** `FuzzEnvelopeHeaderRoundTrip`
  and `FuzzEnvelopeRehydrationRejectsCorruptRecords` in `domain/messaging`, and
  `FuzzIngressPublishProperties` in the MQTT adapter, which mutates Correlation
  Data, content type and User Properties against the reserved-namespace,
  identity-stability and header-bound rules.

### Fixed — an envelope ID that is not valid UTF-8 no longer loses its identity in the store

- Found by the new envelope fuzz target within seconds. Every durable record —
  DLQ entry, outbox row — is keyed by the envelope ID and written through
  `Envelope.MarshalJSON`, and `encoding/json` replaces a byte sequence that is
  not valid UTF-8 with U+FFFD. An ID that was not valid UTF-8 therefore came
  back from the store as a DIFFERENT identity than the one that went in, so a
  redrive injected a message the replay ledger could not match to the original
  and the accounting that makes at-least-once delivery countable broke
  silently. Construction now refuses such an ID (`ErrInvalidEnvelopeID`, which
  adapters already classify as terminal) rather than corrupting it at the store
  boundary. Transports that carry binary identity were already normalising —
  the MQTT adapter base64-encodes binary Correlation Data before using it as an
  identity — so this codifies an invariant the adapters honoured and the domain
  did not enforce.

### Changed — long-running progress gates wait for progress, not for a clock

- A gate that says "wait until 1,000 have flowed, then kill an instance" was
  written as a wall-clock deadline, which makes it an undeclared throughput
  assertion inside a test whose claim is that no message is lost across a
  restart. `lrWaitForProgress` gives up when the count STOPS MOVING instead. A
  slow machine now takes longer and still proves the same thing; a genuinely
  wedged pipeline fails sooner, and says how far it got rather than "condition
  not met".
- Its stall window is derived, not chosen: it has to outlast the source's
  redelivery cycle. When a visibility extension fails, the message is invisible
  until the queue's visibility timeout elapses and SQS hands it back — that
  redelivery IS the recovery. A window equal to the visibility timeout gives up
  at the exact moment recovery is due, which is what a first attempt did.

### Fixed — a fuzz target that measured the machine, not the code

- `FuzzRenderAddress` raced each call against a 100 ms wall clock in a
  goroutine. Fuzzing saturates every core by design, so on a loaded host that
  budget measures scheduler latency rather than `RenderAddress`: the first
  mutation run under `make fuzz` "failed" and wrote a crasher into the corpus,
  where it would have stayed forever. The same input renders in **4
  microseconds**. The wall-clock race is gone — a genuine hang is what the
  fuzzing engine itself detects — and the input's shape is kept as an explicit
  seed, which was the part worth having.

### Fixed — the long-running collector counted and retained in different places

- `mqttCollector.count()` read `len(messages)` while three of the four
  constructors appended to `messages` through their own handler. Adding a
  counting mode to one of them broke the other three silently: `count()`
  returned zero forever, and every test waiting on it timed out with no output.
  There is now one writer (`record`), and the zero value RETAINS — so a
  collector built by a struct literal anywhere in the suite keeps the behaviour
  it had before the field existed.

### Fixed — six long-running proofs waited on the wrong quantity

- `TestUC12_RollingRestart_NoMessageLoss`, `TestUC13_SplitBrain_Recovery`,
  `TestUC3ClusterFailover`, `TestUC33_MaxInFlight1_Serial`,
  `TestUC42_BrokerKillRestart_SharedOutbox` and
  `TestRES005_AutoExtendFailureDuplicates` each waited for N raw deliveries and
  then asserted on N DISTINCT messages. Every one of them deliberately induces
  a failover, a restart or a redelivery, so the wait could return with a repeat
  among those N and a message still in flight — a correct at-least-once outcome
  read as a lost message. Each now waits on the quantity it asserts. Four
  sibling tests already did; intermediate progress barriers still count raw
  deliveries, which is correct for what they gate.

### Fixed — two long-running proofs that only held on an idle machine

- Running the release subset as a subset — sequentially, on a machine already
  warm — surfaced two test defects that an isolated run never reaches.
  `TestUC12_RollingRestart_NoMessageLoss` drove three competing instances and
  3,000 messages through a shared outbox on a lease profile that bounds a
  renew call at one second; a busy store misses that, three misses step the
  owner down, and every in-flight delivery is cancelled. Its rolling restart is
  graceful, so nothing waited out a TTL and the compressed profile bought it
  nothing: it now uses `lrLoadSurvivingSessionConfig`. And the separate-process
  failover assertion WAITED on the raw delivery count while ASSERTING on the
  distinct-payload count, so a failover's duplicates could satisfy the wait
  with a payload still in flight — a correct at-least-once outcome read as a
  lost message. It now waits on the quantity it asserts, and reports its
  duplicate count.

### Changed — long-running message counts now match what the suite reports

- Four use cases advertised a volume they did not exercise (UC1 said 5,000 and
  sent 1,000; UC22 said 500 per rule and sent 100; UC55 said 1,000 and sent 200;
  UC62 said 10,000 and had been reduced to 5,000). The counts and their
  descriptions now agree, and a check in `tests/docsexamples` requires the
  number a test really sends to appear in the row a reviewer reads.

### Added — a Kubernetes profile, and examples that build

- `deployment/kubernetes/` is the maintained non-AWS profile: a Dockerfile for
  the reference binary (`cmd/gobridge`) and one manifest — ConfigMap-mounted
  config, the admin key from a Secret through `GOBRIDGE_ADMIN_API_KEY`, a
  persistent volume for the SQLite state, an init container that seeds the
  durable MQTT session's baseline, and liveness/readiness probes. Its whole
  lifecycle — probes, secret path, traffic, ConfigMap reload, SIGTERM drain
  within the manifest's grace, restart on the same state — runs under Docker in
  `tests/integration` (`TestKubernetesProfile`).
- `cmd/gobridge` gained `-seed-managed-subscriptions <session-id>[=filter,...]`
  and `bridge.Builder.SeedManagedSubscriptionBaselines`: the baseline a
  persistent or exclusive MQTT session needs before it will start, seeded the
  same way the AWS profile seeds it at deploy time. Its HTTP API keys may come
  from `GOBRIDGE_ADMIN_API_KEY` / `GOBRIDGE_MONITOR_API_KEY`, so a mounted
  config never carries a secret.
- Every complete configuration example in the documentation is now built by
  the real `bridge.Builder` (`tests/docsexamples`), not only strict-decoded.
  That gate found twenty-odd examples the builder rejects — MQTT sessions no
  route managed, ephemeral `direct_hold` sources, missing DLQ and managed
  subscription stores, fan-out shapes the runtime does not implement — and each
  is corrected. Published image references must pin by digest and no page may
  describe the image as unpublished.

### Fixed — a session named on a binding is now managed under every delivery mode

- The runtime created a session manager for a session registered through a
  binding `session_id` only inside the shared-outbox wiring. A `direct_hold`
  route that named its MQTT (or AMQP 0-9-1) session that way — the very shape
  the builder recommends — got a session that never connected, a receiver that
  never subscribed, and a readiness probe that answered ready. Every registered
  session sender now gets a manager, and — because that session is the ingress
  of the routes riding on it — the same settlement barrier a route-primary
  session consults before recycling a broker connection, so a reconnect waits
  for deliveries the route already accepted.

### Added — documentation structure is now tested

- Published Markdown is checked the way code is. Every relative link resolves,
  every `#fragment` names a heading that exists (compared against GitHub's own
  slug), every table row belongs to a table, no page states a count of files the
  repository can count for itself, no page cites a Go source LINE, no governed
  page exceeds 500 lines, and no page is unreachable. The scanner reads whole
  documents rather than single lines, because a link whose text wraps is still
  one link — that blind spot was hiding two broken anchors.
- The route-policy and MQTT option tables are derived from the structs the
  parser and decoder fill, in both directions, so a new config key cannot ship
  undocumented and a documented key cannot be one nothing reads.
- `make lint` gained `scripts/lint-planning-refs.sh`: non-test Go source may not
  carry a planning-document identifier (`Chunk N`, `RECONFIG-n`, `Phase-n`,
  `Finding N`, `c13-…`, "the design doc"). Those point at worklists that were
  deleted. Use an ADR, a canonical document plus section, a live page under
  `docs/`, or plain English instead.

### Changed — documentation layout

- Eighteen oversized pages were split. `ARCHITECTURE.md`, `PLUGIN.md`,
  `TESTS.md` and `DDD.md` are now hubs with a contents table; their detail lives
  under `docs/internals/`, and the `§n` section numbers are unchanged, so an
  existing `ARCHITECTURE.md §15` reference still means the same section.
- `docs/transports/mqtt-options.md` documents `assert_stable_client_identity`,
  the flag the MQTT factory names when it refuses a persistent session with a
  hostname suffix.
- `docs/routes-and-runtime-reference.md` documents `replay_budget`: a record is
  poisoned only once BOTH the attempt count and this 15-minute wall-clock budget
  are spent, so raising `max_replay_attempts` alone changes nothing.
- The SQLite `retention` row rejoined its table — it had been sitting after a
  blockquote, rendering as a paragraph of pipe characters.

### Fixed

- A published `sessions` example in the configuration model put the MQTT options
  one level too high (`options.broker_url` instead of `options.session.broker_url`)
  and had never parsed. It now decodes through the real builder like every other
  published example.

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

- **`ports.RoleActive`, `ports.RoleStandby`, `ports.RoleStandalone`.** The role
  the bare `/api/v1/monitor/ready` body and `DeepHealth.Role` report is now an
  exported vocabulary instead of two private copies of the same strings in
  `runtime` and `ports`, so documentation can be checked against it. The wire
  values are unchanged.

- **ADR 0015 — DLQ redrive inject-then-delete.** Records the redrive the code
  has shipped since the inject-before-delete change: fresh envelope ID with a
  causation link, dedup / generated-identity / redelivery-count headers
  stripped, binding-confined, entry deleted only after a confirmed inject,
  at-least-once. ADR 0006 (claim-by-delete, at-most-once) is marked superseded;
  the OpenAPI `DLQRedriveRequest` description and the release notes still
  described it and now describe the shipped behavior.

- **A stuck-MQTT-settlement runbook.**
  [`docs/runbooks/stuck-mqtt-settlement.md`](docs/runbooks/stuck-mqtt-settlement.md)
  starts from the symptom that has no error code: a connected session, green
  readiness, no failing route, and throughput gone. It separates a slow
  downstream from a receive window at its legitimate ceiling, a wedged route
  runner, repeated recovery recycles, a recovery the broker cannot complete, and
  post-stall duplicates — and names the three destructive non-actions
  (`clean_start: true`, shortening `session_expiry_interval`, restarting to
  unstick it) that each turn a stall into silent loss.

- **Fleet convergence signals for the coordinated cluster rollout, and alarms to
  match.** The barrier is atomic BEFORE the store Commit and per-member AFTER it,
  so a member whose local swap fails runs an older generation than its peers — and
  every signal that described the shared rollout row read "committed" identically
  on both. Deep health's `config_watch.rollout` now also carries `converged` (who
  the confirm barrier still waits for) and `observed_at` beside the existing
  `applied` / `observation_age_ms` / `stale`, and a rollout that is decided but
  not applied, observed stale, or terminal now sets `config_watch.degraded` with
  the reason. A new `ClusterRolloutDiverged` gauge joins `ClusterRolloutTerminal`
  and `ClusterRolloutObservationAge` in `DefaultRollupMetrics()`, and
  `gobridgealarms.AlarmsProps.EnableClusterRolloutAlarms` installs a fleet-maximum
  alarm on each. See [ADR 0013](docs/adr/0013-coordinated-cluster-config-rollout.md),
  whose guarantee is now stated as written rather than as "no mixed-version
  cohort".

- **`empty` in deep health, and an empty bridge no longer claims to be ready.**
  `GET /api/v1/monitor/deephealth` now reports `"empty": true` for an instance
  that carries no routes and no sessions, and such an instance is capped at the
  `running` readiness level with `ready_for_traffic: false`. Every "all routes
  are ready" test is trivially satisfied when there are no routes, so a bridge
  started from a missing or route-less configuration used to report full
  readiness while not one message could pass through it — a load balancer would
  steer traffic at it and a rollout gate would count it as converged. It stays
  alive (`/live` is still 200), so nothing restarts a process that is merely
  waiting for its configuration.

- **`-start-empty` flag on the reference binary.** `cmd/gobridge -start-empty=false`
  turns a missing `-config` file back into a fatal startup error, for
  deployments where carrying zero routes is never correct. The default stays
  `true`.

- **`restart_required` in the deep-health `config_watch` projection.** Names a
  part of the desired configuration that has been accepted and durably stored
  but cannot be applied to the running process. Today that is the `http` block:
  the admin and monitor listeners are bound once, at startup, so a reloaded
  address, TLS pair or CORS origin was accepted and then silently ignored.

- **`httpapi.Config.TerminalProvider`.** Lets a composition root feed its own
  terminal state to the liveness probe, independently of any runtime the server
  can see.

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

- **`MQTTConnectFailures`** (tagged `session_id` and the bounded error `code`)
  and **`MQTTSessionResumeLost`** (tagged `session_id`) metrics, and
  [MQTT settlement recovery](docs/transports/mqtt-settlement-recovery.md) as its
  own page, split out of the MQTT behaviour reference.

### Changed

- **Public runtime contracts corrected and derived-checked.** `ARCHITECTURE.md`
  lists the real `DLQReader` / `DLQAdmin` / `DLQStore` port (no `Replay`), both
  admin and monitor endpoint tables now match `spec/httpapi/http-api.yaml`
  (the `/dlq/replay` and `/logs` rows named endpoints nothing registers), and
  its cluster section describes the `refuse` / `independent` / `coordinated`
  rollout ladder instead of denying that a barrier exists. The bare `/ready`
  probe is documented as requiring the `full` level, with a `standby` answering
  503 on it by design; the node-down runbook says `active`, not `leader`.
  `sessions[].transport` lists the stateful kinds (`mqtt`, `amqp091`,
  `amqp10`) rather than `sqs`/`servicebus`/`http`. Every shutdown example shows
  a 45 s process budget above a 20–30 s drain, and the health page explains why
  the `30s`/`30s` defaults leave no headroom. `node_role` is documented as the
  config-transaction single-writer selector it is. Each correction has a test
  beside its source of truth.

- **`docs/aws-deployment/monitoring.md` is split into three pages, and its metric
  catalogue and alarm inventory are now derived from source.** The page claimed
  the CDK bundle "currently creates only `OutboxDepth`, `DLQEntries`,
  `LeaseExpiries`, and `LeaseAcquireFailures`" while the bundle synthesizes 38
  alarms, and its metric tables omitted 17 series the runtime emits — among them
  every cluster-rollout signal, the config-degraded gauge, the DLQ intake hold,
  the route-owner-unknown counter and the post-reload strand counter. Monitoring
  keeps the exporter and the complete catalogue (and the `#key-metrics` anchor);
  [CloudWatch alarms](docs/aws-deployment/alarms.md) carries the full inventory
  with each alarm's statistic, threshold, missing-data treatment and provisioning
  shape, the rollup metrics every built-in alarm depends on, the DLQ-depth
  sampler prerequisite, and the signals nothing provisions;
  [Logging, dashboards and tracing](docs/aws-deployment/logging-and-dashboards.md)
  carries the rest. Tests parse the pages and compare them against the metric
  constants and the synthesized CloudFormation template, in both directions.

- **`docs/transports/mqtt-options.md` states where each ingress cap is enforced
  and what a violating packet costs.** Sizing memory from `max_payload_bytes`
  alone understates the peak: a packet that breaches a local representational cap
  is decoded in full before the callback refuses it. The three boundaries — the
  broker's whole-packet limit, the raw-bytes predecode guard, and the decoded
  callback — are now a table, and the published property caps and default byte
  bound are derived from the constants by a test.

- **`docs/aws-deployment/overview.md` is split into a hub plus six pages.** The
  overview kept growing past the point where it could be reviewed or navigated. It
  is now the architecture plus a page map, with
  [Deployment Topologies](docs/aws-deployment/topologies.md),
  [Compute and Runtime Metrics](docs/aws-deployment/compute.md),
  [Storage and Secrets](docs/aws-deployment/storage-and-secrets.md),
  [Container Image](docs/aws-deployment/container-image.md),
  [CDK Construct Library](docs/aws-deployment/cdk-constructs.md) and
  [IAM Least Privilege](docs/aws-deployment/iam.md) beside it. No content was
  dropped; every inbound link and anchor was repointed. Bookmarks to
  `overview.md` itself still resolve.

### Upgrading

- **Coordinated cohorts running v0.3.6 or earlier: the durable committed-config
  artifact written by those releases cannot be read back.** Its JSON stored every
  duration (`5s`, `1m`) as a bare number, which the config parser refuses. A
  member only decodes that record when it restarts on a config other than the
  committed one, so a cohort can carry a bad record for a long time and then hit
  it during an unrelated restart, with `the durable last-committed config
  artifact ... could not be decoded ... refusing to start`.

  The repair is forward-only and needs no action in the normal case: every
  commit rewrites the record, so upgrade the cohort one member at a time — with
  its config source holding the document the cohort last committed — and the
  first config change you roll out afterwards clears it. A cohort that is
  entirely down when it is discovered needs the record deleted by hand first —
  see [Cluster config rollout](docs/runbooks/cluster-config-rollout.md#after-upgrading-a-member-will-not-start-because-the-committed-config-cannot-be-decoded).

  A cohort first deployed on this release is unaffected, as are standalone,
  refuse-mode and independent-mode deployments.

### Fixed

- **Four shipped CloudWatch alarms could never fire.** The `GoBridgeAlarms` CDK
  bundle provisions dimensionless alarms on `MQTTIngressPoisonDropped`,
  `ReconcileFailures`, `MQTTSessionTakeover` and `MQTTQoSDowngraded`, but the
  runtime emits all four tagged with `session_id` and none of them was in
  `DefaultRollupMetrics()`. A zero-dimension alarm never matches a dimensioned
  series, so every one of them sat at `INSUFFICIENT_DATA` permanently — an
  acked-and-dropped poison packet, a `client_id` collision, a broker QoS cap and
  a subscription reconcile that never converges were all provisioned, visible in
  the console, and incapable of alerting. The four metrics are now rolled up.
  Two tests keep the two halves in agreement across modules that cannot import
  each other: the CDK suite asserts every dimensionless alarm it synthesizes has
  a rollup copy listed in the published table, and the exporter suite asserts
  `DefaultRollupMetrics()` matches that same table.

- **An MQTT publisher can no longer make the bridge decode tens of thousands of
  User Properties per packet.** The CONNECT advertises only a whole-packet
  Maximum Packet Size, so a compliant broker forwards a zero-payload packet at
  that limit whose metadata is nothing but five-byte User Properties — about
  78,600 of them at the default `max_payload_bytes`. The publish callback refused
  such a packet, but only after the SDK had decoded every property, and the SDK
  spends roughly 1.3 KiB per property doing so: around 100 MiB of allocation
  for one packet, against a memory model that budgeted 1.7 MiB for it. The
  predecode ingress guard now cuts the User Property list to 129 entries — one
  above the retained cap — on the raw bytes, before the SDK sees the packet.
  The callback still acks-and-drops it (`MQTTIngressPoisonDropped`), the decode
  costs no more than an ordinary packet, and each truncated packet is counted on
  the new `MQTTIngressUserPropertiesTruncated` metric with the wire count logged
  at Debug. The ingress memory model's crossing slot now budgets the SDK's real
  decode buffers (four wire-sized allocations plus the 129 properties) instead
  of a property count the guard no longer lets through, which lowers the
  default-profile bound from 265,797,600 to 265,185,360 bytes; no configuration
  that validated before fails now. The finite-cgroup proof
  `TestMQTTIngressMemoryPropertyFlood` drives a flood of such packets through a
  real broker under a 512 MiB limit.

- **A cluster rollout member that cannot apply the committed generation no longer
  stays behind the cohort forever.** After three failed swap attempts the member
  gave up permanently: it recorded the generation as applied in its own gate,
  stopped retrying, and ran the previous config until someone noticed. Every cause
  that outlasts three fast attempts and then clears — a broker down for ten
  minutes, a store throttling a burst — therefore produced a permanently split
  cohort. The retry is now bounded in RATE rather than in total: unpaced for the
  first attempts, then capped exponential backoff, and it keeps going, so a
  member converges on its own when the cause clears. At the bound it declares
  itself terminal with a reason naming replacement as the repair, which reaches
  `terminal_generation` in deep health and the `ClusterRolloutTerminal` alarm.

- **A cluster rollout member that converged through the durable committed
  artifact no longer reports itself diverged.** `applied` was answered from the
  staged candidate, so a member that was down when its config source delivered the
  change — and therefore converged by decoding the artifact, which it never stages
  — reported `applied: false` for as long as the committed row stayed current, as
  did every member for one poll after a restart. With the divergence alarm added
  above that would have paged on a healthy cohort. It is now answered by comparing
  the running config's canonical digest against the one the cohort agreed on,
  which carries no process history. The same member is also no longer blocked from
  converging at all when a committed row is current: the reconcile fallback was
  suppressed for the whole observation whenever the row was decided, rather than
  only when an apply had actually been attempted — while a member owing BOTH the
  active row's generation and an older durable artifact could alternate between
  the two paths, resetting its own backoff and rebuilding the runtime every poll.

- **A cluster rollout member's terminal latch now tracks which repair set it.**
  The three local repairs — reaching the decided generation, recording the durable
  artifact, reverting a provisional one — shared one latch, so a completed revert
  retracted an artifact latch it had not fixed (silencing the only signal that
  names a member which would boot the wrong config), while an apply that genuinely
  ended a revert latch left it standing. A member whose revert failed and whose
  generation the cohort then CONFIRMED kept reporting "replace this member" and
  never recorded the artifact for the config it was running. A member whose swap
  went terminal also suppressed its own confirm-window deadman, so it went on
  chasing a generation the cohort had abandoned — and would have joined it if the
  cause cleared.

- **A rollout voter outside the frozen membership epoch now logs why it
  abstained.** It named neither itself nor the roster, so a rollout that
  deadline-aborted because one member could not vote left no local cause on the
  one node an operator logs into.

- **The HA deployment fingerprint no longer rejects every real config change.**
  The `dynamodb_coordinated_ha` profile stamped a hash of the WHOLE logical config
  at synth and required an exact match at runtime. Any change an operator actually
  committed — a new route, an edited binding — therefore failed deployment
  admission on every member, after the cohort had already agreed to run it: a
  committed generation nobody could apply. The stamped value is now
  `bridge.DeploymentProfileFingerprint`, a hash of only the fields the deployment
  provisions (`deployment_mode`, the `bridge.cluster` shape, and the durable
  identity of every deployment-owned store: lease, outbox, DLQ, managed
  subscriptions). Operator content
  — routes, receivers, senders, sessions, processors — is out of it by design: it
  is what a coordinated rollout exists to change. Changing an existing durable
  session identity or an exclusive route's `session_id` is still refused, by the
  live-reload preflight that owns that rule.

  The same check now runs before a member VOTES on a candidate as well as before
  it applies one, and it runs before the reload seam proposes anything, so a
  config this deployment forbids is Nacked instead of committed cohort-wide and
  then refused by everyone.

- **A coordinated cohort now has a durable baseline from its first boot.** Before
  the first rollout committed there was no committed-config artifact, so boot
  resolution fell back to whatever the member's own config source held — and the
  operator's change is durably written to that source BEFORE the barrier decides
  on it, so a member restarting in that window booted a candidate no peer was
  running. The deployment now stamps the digest of the document it seeds
  (`dynamodb_ha_baseline_config_digest`), and a member booting on exactly that
  document records and verifies it as the cohort's generation-zero committed
  artifact before it serves. The write is monotonic: every member seeding the same
  baseline is a no-op, a committed generation is never rewound, and a DIFFERENT
  config at generation zero fails startup closed rather than overwriting what the
  cohort recovers to. Deep health gains `baseline_generation` and
  `baseline_digest` under `config_watch.rollout`, and the seed decision is
  audited as `cluster_rollout_baseline_seed`.

- **The production rollout voter now runs blueprint validation.** The shipped
  file-based composition root never wired `bridge.WithBlueprintValidator`, so the
  paths that build a config the config manager never emitted — the vote's
  candidate and the bytes decoded from the durable committed artifact — reached
  the builder unvalidated. A dangling reference in a candidate now Nacks at the
  vote instead of failing every member after commit.

- **A black-holed rollout store can no longer wedge the coordinated cluster
  rollout drive.** Every rollout store, coordinator-lease and committed-artifact
  call now runs under its own budget (`bridge.ClusterRolloutConfig.StoreCallTimeout`,
  5 s by default), and a call that ignores its context is abandoned rather than
  owning the single drive goroutine — a deadline expires the context, it does not
  unblock the call. While an abandoned call has not returned the barrier starts no
  new one, so a store that has stopped answering costs exactly one parked
  goroutine instead of one per poll. With the drive ticking again, three things
  that a black hole used to suppress keep working: the local confirm-window
  deadman (which reverts a member to its last confirmed generation and now runs
  off a cached deadline, before the observation, rather than behind it), the
  freshness of the published status, and process shutdown. Orderly coordinator
  resignation is bounded by `min(lease_ttl, 5s)` instead of the whole lease TTL,
  so a slow release cannot spend the shutdown budget on a courtesy whose fallback
  — TTL expiry — is the always-safe crash path.

- **Two rollout safety operations were latched as done whether or not they
  worked.** Recording the durable last-committed artifact and reverting a
  provisional generation were one-shot and best-effort, and both set their "done"
  latch regardless of outcome: one cancelled write left a member that reboots
  onto an older generation than the cohort runs, and one failed swap left a
  member serving the config the cohort rejected — with its own repair suppressed.
  Both are now retryable state, attempted under a bounded exponential backoff and
  latched only after verification: the artifact is read back (the store's
  monotonicity rule makes a stale write a no-op SUCCESS, so "the write returned
  nil" and "the artifact holds this generation" are different statements), and
  the revert is confirmed against the running config. A member that exhausts the
  bound says so — `rollout.terminal_generation` in deep health, the
  `ClusterRolloutTerminal` gauge, and a degraded latch — and the reason names the
  right repair: replace a member that cannot revert (it is running rejected
  config), repair the store for a member that cannot record the artifact (it is
  running the correct config, so replacing it is what would boot it stale, and it
  keeps retrying).

- **Deep health reports how old the rollout observation is.** Every rollout field
  is a projection of this member's last read of the shared row, and a member that
  cannot read it kept publishing that snapshot with nothing to say so. The block
  now carries `observation_age_ms`, `stale`, `last_error` and
  `artifact_generation`, and `applied` — declared but never populated — is filled
  in. New metrics: `ClusterRolloutStoreCalls` (by call class and outcome),
  `ClusterRolloutObservationAge`, `ClusterRolloutRetries` and
  `ClusterRolloutTerminal`.

- **MQTT session edge recovery: four states the bridge reported but had not
  verified.** An orphan `UNSUBSCRIBE` answered `0x11` ("no subscription
  existed") was logged as a successful cleanup, so a **wildcard** orphan — which
  MQTT cannot unsubscribe by a topic it delivered — survived silently. It is now
  reported: the Debug convergence line is kept for a real removal (`0x00`), and
  `0x11` warns, naming what does converge it (managed subscriptions, whose exact
  durable history reconcile unsubscribes, or broker administration). See
  ADR-0003.

- **A broker QoS cap no longer churns forever.** A SUBACK below the requested
  QoS failed every reconcile identically, so the supervisor restarted the
  session into the same verdict at its backoff cap — and an exclusive owner
  released and re-seized its lease on every cycle, resetting each standby's
  observation window. The same `(filter, requested, granted)` grant confirmed on
  three consecutive reconciles is now treated as permanent: the error wraps the
  permanent-closure marker, so the session fails terminally instead of retrying
  a configuration only a human can fix. A single weak SUBACK stays retryable,
  and any reconcile that converges clears the count.

- **A refused QoS 1/2 delivery on an ephemeral session no longer pins the
  receive window.** The receiver asked whether an acknowledgement callback
  existed, not whether the session could RESUME. An ephemeral session dials
  `clean_start=true`, so no recycle can ever redeliver it — withholding the
  acknowledgement bought no recovery and pinned a broker Receive-Maximum slot
  for the life of the connection. It now takes the policy QoS 0 already had:
  ack, drop, and count `MQTTReceiverEmitRejected{outcome=lost}`.

- **A reconnect that lost the durable session is now visible.** A persistent or
  exclusive session dials `clean_start=false` because it wants the broker to
  resume its subscriptions and its queued offline QoS 1/2 backlog. When CONNACK
  answered `Session Present=false` the bridge re-subscribed and reported itself
  fully healthy, so a failover that dropped the whole backlog left no trace. It
  now counts `MQTTSessionResumeLost`, warns, and latches the loss on the
  session's `LastError` until the next converged reconcile. A cold start is
  exempt; a non-empty managed subscription history makes even a first connect
  answerable, which is the exclusive standby connecting after
  `session_expiry_interval`.

- **A failed reconnect names its cause.** MQTT authenticates only at CONNECT and
  autopaho then retries on its own goroutine, so a rejected CONNECT was the one
  place the reason a session could not come back was ever visible — and it was
  discarded. The mapped cause is now latched on the session's `LastError` until
  a connection comes up, warned once per attempt, and counted on
  `MQTTConnectFailures` tagged with the bounded error `code` (the broker URL is
  deliberately not a dimension). `runtime/session` also stopped dropping the
  error when logging `session reconnecting`.

- **Session-failure lease hand-off reuses the settlement grace.** Step-down
  waits `step_down_grace` before releasing the lease so destination sends this
  owner already accepted can settle before the next owner advances the fence;
  the session-failure path closed the source and released immediately. Closing
  the source fences ingress, not egress, so it widened the accepted-duplicate
  window the grace exists to close. Both paths now share one bounded wait, and
  both skip it on the same evidence — a destination drainer that reports idle.

- **Two flaky tests, one cause: `clocktest` swallowed ticks the test had already
  advanced past.** `clocktest.Ticker.Reset` drained the tick channel, mirroring
  `time.Ticker`. Both shipped re-pacing loops — the SQS auto-extend loop and the
  Service Bus session-lock renewer — call `Reset` at the END of the handler they
  are running, so a test advancing the clock from its own goroutine could land
  mid-handler: the tick was fired and buffered, and the handler's `Reset` then
  threw it away. Real time would have delivered another tick one period later;
  a fake clock never does, so the loop blocked forever and the test failed on
  its wait deadline — but only when it lost the race, which is why it showed up
  as an occasional red build rather than a broken test. `Reset` now keeps a tick
  already delivered and re-paces only what follows. `Reset` also re-registers a
  timer or ticker that `Advance` retired after `Stop`, which was otherwise a
  re-armed deadline no `Advance` would ever cross. Timer semantics are
  unchanged. See `TESTS.md` §2.2.

- **`runtime.Gosched()` spin-polling removed from every test.** Nineteen waits
  across fourteen files polled with `Gosched` instead of `time.Sleep`, which
  passed the timing audit while defeating its purpose: the waiter stays runnable
  for the whole wait, holding a CPU and competing with the goroutine it is
  waiting for, which under `-race` on a loaded machine is enough to fail a
  correct test. Two had no deadline at all, so a stuck goroutine became a
  package timeout that killed every unrelated test in the binary. All now use
  `testutil/wait`, and `make audit-test-timings` fails on `Gosched` in tests.

- **A DynamoDB config test proved nothing.** It asserted "no emission" after a
  single scheduler yield, so it passed whether or not the loader emitted. It now
  holds the channel silent for a window with `wait.Silent`.

- **`/live` was blind to a wedged reference binary for ~15 seconds.** When a
  reconfiguration swap and its recovery both fail, the process holds no active
  runtime and routes nothing — which looks exactly like a healthy swap window to
  a probe that can only inspect the runtime. `cmd/gobridge` now reports the
  supervisor's terminal state to the HTTP server directly, so `/live` fails
  closed at once instead of waiting out the coarse background backstop
  (3 confirmations × 5 s) that was the only thing covering the gap.

- **Monitor and admin endpoints answered from the long-stopped boot runtime.**
  When a configured `RuntimeProvider` reported no runtime, `httpapi` silently
  fell back to the runtime passed to `New()`. In a composition root that is the
  boot runtime, stopped since the first reconfiguration — so `/topology` served
  boot routes, the DLQ endpoints ran against a closed store, and `/live`
  answered 200 because a stopped runtime is not a terminal one. A configured
  provider is now authoritative and its nil means 503.

- **The admin write deadline was shorter than a config commit could take.** The
  admin listener's `WriteTimeout` was derived from `AdminOperationTimeout`
  alone (45 s by default), while a commit can legitimately run its detached
  apply for 60 s and then a rollback restore for another 60 s before answering.
  The connection was reset while the server was still deciding, leaving
  automation to retry against a state it could not observe. The deadline is now
  derived from the longest response path the server can actually serve; a server
  without the config-transaction endpoints keeps the tighter deadline.

- **The process shutdown budget ignored reloads.** The HTTP drain and the final
  supervisor wait read `bridge.shutdown_timeout` from the boot configuration, so
  an operator who raised it through a reload kept the old, shorter budget until
  the next restart. Both now read the running configuration, falling back to the
  boot value only when no configuration is active.

- **Start-empty advertised an admin API it does not serve.** The missing-config
  warning told operators to "push a config through the admin config API", but
  the start-empty configuration defines no `http` block and this composition
  root binds its listeners once at startup — so there was no admin API and no
  probe port to recover through. The warning now states what actually works:
  create the file to converge the routes, restart to bring up the listeners.

- **SIGTERM killed in-flight deliveries before the drain.** Both shipped
  binaries cancel their process context on `SIGTERM` and only then stop the
  runtime, and every route, receiver, sender and delivery context was derived
  from that same context — so the cancel reached in-flight sends first and
  `Runtime.Stop`'s "settle accepted deliveries before cancelling" phase never
  ran. Every rolling restart aborted work mid-send: duplicates on redelivery,
  and losses under `on_permanent_failure: drop`. Routes now run on a context
  detached from the caller's; `Stop` is the only thing that cancels them, and
  cancelling the start context drives a `Stop` under the configured budget
  instead of an abort. The builder also passes `bridge.drain_timeout` into the
  runtime, so the runtime and the supervisor bound teardown identically instead
  of the runtime falling back to an internal 5s ceiling.

- **A second `Stop` reported a clean shutdown over a failed one.** Two callers
  race on every signal (the start-context watcher and the composition root's own
  stop). The loser returned `nil`, so `cmd/gobridge` logged "bridge stopped" and
  the file-based app returned success over a drain that had actually failed.
  Every `Stop` now returns the result of the teardown that ran. `Stop` errors
  also name the phase that failed — background components, a session manager, a
  named unmanaged session, a role-tagged store handle, or the metrics flush.
  **Operational consequence:** `gobridge-filebased` now exits `1` when a
  shutdown's drain genuinely failed (overran its budget, or a transport refused
  to close) where it previously exited `0`.

- **A committed config installed a runtime and then stopped it.** The file-based
  app applies an admin config commit in-band on the context the httpapi
  transaction detaches from the request but still cancels when `Commit` returns.
  The new runtime was started on it, so its start-context watcher stopped the
  freshly installed runtime moments later and the process was left
  installed-but-stopped — `/live` still 200, because a clean stop is not
  terminal — until an unrelated config arrived. Runtimes are now started under
  the process-scoped context; only shutdown ends them.

- **A failed old-runtime `Stop` left a torn-down runtime installed and serving
  nothing.** `Runtime.Stop` has no early error return, so a reported failure
  arrives after the work context is cancelled and managers, sessions and stores
  are closed — and a stopped runtime is single-use. Both swap paths kept it as
  the current runtime behind a green `/live`, and in the file-based app the
  applied fingerprint still named its config, so the admin transaction's disk
  rollback was recognised as already-applied and skipped: nothing ever
  recovered. Both now wedge (`Terminal()` true, `/live` 503) so the orchestrator
  restarts the task with freshly-built transports; the file-based app also
  releases the never-committed build plan's store handles and clears the
  fingerprint.

- **`/bridge/start` could orphan a runtime that had tripped terminal.** The gate
  read `IsRunning()` (running **and** healthy), and a component-failure trip
  flips healthy while leaving the runtime flagged running and closing nothing.
  A resume therefore published a fresh runtime beside a live one still holding
  its broker sessions, store handles and leases — for the process lifetime,
  since the terminal backstop then read the new runtime. The previous runtime is
  now stopped before a replacement is built.

- **A slow drain made prepare/commit reloads fail deterministically.** The swap
  phase deadline was armed before the old runtime's `Stop`, which may consume
  the whole `drain_timeout`, so construction inherited a spent context and every
  retry failed the same way. Construction now gets its own deadline, derived
  after the drain returns.

- **Process shutdown could stall behind an unbounded wait.** The config-watcher
  join and the coordinated-rollout drive stop were unbounded, and the drive
  resigns its coordinator lease on the way out under a context bounded only by
  `lease_ttl` — so a stuck reload or a lease store that would not release held
  `SIGTERM` ahead of the runtime drain, the HTTP shutdown and the metrics flush
  until the platform's SIGKILL. Both waits are now bounded by the process
  shutdown budget — and in the file-based app that budget is now
  `bridge.shutdown_timeout` from the boot config rather than an invisible 30s
  constant, so the field documented as the total shutdown grace period actually
  governs it. An explicit `bootstrap.WithShutdownTimeout` still wins.

- **A failed startup left a runtime running with nobody to stop it.** The
  file-based app installs the runtime before it opens the transport, admin and
  monitor listeners and before the config watcher, and every failure after that
  point returned an error while the runtime kept its sessions, stores and lease
  renewals live. It is now released under a bounded, detached context.

- **A rejected reload changed live logging.** `bridge.log_level` was applied at
  the top of the reload path, before deployment-profile validation and before
  anything was built, so a rejected candidate changed process verbosity while
  desired and running state stayed on the old config. The level is now committed
  with the runtime.

- **Sessions were disconnected with an expired build context.** When a build
  failed, receivers and senders were closed under a detached bounded budget but
  sessions were closed with the original — routinely already-expired — build
  context, so brokers refused the disconnect and still held the client id when
  the recovery build reconnected with the same identity. Sessions now use the
  same detached teardown.

- **The initial supervisor build was unbounded.** Every reload build ran under
  the swap deadline; the initial one did not, so a composition root without its
  own outer wait blocked in `Run` forever on a hung construction call, with no
  runtime, no health surface and no terminal signal.

- **A session failure could leave a dead owner holding the lease for a full
  TTL.** On the default `connect_after_lease` profile, a reconnect-reconcile
  failure (or a dead transport event stream) closed the source, released the
  lease and restarted the session — and the restarted session re-seized the
  lease through the store's same-owner path before discovering that the
  single-use MQTT transport refuses to start after close. That terminal failure
  kept the freshly re-seized lease, so takeover waited out `lease_ttl` (45s in
  the HA preset, 360s by default) plus a poll instead of one acquire poll.
  Nothing has been accepted on a failed deferred connect — no subscription, no
  delivery, no unsettled work — so the lease is now released before the process
  goes terminal. The reconcile / managed-migration phase still retains the lease:
  there, durable route work may still unwind.

- **Lease timings could silently collapse to a millisecond store storm.**
  `lease_ttl: 5s` with `max_renew_fails: 5`, or `lease_ttl: 45s` with
  `max_renew_fails: 50`, left no per-attempt renew budget, so construction
  clamped the derived renew interval and the standby acquire poll to 1 ms and
  only logged a warning. Validation looked at an explicitly pinned
  `renew_interval` only, so the derived path — the production path — passed.
  The cadence resolution now lives in the domain (`routing.LeaseTimingRequest`),
  and the session manager, the builder and the **blueprint validator** all
  resolve through it, so an unserveable cadence is rejected before the config
  transaction's durable write instead of at apply. An exclusive session is
  refused when the expiry-margin clamp had to cut the renew interval or the
  per-call timeout, or when the resolved renew interval or acquire poll falls
  below a documented `250ms` floor. A clamp that only sheds `lease_renew_jitter`
  is still accepted — the clamp trims jitter first by design and the remaining
  cadence is healthy.

- **`Manager.Close` released the lease before closing the source session.** A
  standby could seize the partition and activate while this node stayed
  connected and subscribed for the whole duration of `session.Close`. MQTT hides
  that behind client-ID takeover; an exclusive AMQP 0-9-1 / 1.0 consumer really
  does double-consume. Close now follows the same close-then-release discipline
  as step-down, activation failure and session-failure recovery, under the same
  bounded, detached context — and skips the release entirely when the source
  Close ignored its context and never returned. The close is bounded by the
  caller's remaining deadline rather than by a budget of its own, so tearing down
  many managed sessions can no longer overrun `bridge.shutdown_timeout`.

- **A terminal deferred connect could hand off the lease before its source had
  quiesced.** Releasing on a permanent-closure marker assumes nothing of ours can
  still send, but a session can latch that marker asynchronously — the MQTT
  ingress-poison rejection returns immediately and quiesces on a goroutine — so
  the marker alone was not evidence. The source is now closed under the bounded
  teardown before the lease is released, and a close that never returns keeps the
  lease until natural expiry.

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
