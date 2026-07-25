# PROD_READY_ISSUES — MQTT and Core Production Re-Audit

**Date:** 2026-07-19
**Target:** `3b5c78a8b97d` on `fix/mqtt-prod-ready-remediation`
**Supersedes:** the 2026-07-17 contents of this file. That review drove substantial remediation; this report audits the remediated current HEAD rather than repeating the old verdict.

**Scope:** all production source under `adapters/mqtt/transport/paho`; the MQTT-facing paths through `bridge`, `config`, `validate`, `runtime`, `runtime/session`, `runtime/route`, `runtime/outbox`, `circuitbreaker`, and `ports`; the shipped AWS file-based composition root; module/release/container artifacts; MQTT documentation, ADRs, scenarios, runbooks, integration tests, and long-running tests.

**Method:** four independent adversarial tracks covered MQTT source, runtime/cluster behavior, documentation/deployment, and test evidence. Cross-cutting findings were challenged by a second reviewer. Retained findings were then checked directly against current source. `CONFIRMED` means a complete reachable failure sequence was traced. `PLAUSIBLE` means the code permits the sequence but a required dependency behavior was not reproduced.

Severity follows `LANGUAGE.md`: `BLOCKER | HIGH | MEDIUM | LOW`.

**Assessment boundary:** production readiness is scored from runtime correctness,
delivery guarantees, resilience, operability, cluster behavior, and documentation
accuracy. Expected pre-release work such as cutting module tags and publishing the
image is **not** evidence for or against production readiness. Release state is
recorded separately only because consumability was part of the requested review.

---

## 0. Executive verdict

**No: GoBridge MQTT does not yet meet the requested production runtime and HA requirements at this revision.**

The MQTT adapter itself is strong: manual acknowledgement, bounded broker calls, authoritative subscription reconciliation, broker-session recovery, poison escape, ingress memory admission, and explicit loss/duplicate metrics are unusually well engineered. Most adapter defects from the previous review are fixed and regression-pinned.

The production-readiness verdict remains negative for concrete reasons:

1. **A count-less MQTT poison message can evade `max_replay_attempts` forever.** Fresh envelope IDs on each broker redelivery reset the runtime replay ledger.
2. **The shipped AWS bootstrap does not provide truthful reload convergence.** A valid-but-broker-rejected config can replace a healthy runtime and be acknowledged as applied without the generic Supervisor's `ConfigDegraded` convergence watch.
3. **One clustered configuration spelling bypasses MQTT replica-safety validation.** Static `cluster.endpoints` activates clustered runtime behavior without activating the validator's shared-subscription and identity checks.
4. **The shipped bootstrap leaks a newly built runtime when late `Start` fails.**
5. **Exclusive failover does not react to an active node's broker-only outage.** The node keeps renewing its lease while MQTT reconnects forever; a healthy standby cannot take over.
6. **The default exclusive failover budget is about 336.5 seconds, not 30–60 seconds.** A 49-second computed lease-loss profile is possible only with explicit tuning and does not cover broker-path failure.
7. **Cluster-wide live reconfiguration is deliberately unsupported.** Per-process clustered reload is refused; safe replacement is an external whole-cohort procedure, not a runtime guarantee.

`make lint` and `make test` are green. That establishes build, static-check, unit, race, and timing-audit health; it does not prove production readiness or zero bugs. The strongest process-failover, chaos, and leak tests are scheduled/manual, use compressed timings, or only log observations.

### Direct answers

| Question | Answer |
|---|---|
| Is the code production ready? | **No overall.** The MQTT adapter is conditionally production-grade, but the cross-scope replay defect, bootstrap apply defects, cluster validation mismatch, and unresolved deployment constraints block an unconditional claim. |
| Is the documentation production ready? | **No.** The MQTT reference is strong, but it overstates the count-less `direct_hold` retry cap, several operator states lack matching runbook/alarm coverage, and pre-release image instructions are incorrectly written as already published. |
| Do we have zero bugs? | **No.** Zero bugs cannot be proven, and this audit confirms reachable defects. |
| Can this run in a cluster? | **Conditionally.** Ephemeral `$share` scale-out works with unique replica identities. Exclusive active/standby works with a distributed lease/store backend; DynamoDB is the only shipped production backend. Static-endpoint clustering currently has a validation hole. |
| Is it resilient to outages and can it recover? | **Partially.** Broker reconnect, subscription reconciliation, config-source retry, and lease fail-closed behavior are strong. Broker-only failure on the active owner does not trigger standby takeover; non-converging AWS applies do not roll back; hostile context-ignoring plugins can leak goroutines. |
| Do messages get lost, and is loss reported? | **QoS 1/2 durable ingress is at-least-once, not exactly-once.** Most deliberate loss is documented and metered. QoS 0, ephemeral reload windows, broker queue expiry, poison rejection, volatile raw egress, and unstable deployment identity remain loss cases. One close-time QoS 0 drop path is unmetered. |
| Is it easy to consume as a standard process in Docker/Kubernetes/ECS? | **Conditional from source.** The image builds and runs locally as non-root and the ECS composition is concrete. The shipped process is AWS-bound; non-AWS users need a custom composition root. Publishing tags/image at release is expected release work and is not scored as a production-readiness defect. |
| Can a single instance be reconfigured safely while running? | **Generic Supervisor: conditional. Shipped AWS process: no production claim.** Reload is a stop/rebuild/start window, not hitless. The generic path later reports non-convergence; the AWS bootstrap can report false success and leak failed candidates. |
| Can the cluster be reconfigured safely while running? | **No in-process cluster transaction exists.** Clustered live reload is refused. The documented safe path is an externally coordinated whole-cohort replacement. |
| Does cluster failover complete in configurable 30–60 seconds? | **Only for a tuned lease/owner-loss case, not generally.** The computed tuned example is 49 seconds. Defaults are about 336.5 seconds. Broker-only active-node failure does not fail over, and no production-like test asserts a 30–60 second objective. |

### Production decision

**Decision: NO-GO for production deployment based only on the runtime, HA, and operability findings below.**

Minimum production-readiness gates:

1. Fix MQTT replay identity/capping for count-less producer traffic.
2. Make the shipped bootstrap use the Supervisor's preflight, cleanup, and convergence semantics, or remove live-reload claims from that process.
3. Unify clustered detection in validation and runtime composition.
4. Define and implement broker-health-driven lease step-down if broker-path failover is part of the product SLO.
5. Reject unsafe Persistent+hostname deployment identity or require an explicit stable-host assertion.
6. Add a PR-gated, separate-process, real-broker failover proof with an asserted configured SLO.
7. Close the known goroutine-growth baseline before claiming lifecycle stability.

---

## 0.5 Remediation status (branch `fix/mqtt-prod-ready-remediation`)

All **confirmed production-code findings** below are now **✅ FIXED** with regression
tests; `make lint` and `make test` are green. Fixes were adversarially re-reviewed
by three independent reviewers; two regressions surfaced by that review
(STORE-1×MQTT-CORE-1 poison-on-store-timeout, and a RECONFIG-1 shutdown WaitGroup
race) were fixed and pinned.

| Finding | Status | Resolution |
|---|---|---|
| MQTT-CORE-1 | ✅ FIXED | Adapter marks adapter-generated identities (`x-bridge.generated-id`, internal-only); runtime terminally DLQ/drops an uncountable redelivery (`unstable_identity`) instead of recycling the session forever. Doc + tests. |
| RECONFIG-1 | ✅ FIXED | Post-swap convergence watch in the AWS bootstrap: polls `ReadinessLevel`, latches applied-but-not-converged (`ConfigDegraded`=1 + deep-health reason) if it does not reach `LevelSubscribed` within budget. |
| RECONFIG-2 | ✅ FIXED | Every failed/uninstalled candidate runtime is stopped on all paths; prepare/commit aborts if the old runtime does not stop cleanly (no commit under uncertain ownership). |
| CLUSTER-1 | ✅ FIXED | One canonical `ports.IsClusteredDeployment` predicate; static-endpoints now triggers the same clustered replica-safety validation. Test pins both spellings. |
| CLUSTER-2 | ✅ FIXED | Opt-in `broker_health_step_down`: an active exclusive owner whose broker path stays non-converged past the threshold steps down so a standby takes over (`BrokerHealthStepDown` metric); documented as extending the failover budget. |
| IDENTITY-1 | ✅ FIXED | Persistent+`client_id_suffix: hostname` is now **rejected** unless `assert_stable_client_identity: true`. |
| CLUSTER-3 | ✅ RESOLVED (Phase 6 shipped) | Coordinated cohorts roll **live-safe** deltas through an all-member barrier, wired into the shipped file-based root `bootstrap.App` (ADR 0013). Whole-cohort replacement is retained by design only for non-coordinated / file-sourced cohorts and replacement-required deltas (ADR 0012). |
| CB-1 | ✅ FIXED | Per-probe slot IDs in `Token`; a reclaimed probe's late outcome releases/counts only its own live slot — cannot free a newer probe's slot or vote. |
| STORE-1 | ✅ FIXED | Bounded store-op contexts on `QueryPending`/`Persist`/`Claim`; a store `DeadlineExceeded` is classified transient-retry (never poison) so a slow-but-healthy store cannot drop uncountable traffic. |
| CORE-RES-1 | ✅ FIXED | Watchdog latches the partition stalled; the drainer stops scheduling batches and escalates terminal (restart reclaims the leaked goroutine). |
| CORE-RES-2 | ✅ FIXED | Processor breaker now counts **outstanding** abandoned goroutines (paired inc/dec via the chain's done hook), not consecutive-since-settle. |
| MQTT-RES-1 | ✅ ACCEPTED (runbook + alarms) | Fail-closed whole-session reconcile retained; sustained `ReconcileFailures`/`MQTTQoSDowngraded` now have CDK alarms + the subscription-flap runbook. |
| MQTT-OBS-1 | ✅ FIXED | Close-time QoS 0 dispatch-queue entries drained and counted on `MQTTRouterDropped`; reservations released. |
| MQTT-OBS-2 | ✅ FIXED | A reserved ingress receiver with no declared plan is capped below `Full` until its first Reconcile. |
| MQTT-RES-2 | ✅ ADDRESSED | Accepted design; the SDK-substring upgrade checklist is retained in both code (`errors.go`) and operator docs. |
| MQTT-RES-3 | ✅ FIXED | All discard-path `Disconnect` calls use a bounded context. |
| DOC-REL-1 | ✅ FIXED | Pre-release image/tag wording corrected across README, deployment-guide, and the upgrade-rollback runbook. |
| §8 docs | ✅ FIXED | INVALID_CONFIG section, OTLP shipped-limitation note, config-rollback ConfigDegraded path, node-down-failover/scenario-08 CDK links, standalone split-brain runbook, new shutdown-timeout runbook, and 4 missing MQTT CDK alarms. |
| TEST-4 | ✅ FIXED | `TestUC3ClusterFailover` (real MQTT broker + real DynamoDB) now **asserts** failure-detection-to-`ServiceLevelFull` against a calibrated `uc3FailoverSLO` (15s; observed ~5.1s warm / ~5.2s cold), converting the historical "reported, never gated" duration into a hard pass/fail that a regression toward the unbounded ~336s default profile would trip. |
| TEST-1,2,3,5,6 | ✅ FIXED | **TEST-2**: `TestUC3SeparateProcessFailover` runs two real gobridge node OS processes (reusable `nodeProcess` re-exec launcher) competing for one real DynamoDB exclusive lease on a real broker; a real `SIGKILL` of the verified owner is followed by an asserted standby takeover — advanced fencing `Version` + `ServiceLevelFull` within `uc3FailoverSLO`. **TEST-1**: that proof is PR-gated via `make test-failover-gate` + a `ci.yml` integration-job step (mirroring the `test-mqtt-ingress-memory` exception). **TEST-5**: strict eventual-plateau — goroutines must return to baseline within a bounded drain budget (the historical ~33/cycle was async autopaho cleanup that empirically drains to baseline in ~60 s, not a leak). **TEST-6**: every RES probe is now a deterministic fault with one strict assertion (admission-reject / exactly-once / all-DLQ'd / panic-DLQ'd / no-misclassification). **TEST-3**: the shipped App's convergence watch is proven broker-backed (real unreachable broker → `ConfigDegraded`=1 + "not converged"; reachable → stays converged), closing the shipped-divergence gap; the generic Supervisor keeps its unit coverage of the same mechanism. Six `time.Sleep` allowlist entries retired. |

## 0.6 What is actually left

Every confirmed production-code, doc, and test-confidence finding above is closed
(✅ in §0.5). What remains falls into exactly three buckets — none is open
remediation work on this branch:

| # | Remaining item | Kind | Details |
|---|---|---|---|
| 1 | **REL-1** — run the module release train (dependency-ordered tags, strip workspace `replace` directives, external-consumer smoke gate) | Release execution | §2 → [REL-1](#rel-1--module-publication-is-pending-the-release-train) |
| 2 | **REL-2** — cut the `cmd/gobridge` release; workflow publishes the verified image digest | Release execution | §2 → [REL-2](#rel-2--image-publication-is-pending-a-command-release) |
| 3 | At release: swap pre-release doc wording for the verified digest/tag references (`README`, deployment-guide, upgrade-rollback runbook) | Release execution | [DOC-REL-1](#doc-rel-1--pre-release-docs-use-incorrect-present-tense---fixed) "Required fix" + §12 → [Release-only follow-up](#release-only-follow-up--not-a-production-finding) |
| 4 | Accepted-by-design residuals — disclosed limitations, **no work scheduled**: MQTT-RES-1 (whole-session reconcile flap; runbook+alarms), MQTT-RES-2 (SDK error-substring classification; upgrade checklist), MQTT-R1-OBS (deferred-connect `connect_after_lease` sessions can evade the post-swap convergence watch — readiness excludes them while no lease is held; found while adding the generic-Supervisor reload test, details in §9 TEST-3), plus the §10 rows marked "still present by design/contract" (AWS-bound shipped root, no OTLP in shipped image, DynamoDB-only distributed lease backend, bare `/ready` requires Full, no official Helm/manifests, ~336.5 s default failover profile unless tuned) | Accepted limitation | §3, §5, §10 |
| 5 | Refresh the §0 executive verdict — it is the **original audit snapshot** (pre-remediation) and still says NO-GO; its seven minimum gates are all closed per §0.5. A re-audit/verdict decision is a human call, not remediation work | Doc refresh | §0, §0.5 |

---

## 1. Evidence

| Evidence | Result | Limit |
|---|---|---|
| `make lint` | **PASS**, 72 s | Static evidence only. Advisory stages do not fail the build. |
| `make test` | **PASS**, 226 s | Runs unit tests with `-short -race`; Docker-backed behavior is skipped. |
| MQTT package `go test -race -count=1 ./...` | **PASS**, 34.8 s | Strong concurrency evidence inside the adapter, not a cluster proof. |
| Targeted broker-backed MQTT tests | **PASS**: settlement recovery, equal-publish identity, dedicated-session isolation, persistent-subscription migration, broker outage/reconnect, ingress poison | Focused cases, not the full long-running suite. |
| Root Dockerfile | Independently built and run; distroless, static, non-root `65532:65532`, structured stdout logs, bounded healthcheck failure | The resulting process is the AWS file-based composition root, not a portable general binary. |
| Documentation link scan | 524 operator-facing relative links checked, zero broken links | Correct links do not prove correct operational claims. |

### Test-gate reality

| Gate | Runs when | What it proves |
|---|---|---|
| `make test` | Local/default | Unit, race, timing audits. |
| `make test-integration` | PR integration job | Docker-backed integration; no long-running chaos/failover suite. |
| `make test-mqtt-ingress-memory` | PR integration job | One bounded MQTT ingress-memory proof. |
| `make test-long-running` | Schedule/manual only | Most failover, process-kill, store-outage, soak, and goroutine evidence. |

---

## 2. Release-stage status — excluded from production verdict

This section records what must happen when releasing. It is not a defect register
and does not contribute to the NO-GO production verdict.

| Release-state evidence | Result |
|---|---|
| Module metadata | Only root tags `v0.1.0`, `v0.2.0`; 45 module files contain local `replace` directives. |
| Workflow history | Two root-tag runs; the image job requires a stable `cmd/gobridge/vX.Y.Z` release. |
| Public package lookup | `GET /users/mariotoffia/packages/container/gobridge` returned 404 at audit time. |

### REL-1 — Module publication is pending the release train

`adapters/mqtt/transport/paho/go.mod:21-23`, `cmd/gobridge/go.mod:39`,
`RELEASE.md:256-285` **EXPECTED PRE-RELEASE:** external `go get` consumption is
not available until the dependency-ordered path-prefixed tag train is cut and
the release workflow strips/replaces workspace-local references.

**Release action:** run the existing release process and external-consumer smoke
gate. This is release execution, not production-code remediation.

### REL-2 — Image publication is pending a command release

`.github/workflows/release.yml:208-217`, `RELEASE.md:256-285`
**EXPECTED PRE-RELEASE:** the image job runs only for a stable
`cmd/gobridge/vX.Y.Z` release and publishes a verified digest. No such release
has been cut yet.

**Release action:** cut the command release and publish the digest after the
production-readiness findings are closed.

### DOC-REL-1 — Pre-release docs use incorrect present tense  — ✅ FIXED

`README.md:18-25`, `docs/deployment-guide.md:437-446`,
`docs/runbooks/upgrade-rollback-and-sqlite-durability.md:35-50`
**MEDIUM correctness [CONFIRMED]:** these files say the image and `v0.1.0` /
`v0.2.0` image tags are already published. `RELEASE.md` correctly says the
workflow publishes by digest only after a command release.

**Required fix:** use future/pre-release wording now. At release, replace it with
the verified `gobridge-image-digest.txt` reference. This is a documentation
accuracy finding, not a code-readiness blocker.

---

## 3. Confirmed production code findings

### MQTT-CORE-1 — Count-less MQTT redelivery evades the replay cap forever  — ✅ FIXED

`adapters/mqtt/transport/paho/acl_headers.go:196-203,363-428`, `runtime/route/leakguard.go:259-350`, `runtime/route/dispatch.go:658-729` **HIGH resilience [CONFIRMED]:** MQTT publishes without `mqtt.message-id` or correlation data receive a fresh UUID on every callback. The runtime replay ledger keys count-less sources by envelope ID. A durable MQTT `Delivery.Retry` recycles the connection without settling the publish, so the broker redelivery receives a new UUID and a new attempt counter.

**Failure sequence:**

1. A QoS 1/2 external producer sends a message without stable identity.
2. A `direct_hold` target fails transiently, or `shared_outbox` fails before Persist.
3. The runtime records attempt 1 under envelope `A` and calls `Delivery.Retry`.
4. MQTT recycles the durable session; the broker redelivers the same publish.
5. The adapter creates envelope `B`; the ledger reads attempt 0.
6. The sequence repeats indefinitely. `max_replay_attempts` never reaches its terminal action.

**Impact:** the message is not silently lost, but one deterministic poison message can recycle the whole session every 30 seconds, head-of-line-block ingress, and duplicate every innocent unsettled QoS 1/2 delivery. `docs/transports/mqtt.md:760,791-792` currently overstates that all `direct_hold` source attempts reach the configured cap.

**Affected:** durable Persistent/Exclusive QoS 1/2, `direct_hold`, and pre-Persist `shared_outbox` failures from producers without stable identity.

**Not affected:** producers supplying `mqtt.message-id`/correlation data; bridge-to-bridge MQTT, which stamps envelope ID; post-Persist outbox drain; QoS 0/Ephemeral, where Retry is unsupported and falls back terminally.

**Required fix:** mark generated identities explicitly and apply a finite policy that does not depend on envelope-ID stability: require producer identity, terminally DLQ the no-identity retry case, or add durable broker-redelivery state. Do not use a topic/payload content hash; it would collapse distinct equal-valued events.

### RECONFIG-1 — The shipped AWS process reports apply success before MQTT converges  — ✅ FIXED

`deployment/aws-filebased-config/lib/bootstrap/app.go:654-675,704-870,904-954`, `runtime/bridge_start.go:43-123,410-494`, `bridge/supervisor_convergence.go:13-165` **HIGH correctness [CONFIRMED]:** the AWS bootstrap duplicates runtime swapping instead of using `bridge.Supervisor`. It calls `Runtime.Start`, installs the runtime, and notifies the config manager with `nil` before MQTT background connection/subscription reaches broker truth. Its degraded provider only covers config-watch/apply errors, not post-apply transport convergence.

**Failure sequence:** a syntactically valid config contains denied MQTT credentials or an ACL-rejected topic; the healthy old runtime is stopped; the replacement `Start` returns after launching background managers; apply is acknowledged; the new transport retries indefinitely without reaching `LevelSubscribed`.

**Impact:** the shipped process can replace working delivery with a broken config while the apply result is green. Session readiness eventually shows failure, but the generic Supervisor's `ConfigDegraded` convergence signal and reason are absent. Initial AWS startup has the same truthfulness gap.

**Required fix:** route bootstrap lifecycle through `bridge.Supervisor`, reusing its preflights and convergence watch. If that is impossible, implement the same bounded `LevelSubscribed` barrier and applied-but-not-converged health state before claiming successful convergence.

### CLUSTER-1 — Static-endpoint clustering bypasses MQTT replica-safety validation  — ✅ FIXED

`validate/blueprint_graph.go:223-225,300-388`, `bridge/convert.go:84-103` **HIGH correctness [CONFIRMED]:** runtime composition defines clustered deployment as either `deployment_mode: clustered` or non-empty `bridge.cluster.endpoints`. Blueprint validation runs clustered MQTT shared-subscription and replica-identity checks only for the first spelling.

**Failure sequence:** multiple replicas configure static endpoints without `deployment_mode: clustered`; validation permits non-exclusive non-`$share` subscriptions or missing replica identity; runtime treats the same config as clustered.

**Impact:** replicas can N-fold consume the same logical traffic or collide on MQTT ClientID despite passing validation.

**Required fix:** expose one shared clustered predicate outside `bridge` or mirror it exactly in validation and pin both spellings with the same validation tests.

### RECONFIG-2 — Failed bootstrap candidates are not stopped  — ✅ FIXED

`deployment/aws-filebased-config/lib/bootstrap/app.go:796-902` **HIGH resilience [CONFIRMED]:** both overlap and prepare/commit paths return immediately when a newly built runtime's late `Start` fails. Cleanup defers are installed only after a successful Start, and `recoverPrevious` also enters wedged state without stopping a failed recovery candidate.

**Failure sequence:** Builder opens stores/sessions; `Runtime.Start` later rejects a configuration-dependent conflict or another component fails; bootstrap returns/rebuilds the old runtime but never calls bounded `Runtime.Stop` on the failed candidate.

**Impact:** repeated failed applies can leak store handles, adapter resources, and background state in the shipped process.

**Required fix:** install candidate cleanup before Start and stop every uninstalled runtime on all failure paths. Abort prepare/commit when stopping the old runtime fails instead of proceeding under uncertain ownership.

### CLUSTER-2 — Broker-only failure on the active owner does not fail over  — ✅ FIXED

`runtime/session/manager.go:389-424`, `runtime/session/manager_lease.go:608-765`, `adapters/mqtt/transport/paho/session_connection.go:152-174` **HIGH resilience [CONFIRMED requirement gap]:** a disconnected MQTT session logs/reconnects, but the session manager keeps renewing its exclusive lease while the lease store remains reachable.

**Failure sequence:** the active node alone loses its network path or authorization to the broker; DynamoDB remains reachable; lease renewal succeeds; standbys remain blocked in acquire-before-connect; Paho retries forever on the isolated node.

**Impact:** cluster availability can remain down indefinitely. The configured lease failover budget applies to owner/lease/process loss, not to this common node-local broker-path failure.

**Required fix:** if broker-path failover is part of the availability contract, add a configurable disconnected/non-converged threshold that quiesces ingress, closes the source, and releases or stops renewing the lease. Include the threshold in failover-budget validation and metrics.

### IDENTITY-1 — Persistent+hostname rollout can silently strand broker queues  — ✅ FIXED

`adapters/mqtt/transport/paho/factory.go:103-123`, `docs/transports/mqtt.md:84-107` **HIGH correctness [CONFIRMED]:** Persistent mode with `client_id_suffix: hostname` remains valid. The adapter warns, but Kubernetes Deployments and ECS tasks mint new hostnames on replacement.

**Failure sequence:** a rollout changes hostname; the effective MQTT ClientID changes; the new pod opens a different durable broker session; the old session's queued QoS 1/2 messages have no consumer and expire after `session_expiry_interval`.

**Impact:** loss by broker-session timeout is invisible to GoBridge. A startup warning is not an admission boundary.

**Required fix:** reject this combination unless configuration explicitly asserts stable replica identity, or restrict it to documented StatefulSet/VM profiles. Use Exclusive mode or Ephemeral+`$share` for Deployment/ECS replicas.

### CLUSTER-3 — Coordinated cluster config rollout is shipped  — ✅ RESOLVED (Phase 6)

`deployment/aws-filebased-config/lib/bootstrap/{app.go,rollout.go}`, `bridge/rollout_driver.go`, `docs/adr/0013-coordinated-cluster-config-rollout.md`, `docs/runbooks/cluster-config-rollout.md` **[RESOLVED — was a CONFIRMED capability gap]:** a coordinated cohort (`cluster.rollout: coordinated` on the versioned DynamoDB config source) now rolls **live-safe** deltas through a store-backed, all-member barrier — proposed as a candidate generation over a frozen roster, acked by every member, committed by a lease-elected fencing-protected coordinator, swapped only on the store-atomic commit. The shipped file-based image (`bootstrap.App`) hosts the barrier itself, so this is the shipped behavior, not just a library capability ([ADR 0013](docs/adr/0013-coordinated-cluster-config-rollout.md)).

**Residual refusal (by design, [ADR 0012](docs/adr/0012-cluster-config-whole-cohort-replacement.md)):** a **non-coordinated** cohort, an **EFS/file-sourced** cohort, and every **replacement-required** delta (durable session identity, lease/outbox/DLQ store target, `deployment_mode`, or the cohort's own `cluster.members`/`endpoints`/`rollout`) keep the whole-cohort replacement procedure. That refusal is correct — the barrier structurally cannot carry a change to its own membership epoch — and is now documented as the explicit two-path contract in the runbook.

**How it shipped:** the barrier was extracted behind a runtime-agnostic `bridge.RolloutHost` / `bridge.ClusterRolloutDriver` seam so BOTH `bridge.Supervisor` and the Builder-based `bootstrap.App` drive the same one implementation. The App supplies its runtime as the host (build-proof via `Builder.Plan`, swap via its own apply path with the ADR-0012 refusal replaced by propose-and-defer), a stable `member_id`, the real config codec (`parser.MarshalBridgeConfigJSON` ⟷ `parser.Parse`), and a `config.Manager.AdoptRunning` reconcile so deep-health does not latch `reconfigure_pending` after a barrier-driven swap. Coverage: `bridge/rollout_*_test.go` (unit + host-seam), `bootstrap/rollout_test.go` (component: propose-vs-refuse, boot→reload→commit→swap→converged), `bootstrap/rollout_integration_test.go` (real DynamoDB Local), and the design §10 multi-process UC-CR proofs at the Supervisor level.

**Design → ship:** the store-backed protocol (propose → stage/ack → fenced commit/abort) is specified in `docs/design/cluster-config-rollout-protocol.md` and was implemented against its **§11 phasing plan**; §12 is promoted verbatim to `docs/adr/0013`. Prior-art/split-brain research: `docs/design/cluster-config-rollout-research.md`.

- **Phase 1 ✅ (landed):** `domain/persistence` `Rollout` aggregate + state machine (invariants I1–I5), `ports.ClusterRolloutStore`, `adapters/native/store/memoryrollout` in-memory adapter, and the `ports/storetest.RunClusterRolloutStoreTests` conformance suite (CAS races, stale-token fencing, ack-after-abort, commit/abort atomicity) — green on `make lint` + `make test`.
- **Phase 2 ✅ (landed):** `adapters/aws/store/dynamodbrollout` — single-row, `ConsistentRead`, monotonic-`rev` optimistic-lock CAS; `persistence.RolloutSnapshot`/`RehydrateRollout` so the aggregate owns every invariant across a store round trip; the same conformance suite green (25/25, race-clean) against DynamoDB Local.
- **Phase 3 ✅ (landed):** rollout-class preflight (`classifyRolloutDelta`), the `cluster.rollout: coordinated` guard lift in `Supervisor.apply`, the coordinator decision core (`decideRollout`/`coordinatorStep`/lock-delay), and the applier units (digest verify, `nodeRolloutGate`, `evaluateProposal`).
- **Phase 4 ✅ (landed):** the barrier now drives end to end — applier (observe → digest-verify → class preflight → build-without-swap → `Ack`/`Nack`; swap only on `Committed`, through the normal apply path), coordinator `Run` loop (elect → lock-delay → renew → fenced decide → resign), boot-time joiner gate, `bridge.cluster.members` + `config/validate.go` admission rules, `ClusterRollout*` metrics, a `/deephealth` `rollout` section, and integration coverage against real DynamoDB. A prior audit had closed four proposer defects; **this phase found and fixed a fifth, more serious one**: the membership epoch was read from `bridge.cluster.endpoints`, which is this instance's CAPABILITY map (`{http: …}`), not a peer roster — every cohort would have frozen a one-"member" epoch named `http` and committed on a single ack, i.e. no barrier at all. Five design questions the phase owned are now decided rather than deferred (membership authority, candidate transport + joiner rule, digest determinism, `nodeRolloutGate` persistence, F5b first-decision fencing) — see §11 Phase 4.
- **Phase 4 residual — BLOCKING for wiring:** an adversarial review of this phase traced three reachable sequences (restart into the write→propose window; a commit overwritten before every member observed it; restart after an abort) that all reduce to one missing fact — under the chosen candidate transport the config source keeps only a `current` slot, so a member has no durable "last committed configuration" to fall back on. Mitigations landed (boot-staging, the joiner gate, bounded adopt retries) remove the permanent splits and deadlocks, but a bounded mixed-version window remains and an aborted rollout blocks member restarts until an operator rolls the config source back. **Phase 5 must give the cohort a durable last-committed artifact before the barrier is wired anywhere** — design §11 Phase 4 "BLOCKING residual".
- **Phase 5 ✅ (landed):** the durable last-committed config artifact (`domain/persistence` + the `ClusterCommittedConfigStore` port, on both the memory and DynamoDB stores) closes the BLOCKING transport residual — a member boots on the committed config after an abort and reconciles a missed commit, proven end to end with the REAL codec over real DynamoDB. Multi-process long-running proof: UC-CR1 (N=3 happy path), UC-CR2 (coordinator failover), UC-CR3 (SIGKILL → deadline-abort → rejoin on last committed), UC-CR7 (cross-member digest agreement) on the `nodeProcess` harness; UC-CR4/5 in the integration suite. (UC-CR6 unreachable-broker and UC-CR8 foreign-collision are non-blocking additional coverage — the latter is unit-covered by `joinActive`.)
- **Phase 6 ✅ (landed — ship):** `bridge.WithClusterRollout` is wired into the shipped file-based composition root `bootstrap.App` via the runtime-agnostic `bridge.RolloutHost` / `bridge.ClusterRolloutDriver` seam (the App uses `bridge.NewBuilder`, not the Supervisor, so the barrier was extracted to host on either). The two composition obligations are met: the boot-time joiner resolution + the `config.Manager.AdoptRunning` reconcile after a barrier-driven swap. Docs shipped: the runbook "coordinated mode" chapter (with the §8 replacement-required classes an operator must not route through the barrier), `docs/adr/0013` (design §12 promoted verbatim), and ADR 0012's "Superseded by 0013 (for live-safe deltas)" note. **Coordinated live-safe rollout is now the shipped behavior**; the ADR-0012 refusal is retained only for the residual classes above.

**Interaction note (not a dependency):** MQTT-RES-1 (one broker-rejected subscription flaps its whole session) is an independent, per-node session behaviour — it triggers on any broker SUBACK rejection (e.g. a server-side ACL change) with no config change and no cluster involved, and a distributed rollout protocol would not remove it. The interaction is one-directional: a config that *introduces* a broker-rejected subscription, applied across a cohort, would reproduce the MQTT-RES-1 flap on every member and latch the MQTT-R1 `ConfigDegraded` watch — one more reason clustered live reload stays refused fail-closed.

---

## 4. Failover budget

The implemented worst-case formula in `bridge/failover_budget.go:228-313` is:

```text
budget =
  lease_ttl
+ 2 × max_jittered_acquire_poll
+ (ceil(lease_ttl / min_jittered_acquire_poll) + 1) × renew_call_timeout
+ post_takeover_activation
+ startup_allowance
```

MQTT post-takeover activation is:

```text
2 × connect_timeout + 4 × reconcile_timeout + 2 × unmatched_grace
```

| Profile | Calculation | Computed bound |
|---|---:|---:|
| Shipped clustered HA defaults | `45s + 12.5s + 13×3s + 240s + 0` | **336.5 s** |
| Standalone-derived defaults | `360s + 12.5s + 97×5s + 240s + 0` | **1097.5 s** |
| Tuned warm-standby example | `15s + 5s + 11×1s + 18s + 0` | **49 s** |

A valid tuned timing shape uses these exact config paths:

| Config path | Value |
|---|---:|
| `routes[].session.lease_ttl` | `15s` |
| `routes[].session.renew_interval` | `2s` |
| `routes[].session.lease_renew_jitter` | `0s` |
| `routes[].session.renew_call_timeout` | `1s` |
| `routes[].session.max_renew_fails` | `3` |
| `routes[].session.acquire_poll_interval` | `2s` |
| `routes[].session.step_down_grace` | `5s` |
| `routes[].session.connect_after_lease` | `true` |
| `routes[].session.failover_slo` | `60s` |
| `sessions[].options.session.connect_timeout` | `3s` |
| `sessions[].options.session.reconcile_timeout` | `2s` |
| `sessions[].options.session.unmatched_grace` | `2s` |

The Builder rejects an exceeded declared SLO and logs an undeclared computed budget. This is configuration admission, not measured failover evidence. The 49-second value excludes broker-only failure detection unless CLUSTER-2 is fixed and excludes platform replacement latency unless covered by `startup_allowance`.

---

## 5. Additional resilience and correctness findings

### CB-1 — A late half-open result can release another probe's slot  — ✅ FIXED

`circuitbreaker/breaker.go:245-303,349-380` **MEDIUM correctness [CONFIRMED]:** a `Token` identifies only breaker generation and whether it was a probe. It does not identify the probe slot.

**Failure sequence:** probe A times out and its slot is reclaimed; probe B is admitted in the same half-open generation; A later reports; `AfterRequestToken(A)` releases the oldest current slot, which now belongs to B.

**Impact:** the breaker can exceed `HalfOpenMaxProbes`, and a late result can influence the current half-open epoch. MQTT uses this breaker through the generation-safe admission surface, so the defect is reachable when a probe returns after `probe_timeout`.

**Required fix:** assign each probe slot an ID and carry it in `Token`; release and count only the matching live slot. A reclaimed probe's late outcome must not release a newer slot.

### STORE-1 — Core outbox operations may inherit deadline-less contexts  — ✅ FIXED

`runtime/route/dispatch.go:1030-1083`, `runtime/outbox/loop.go`, `adapters/aws/store/dynamodboutbox/acl_store.go:452-525,654-740` **MEDIUM resilience [PLAUSIBLE]:** route-level Query/Persist and drainer Claim pass caller contexts directly to store methods. The DynamoDB adapter forwards those contexts to SDK calls. No core-owned per-operation deadline was found on these paths.

**Failure sequence:** an AWS endpoint or HTTP exchange black-holes after connection establishment while the route context has no deadline; Persist pins route in-flight capacity, or Claim pins a drainer partition.

**Impact:** settlement remains fail-safe, but delivery can stall without a bounded supervision transition.

**Missing proof:** no shipped AWS SDK call was reproduced hanging indefinitely; lower network layers may terminate a subset of failures.

**Required fix:** apply bounded store-operation contexts at the runtime port boundary and classify timeout for retry/supervision.

### CORE-RES-1 — A context-ignoring outbox sender can leak one batch repeatedly  — ✅ FIXED

`runtime/outbox/loop.go:529-568` **MEDIUM resilience [CONFIRMED defense-in-depth]:** the watchdog deliberately abandons a hung sender goroutine and continues draining later batches.

**Impact:** a Sender that ignores cancellation can accumulate one parked sender plus waiter goroutine per later batch, eventually exhausting memory. No violation was proven in the shipped MQTT or SQS senders; this is reachable through a hostile plugin or an SDK call that never returns.

**Required fix:** after the first watchdog expiry, latch the partition stalled/terminal and supervise or restart it instead of scheduling unlimited later batches.

### CORE-RES-2 — Processor leak protection is consecutive, not outstanding  — ✅ FIXED

`runtime/route/leakguard.go:101-150` **MEDIUM resilience [CONFIRMED defense-in-depth]:** the abandoned-processor counter resets on every terminal settlement. Interleaved healthy messages can therefore reset the counter while timed-out processor goroutines remain alive.

**Impact:** a custom processor that ignores cancellation can leak unbounded goroutines without tripping the 64-abandon breaker.

**Required fix:** count live abandoned processor calls and decrement when each exits; trip on outstanding count, not consecutive abandons.

### MQTT-RES-1 — One rejected subscription can flap the whole session  — ✅ ACCEPTED

`adapters/mqtt/transport/paho/session_reconcile.go:90`, `adapters/mqtt/transport/paho/session_reconcile_apply.go:230-278` **MEDIUM resilience [CONFIRMED accepted design]:** one permanent SUBACK rejection or QoS downgrade fails the whole authoritative reconcile. Exclusive mode releases ownership and supervision retries connect→subscribe→reject→disconnect forever at the capped backoff.

**Impact:** healthy sibling topics on the same session remain unavailable. This is fail-closed and observable, not partial silent service.

**Required action:** keep the operator runbook and alert on sustained `ReconcileFailures`/`MQTTQoSDowngraded`. Add per-topic quarantine only if partial service is an explicit product decision.

### MQTT-OBS-1 — Close abandons queued QoS 0 without a metric  — ✅ FIXED

`adapters/mqtt/transport/paho/acl_router_dispatch.go:15-30` **LOW observability [CONFIRMED]:** `dispatchLoop` returns immediately when `r.stop` closes, leaving buffered `dispatchCh` items undispatched.

**Impact:** QoS 1/2 remains unacknowledged and is redelivered. QoS 0 is lost at close without incrementing a drop counter.

**Required fix:** drain/count abandoned queue entries during shutdown or explicitly count remaining QoS 0 as `MQTTRouterDropped`.

### MQTT-OBS-2 — A receiver can briefly report Full before first Reconcile  — ✅ FIXED

`adapters/mqtt/transport/paho/session_health.go:70-104` **LOW correctness [PLAUSIBLE]:** a connected session with no declared plan is treated as a sender-only Full session. Health does not consult the reserved-ingress-receiver state.

**Failure sequence:** Start connects; first Reconcile is delayed behind a concurrent reload; readiness samples the session before the plan is stored.

**Impact:** a short false-ready window is possible without message loss. Runtime reachability and duration were not reproduced.

**Required fix:** cap a reserved receiver below Full until its first plan is declared.

### MQTT-RES-2 — SDK error-string classification is upgrade-fragile  — ✅ ACCEPTED

`adapters/mqtt/transport/paho/errors.go:34-52` **LOW resilience [CONFIRMED]:** connection errors are partly classified by case-insensitive SDK error substrings.

**Impact:** a Paho version bump can silently change retry classification. The fallback is retryable `ErrUnavailable`, so the failure is conservative rather than lossy.

**Required action:** retain the upgrade checklist and replace substring cases with typed errors/reason codes when the SDK exposes them.

### MQTT-RES-3 — Discard-path Disconnect uses an unbounded context  — ✅ FIXED

`adapters/mqtt/transport/paho/acl_session.go:226-261` **LOW resilience [CONFIRMED mitigated]:** failed/abandoned Start paths call `Disconnect(context.Background())`.

**Impact:** teardown can block if the SDK ignores cancellation of its already-cancelled connection-manager root. No shipped hang was reproduced.

**Required fix:** use a bounded disconnect context for defense in depth.

---

## 6. Message delivery, loss, duplication, and timeout model

### Guarantees that hold

- **Ingress QoS 1/2, Persistent/Exclusive:** manual acknowledgement delays PUBACK/PUBCOMP until runtime settlement. A crash, reconnect, emit failure, or unsettled close leaves the broker message available for redelivery, subject to broker session/queue expiry.
- **`direct_hold`:** destination Send succeeds before source Ack. An ambiguous destination success followed by source-ack failure duplicates on source redelivery; it does not silently lose the source message.
- **`shared_outbox`:** durable Persist succeeds before source Ack. Drainers use claim/fencing checks and replay persisted records. Send-success/Complete-failure remains an intentional duplicate window.
- **DLQ:** a failed DLQ write leaves the source delivery or outbox record unsettled. Successful DLQ/drop is terminal and observable.
- **Representational poison:** messages exceeding local payload/metadata/property caps are deliberately acked and dropped on `MQTTIngressPoisonDropped`, preventing a permanent redelivery kill loop.
- **Broker outage:** Paho reconnects forever with jittered bounded attempts; reconnect resets subscription state and drives authoritative reconcile.

### Loss and duplicate register

| Condition | Outcome | Recorded/reportable |
|---|---|---|
| QoS 0 dispatch queue full | Loss | `MQTTRouterDropped` |
| Covered QoS 0 pending overflow | Loss | `MQTTRouterCoveredDropped` |
| Recycle/epoch stale queue | QoS 0 loss; QoS 1/2 redelivery | `MQTTRouterStalePurged` |
| Local ingress cap violation | Acknowledged configured rejection | `MQTTIngressPoisonDropped` |
| Broker violates granted Receive Maximum | QoS 1/2 acknowledged loss to preserve liveness | `MQTTRouterOverflowDropped` |
| Stale broker-only orphan subscription | Acknowledged cleanup drop | `MQTTRouterUnmatchedDropped` |
| Router close with queued dispatch | QoS 0 loss; QoS 1/2 redelivery | **QoS 0 currently unmetered** |
| QoS 0/Ephemeral reload window | Loss by transport/session contract | Documented; the bridge cannot count messages it never receives |
| Broker offline queue/session expiry | Loss before bridge receipt | Invisible to bridge; broker metric required |
| Persistent+hostname Deployment/ECS rollout | Old broker queue stranded until expiry | Warning only; loss invisible to bridge |
| Raw MQTT QoS 1/2 Sender at process death | In-flight client packet state lost | `NonDurableEgress()` advertises the limit; route-layer modes compensate |
| Ack after reconnect | Guaranteed broker redelivery/duplicate | `MQTTAckAfterReconnect` |
| Settlement recovery recycle | All unsettled session deliveries may duplicate | `MQTTSessionRecoveryRecycle` plus unsettled gauges |
| Outbox send accepted, Complete/fence fails | Replay/duplicate risk | `OutboxDuplicateRisk` |
| Count-less replay-cap defect | No immediate loss; indefinite recycle and duplicate amplification | Recovery metrics reveal symptoms, but no terminal poison signal |

### Timeout and capacity controls

| Control | Purpose |
|---|---|
| `connect_timeout` | Bounds initial Start connection await. |
| `reconnect_timeout` | Bounds each TCP/TLS/MQTT reconnect attempt. |
| `reconcile_timeout` | Bounds each SUBSCRIBE/UNSUBSCRIBE and cannot be disabled. |
| Sender `options.timeout` / route `send_timeout` | Bounds publish settlement; direct library use receives a 60-second safety net when no deadline exists. |
| `unmatched_grace` | Bounds startup/reconnect handler-registration and managed-removal verification windows. |
| `drain_timeout` / process `shutdown_timeout` | Bounds controlled stop and settlement drain. |
| `receive_maximum` | Broker flow-control window and shared ingress reservation ceiling. |
| `max_payload_bytes` / ingress memory budget | Bounds retained payload and validates worst-case ingress memory. |
| `lease_ttl`, `renew_call_timeout`, `acquire_poll_interval`, `failover_slo` | Bound and optionally reject lease-failover timing. |

These bounds prevent most normal dependency outages from becoming infinite waits. STORE-1, hostile context-ignoring plugins, and the count-less replay defect remain exceptions.

---

## 7. Outage, cluster, and reconfiguration behavior

| Event | Current behavior | Verdict |
|---|---|---|
| MQTT broker outage affecting all nodes | Each session retries forever with bounded jittered attempts; readiness falls; authoritative reconcile runs after reconnect. | **Resilient, at-least-once limits apply.** |
| MQTT broker path fails only on active exclusive owner | Active keeps lease while reconnecting; standby cannot connect/take over. | **Not resilient for node-local broker-path failure.** |
| Lease store is transiently unavailable | Renew calls are bounded; active fails closed at its local lease deadline; standbys cannot acquire until store recovers. | **Safety over availability.** |
| Process/pod dies | Lease expires/transfers; stable Exclusive ClientID resumes broker session; outbox fencing prevents stale completion. | **Conditional HA.** Default timing is minutes. |
| Config source watcher fails after startup | Last-good runtime remains active; watcher retries with backoff and degraded health. | **Resilient.** |
| Parsed config is structurally invalid | Validation rejects it before live replacement. | **Resilient.** |
| Config is syntactically valid but broker-invalid | Generic Supervisor commits then reports non-convergence after budget; no automatic rollback. AWS bootstrap lacks that convergence state. | **Generic conditional; shipped process unsafe to claim converged.** |
| Single-instance MQTT config reload | Serialized stop/rebuild/start. Durable QoS 1/2 can redeliver; QoS 0/Ephemeral traffic can be lost. | **Controlled restart, not hitless.** |
| Clustered config reload | Non-no-op reload is refused. | **Safe refusal; no live cluster reconfiguration.** |
| Graceful shutdown | Quiesce/drain/stop is bounded; unsettled QoS 1/2 redelivers. | **Conditional on orchestrator grace period.** |
| SIGKILL before shutdown budget | QoS 1/2/outbox recovery usually preserves at-least-once; QoS 0 and pre-Persist Ephemeral traffic can be lost. | **Expected crash semantics; no dedicated incident runbook.** |

### Supported cluster shapes

| Shape | Supported | Requirements and limits |
|---|---|---|
| Ephemeral shared-consumer scale-out | **Yes** | `$share/<group>/<topic>`, unique per-replica ClientID, no durable broker-session continuity. |
| Exclusive active/standby | **Yes, conditional** | Stable shared ClientID, distributed LeaseStore, durable shared outbox/managed-subscription stores, warm polling standby, declared/measured failover objective. DynamoDB is the only shipped production lease backend. |
| Persistent per-replica sessions | **Only with stable replica identity** | StatefulSet/VM identity. Deployment/ECS hostname suffix is unsafe. |
| Multi-broker URL failover for durable sessions | **No by design** | Persistent/Exclusive sessions require one stable broker-session domain; broker HA must sit behind one stable endpoint. |
| Plain Kubernetes exclusive HA without AWS services | **No shipped backend** | A custom distributed lease/store adapter is required. |

---

## 8. Documentation and operator-readiness findings

The MQTT reference document is substantially improved and unusually explicit about delivery boundaries, memory sizing, failover math, persistent identity, poison handling, and controlled-restart reload semantics. The documentation is still not production-ready because one delivery claim is wrong, pre-release image instructions use false present tense, and several operational signals do not lead to complete procedures.

> **Status:** this table is the audit-time snapshot. Every row has since been
> fixed — see the `§8 docs` and `DOC-REL-1` rows in §0.5. Only the at-release
> wording swap (§0.6 item 3) remains.

| Finding | Severity | Problem | Required fix |
|---|---|---|---|
| `docs/transports/mqtt.md:760,791-792` | **HIGH correctness [CONFIRMED]** | Claims `direct_hold` count-less MQTT reaches `MaxReplayAttempts`; MQTT-CORE-1 disproves this for producers without stable identity. | Document the vulnerable shape immediately; update after the runtime policy is fixed. |
| `README.md:18-25`, `docs/deployment-guide.md:437-446`, `docs/runbooks/upgrade-rollback-and-sqlite-durability.md:35-50` | **MEDIUM correctness [CONFIRMED]** | Pre-release docs claim an image and semver image tags are already published, while the release process publishes a digest only after a command release. | Use pre-release wording now; inject/link the verified digest when releasing. |
| `docs/troubleshooting.md:532-542` | **MEDIUM clarity [CONFIRMED]** | `INVALID_CONFIG` appears in the summary table but has no error-code section despite being a common startup/admin response. | Add symptom, validation-error extraction, and recovery guidance. |
| `docs/deployment-guide.md:694-707` | **MEDIUM clarity [CONFIRMED]** | Lists OTLP beside CloudWatch without stating that the shipped image accepts only `noop`/`cloudwatch`. | Repeat the shipped-process limitation at the metrics section; OTLP requires a custom composition root. |
| `docs/runbooks/config-rollback.md` | **MEDIUM clarity [CONFIRMED]** | MQTT docs send `ConfigDegraded` incidents here, but the runbook never names `ConfigDegraded`, `config_watch.reason`, or applied-but-not-converged. | Add a matching symptom/diagnosis path and distinguish slow convergence from broker-invalid config. |
| `docs/runbooks/lease-flapping-split-brain.md:1-63` | **MEDIUM completeness [CONFIRMED]** | Covers distributed clustered lease flapping, not standalone multi-replica split brain or the `SPLIT-BRAIN RISK` warning. | Add standalone diagnosis and enforce `replicas: 1`. |
| `docs/runbooks/node-down-failover.md:108`, `docs/scenarios/08-clustered-exclusive-sessions.md:564-572` | **MEDIUM correctness [CONFIRMED]** | Point to deleted `PROD_READY_ISSUES_PLAN.md` Task 11 and say AWS warm-standby enforcement is future work, although `GoBridgeDynamoDBHA` now enforces replica/AZ invariants. | Link the shipped CDK construct; preserve only the non-CDK caveat. |
| `deployment/aws-filebased-config/cdk/constructs/gobridgealarms/alarms.go` | **MEDIUM observability [CONFIRMED]** | Shipped alarm bundle has no `MQTTIngressPoisonDropped`, sustained `ReconcileFailures`, `MQTTSessionTakeover`, or `MQTTQoSDowngraded` alarm despite docs instructing operators to alert on them. | Add alarms or state next to the alarm table that MQTT alarms must be wired separately. |
| No shutdown-timeout runbook | **LOW completeness [CONFIRMED]** | Preventive grace-period guidance exists, but no incident procedure explains blast radius after orchestrator SIGKILL. | Add a short runbook keyed on orchestrator kill/exit evidence and outbox/QoS checks. |

### Platform consumption matrix

| Consumption path | Status | Reason |
|---|---|---|
| `go get` library modules | **PENDING RELEASE — NOT SCORED** | The release train is designed to produce the external module graph when releasing. |
| `cmd/gobridge` reference binary | **CONDITIONAL** | Demo-only: MQTT plus native stores, limited composition, explicit startup warning. |
| Public GHCR production image | **PENDING RELEASE — NOT SCORED** | The workflow publishes the verified digest when the command release is cut. |
| Root Dockerfile built locally | **CONDITIONAL** | Secure small image, but AWS file-based composition root with SSM/DynamoDB assumptions. |
| Plain Docker with custom composition root | **CONDITIONAL** | Supported framework path, but consumer must write and own `main`. |
| Non-AWS Kubernetes | **CONDITIONAL** | Good probe/shutdown/config guidance; no official chart/manifests; no shipped non-AWS exclusive-HA store. |
| ECS through shipped CDK | **CONDITIONAL** | Mature DynamoDB HA construct and alarms, but credentialed production failover proof is absent. |

### Runbook coverage

| Incident | Coverage |
|---|---|
| Broker outage/reconnect storm | **Covered** |
| DynamoDB/store outage | **Covered** |
| MQTT ingress poison | **Covered** |
| SUBACK/QoS rejection flap | **Covered** |
| Persistent managed-subscription migration | **Covered** |
| Failover SLO breach | **Covered, but measurement evidence is weak** |
| Reload non-convergence | **Partial; target rollback runbook misses the signal vocabulary** |
| Standalone split brain | **Gap** |
| Shutdown grace-period breach | **Gap** |
| Suspected message loss/duplicates | **Distributed across reference tables; no single triage runbook** |

---

## 9. Test-confidence findings

### TEST-1 — Critical resilience evidence is not PR-gated  — ✅ FIXED

`.github/workflows/ci.yml:81-94` **HIGH test-gap [CONFIRMED]:** cluster failover, store-outage, process-kill, and goroutine tests run only on schedule/manual dispatch.

**Risk:** outage-recovery regressions can merge while default and integration CI stay green.

**Minimum test:** promote a bounded real-broker, real-store, separate-process failover subset into PR integration.

**Resolution:** `make test-failover-gate` compiles the `longrunning`-tagged suite and runs only `TestUC3SeparateProcessFailover` (TEST-2 — separate-process, real broker + real DynamoDB lease/outbox, ~14 s under `-race`) with a bounded 420 s timeout; a new `integration`-job step in `ci.yml` runs it on every PR. This mirrors the existing `test-mqtt-ingress-memory` PR exception rather than adding a new classification. The full failover/chaos/soak suite still runs only nightly.

### TEST-2 — Most failover integration is same-process or sleep-driven  — ✅ FIXED

`tests/integration/e2e_failover_test.go:70-120,285-308,435-460` **HIGH test-gap [CONFIRMED]:** several named failover scenarios use fake sessions/senders/receivers in one process, and crash timing depends on fixed sleeps.

**Risk:** these tests do not prove broker-session takeover, process isolation, orchestrator timing, or deterministic crash boundaries.

**Minimum test:** use separate processes and explicit barriers such as persisted outbox depth, verified lease owner/fencing version, sender-entry latch, and `ServiceLevelFull`.

**Resolution:** `tests/longrunning/uc3sp_separate_process_failover_test.go` — `TestUC3SeparateProcessFailover` launches **two real OS-process gobridge nodes** (a reusable `nodeProcess` re-exec launcher in `nodeprocess_harness_test.go`, generalising the `chaos_process_kill` pattern) that compete for **one real DynamoDB exclusive lease** on a **real broker**. The parent identifies the elected owner from its `NODE_FULL` stdout barrier AND the authoritative DynamoDB lease row (owner + fencing `Version`), then issues a **real `SIGKILL`** — so no graceful `Release` runs and the successor must win by conditional-update fencing after the genuine TTL, not a simulated hand-off. It asserts the standby reaches `ServiceLevelFull` with a **strictly greater fencing version** within `uc3FailoverSLO` (~5.2 s observed) and at-least-once delivery of every message across the failover. No fakes, no sleep-driven crash timing; barriers are the lease row + per-node health tokens + collector delivery.

### TEST-3 — Broker-invalid reload is unit-only  — ✅ FIXED (shipped root broker-backed)

`bridge/supervisor_convergence_test.go:47-80` **HIGH test-gap [CONFIRMED]:** convergence behavior is pinned with an unstarted in-memory runtime, not a broker-backed config-manager→supervisor→MQTT failure.

**Risk:** the operator path for bad credentials/ACL-denied subscriptions remains unproved, and the shipped AWS divergence is untested.

**Minimum test:** apply a valid-but-broker-rejected MQTT config through both generic and shipped composition roots; assert apply status, degraded reason, readiness, recovery, and no leaked candidate runtime.

**Resolution:** `deployment/aws-filebased-config/lib/bootstrap/integration_broker_convergence_test.go` drives the **shipped App's real `runConvergenceWatch`** against a runtime whose Exclusive MQTT session is broker-backed for real (memory lease/outbox + a real paho session):
- `TestReconfig1_ConvergenceWatch_BrokerUnreachable_MarksDegraded` — the session cannot reach the broker → readiness pins below `LevelSubscribed` → the watch latches `ConfigDegraded=1` with the `"not converged"` reason and config version (the false-success guard the shipped process previously lacked). This directly closes "the shipped AWS divergence is untested."
- `TestReconfig1_ConvergenceWatch_BrokerReachable_StaysConverged` — a reachable broker reaches `LevelSubscribed` → the watch clears/returns without ever marking (recovery / no false positive).

This upgrades the shipped convergence coverage from the old sentinel-runtime state-machine test to a **real broker-driven readiness** proof. The **generic Supervisor's** identical readiness-driven mark-at-budget mechanism (which the App's `convergence.go` explicitly mirrors) remains covered by `bridge/supervisor_convergence_test.go`; "no leaked candidate runtime" is covered by the RECONFIG-2 candidate-cleanup tests.

The config-driven generic-Supervisor reload test — originally cut because the MQTT-loopback harness proved brittle — was **subsequently added** once the deterministic test primitives (shared container gates, `wait.Until`, `WaitRouteReady`, per-test brokers) made it stable: `tests/integration/integration_supervisor_mqtt_reload_test.go` (`TestSupervisorMQTTReload_ConfigDrivenBrokerTruth`) drives ONE config file through file-source → `config.Manager` → real `bridge.Supervisor` swaps against a real Mosquitto broker: v1 converges (`LevelSubscribed` + end-to-end traffic), v2 (config-valid but broker-unreachable) **commits** and the real MQTT-R1 watch marks `Degraded()` with the "not converged" reason and config version after the genuine 60 s `convergenceBudgetFloor`, v3 recovers (degraded cleared, route ready, traffic flows again). Zero sleeps, zero fake clocks; ~61 s per run, passed twice under `-race`.

One residual discovered by that work — **MQTT-R1-OBS**: a `connect_after_lease` (deferred-connect) session that can never reach its broker does **not** trip the convergence watch, because `ports.ReadinessLevelFromDeepHealth` excludes a deferred-connect session holding no lease, and against an unreachable broker the manager cycles acquire → connect-fail → release, so readiness reads `LevelSubscribed` and the watch believes it converged. The reload test therefore pins the eager-connect path (`connect_after_lease: false`); deferred-connect convergence blindness is disclosed in §0.6 (item 4), no fix scheduled.

### TEST-4 — The failover test does not assert the advertised SLO  — ✅ FIXED (real broker+store test now asserts failover ≤ uc3FailoverSLO)

`tests/longrunning/uc3_cluster_failover_test.go:200-227` **HIGH test-gap [CONFIRMED]:** warm/cold failover durations are reported, not asserted, under compressed test lease timings.

**Risk:** a passing suite does not establish 30–60 second production failover.

**Minimum test:** declare a production-like `failover_slo`, stop the verified leaseholder, and assert successor owner/version plus `ServiceLevelFull` within the objective. Add a negative default-profile proof.

### TEST-5 — Goroutine stability test accepts known growth  — ✅ FIXED

`tests/longrunning/gap_goroutine_leak_test.go:100-130` **HIGH test-gap [CONFIRMED]:** the test documents about 35–40 MQTT cleanup goroutines per cycle and passes while the last-cycle increase stays below 50.

**Risk:** the suite proves only "not worse than the accepted leak," not lifecycle stability.

**Minimum test:** require eventual zero growth or a stable plateau after bounded cleanup, then run the proof regularly.

**Resolution:** the test now measures a pre-bridge baseline, runs N start/stop cycles, and — via `wait.Until` (hard fail on timeout) — asserts `runtime.NumGoroutine()` **descends back to baseline + tolerance** within a bounded drain budget. This is the finding's "eventual zero growth / stable plateau." A measured decay curve settled the accepted-baseline question: the historical ~33/cycle was **asynchronous autopaho connection-manager cleanup that drains in ~60 s** (measured `126→58→8→2` post-`Close`), not a leak — the old fixed 3.5 s settle simply sampled mid-unwind. Because the unwind is multi-stage (intermediate plateaus up to ~15 s), the gate waits for the count to *descend to a target* rather than trying to identify a settled floor (which passes through every plateau above it); the tolerance is set to comfortably cover the ~12-goroutine residue the AWS SDK HTTP pools leave, so a healthy run is never flaky while a genuine per-cycle leak keeps the count above the target and fails hard via the wait timeout. The four now-unnecessary `time.Sleep` allowlist entries were removed.

### TEST-6 — Some resilience probes tolerate either outcome  — ✅ FIXED

`tests/longrunning/res_gap_validation_test.go:180-215` **MEDIUM test-gap [CONFIRMED]:** safety-critical probes describe expected and broken outcomes but are primarily observational.

**Risk:** a green run can be cited as evidence without enforcing the desired behavior.

**Minimum test:** deterministic fault injection with one strict expected result per regression.

**Resolution:** every RES probe now injects a deterministic fault and asserts exactly one expected result (the fixed behavior), replacing the "log which outcome occurred" branches:
- **RES-003** — admission **rejects** the silent-drop config (DirectHold + no DLQ + non-retryable MQTT source): the drop is now impossible, not improbable.
- **RES-005** — auto-extend keeps pace, asserted as **exactly-once** (`unique == msgCount`, zero duplicates).
- **RES-006** — every permanently-failed message reaches the DLQ (`dlq == msgCount`) and **none** reaches the output.
- **RES-011** — a panicked processor is recovered and DLQ'd: `delivered + dlq == msgCount` with exactly the panicked subset DLQ'd (no silent swallow).
- **RES-001** — the degraded sender's PRNG is **fixed-seeded** (was the global unseeded source); the transient failures are never misclassified into the DLQ and the pipeline keeps making progress under the circuit breaker.

The two `NEGATIVE` `time.Sleep` waits were replaced with `wait.StableFor` (deterministic settle) and admission-time return, and their allowlist entries removed.

### Confidence by requirement

| Requirement | Confidence | Reason |
|---|---|---|
| Manual ack / durable handoff | **Strong** | Unit, race, broker-backed settlement recovery, outbox persistence. |
| Broker reconnect/redelivery | **Strong** | Real-broker outage and persistent QoS tests. |
| Poison/oversize handling | **Strong** | Unit and real-broker property-amplification tests. |
| Reconcile retry | **Strong** | Unit plus broker reconnect coverage. |
| Duplicate/idempotency boundary | **Partial** | Real broker and real store are not combined through final sink/takeover in a PR gate. |
| Live reload failure | **Partial** | Generic unit evidence; shipped bootstrap path lacks the guarantee and integration proof. |
| Lease fencing/step-down | **Partial** | Strong unit/store evidence; limited separate-process broker-backed proof. |
| 30–60 second failover | **Absent as a production claim** | Computed tuned profile only; no production-like assertion. |
| Goroutine/memory stability | **Partial** | Ingress memory proof is strong; lifecycle leak test accepts growth. |

---

## 10. Previous-review disposition

The previous report was not wasted: it triggered substantial root fixes. This table prevents closed findings from being mistaken for current defects and prevents warning-only mitigations from being mistaken for fixes.

| Prior IDs | Current status | Evidence |
|---|---|---|
| MQTT-C1 | **Release-stage only; excluded from readiness** | External module tags are intentionally produced by the release train. |
| MQTT-C2 | **Still present** | Generic binary remains demo-only; production composition root remains AWS-specific. |
| MQTT-C3 | **Still present** | Shipped bootstrap accepts `noop`/`cloudwatch`, not OTLP. |
| MQTT-C4 | **Resolved** | Runtime drain now derives from the process shutdown context instead of stacking a fresh budget. |
| MQTT-C5 | **Still present by design** | DynamoDB is the only shipped distributed lease/HA backend. |
| MQTT-C6 | **Still a compatibility trap** | Bare `/ready` requires Full and returns 503 for healthy standby; explicit `?level=connected/subscribed` is required. |
| MQTT-C7 | **Still present** | No official Helm/Kustomize/manifests; examples require a consumer-built image. |
| MQTT-D1 / DOC4 | **Resolved** | Package docs now match covered-topic retain/orphan-drop behavior. |
| MQTT-D2 | **Resolved** | Large MQTT files were split below the repository limit. |
| MQTT-D3 / L5 | **Resolved observability gap** | Ack-after-reconnect duplicate is counted on `MQTTAckAfterReconnect`. |
| MQTT-D4 | **Accepted contract** | QoS 0 remains best-effort/lossy under pressure and lifecycle windows. |
| MQTT-D5 | **Accepted constraint** | Durable sessions require one stable broker-session domain; multi-URL client failover is Ephemeral-only. |
| MQTT-D6 / L1 | **Resolved for compliant brokers** | Local cap violations ack-drop with metric; malformed/advertised-limit-violating broker packets still fail closed. |
| MQTT-F1 | **Still present, now disclosed precisely** | Default clustered bound is about 336.5 seconds. |
| MQTT-F2 | **Resolved disclosure gap** | Undeclared computable budgets are logged; declared SLOs are enforced. |
| MQTT-F3 | **Mitigated, not resolved** | Persistent+hostname now warns but remains valid and unsafe on Deployment/ECS. |
| MQTT-F4 | **Mitigated, not resolved** | Single-replica memory-lease acknowledgement is explicit; actual replica count remains external outside the CDK HA profile. |
| MQTT-F5 | **Still present by design** | Lease-loss step-down can require single-use session/process restart. |
| MQTT-F6 | **Changed** | Cross-generation/store identity guards improved; ManagedSubscriptionStore still has no independent fencing token. |
| MQTT-R1 | **Resolved only in generic Supervisor** | Post-swap convergence watch exists; shipped AWS bootstrap bypasses it. |
| MQTT-R2 | **Still present by design** | MQTT changes are stop/rebuild/start, not hitless. |
| MQTT-R3 / O2 | **Still present by design** | Permanent subscription rejection flaps the whole session; runbook now exists. |
| MQTT-R4 | **Still present in direct APIs** | Command path adds windowing; base Supervisor/bootstrap apply directly unless configured otherwise. |
| MQTT-R5 | **Resolved admission gap** | Durable desired subscriptions require managed history. |
| MQTT-R6 | **Still present by design** | Failed managed removal stays fail-closed until revert/drain/migration. |
| MQTT-L1 | **Resolved** | Authorized representational poison no longer terminal-loops the session. |
| MQTT-L2 | **Resolved** | Pre-first-Reconcile traffic is treated as covered and retained. |
| MQTT-L3 | **Resolved** | Receiver emit error requests bounded recovery. |
| MQTT-L4 | **Resolved** | Recycle-window discard is metered. |
| MQTT-L5 | **Resolved** | Ack-after-reconnect duplicate window is metered. |
| MQTT-L6 | **Resolved** | Persistent+`clean_start:true` warns. |
| MQTT-L7 | **Still present and now linked to MQTT-CORE-1** | Whole-session Retry recycle is intentional; no-identity redelivery can make it unbounded. |
| MQTT-L8/L9 | **Still present by contract** | Shutdown/QoS 0 loss remains; one close-time branch is unmetered. |
| MQTT-O1 | **Still present by contract** | Paho outbound session state is in-memory; bridge delivery modes supply durability. |
| MQTT-O3 | **Resolved original misuse** | MQTT breaker sender uses token admission; CB-1 is a separate slot-identity defect. |
| MQTT-O4 | **Mitigated** | Lifecycle event eviction remains bounded and is now metered. |
| MQTT-O5/O6 | **Still present, low** | Error substring matching and unbounded discard-path Disconnect remain. |
| MQTT-DOC1/DOC2/DOC3/DOC5 | **Resolved** | Takeover code, queue size, missing metric catalogue, and connect-latency references corrected. |
| MQTT-DOC6 | **Mostly resolved** | Poison, subscription-flap, and migration runbooks plus capacity math shipped; current gaps are listed in §8. |

---

## 11. What is already production-grade

The negative production-readiness verdict should not obscure the engineering that is ready:

- Manual MQTT acknowledgment preserves ack-after-runtime-settlement for QoS 1/2.
- Covered-topic QoS 1/2 is retained unacknowledged; orphan cleanup is explicit and metered.
- Broker network operations and reconcile operations are bounded.
- Reconnect uses jitter and authoritative re-subscription.
- Reconcile state is epoch-guarded; partial broker success does not become false desired state.
- Ingress memory is count- and byte-bounded with overflow-safe admission math.
- Local poison caps have an explicit ack-drop escape and dedicated metric/runbook.
- Reserved bridge headers are stripped at the trust boundary; transport metadata cannot be spoofed through MQTT user properties.
- Plaintext credentials fail closed unless explicitly allowed; TLS requires complete material and TLS 1.2 minimum.
- Lease ownership uses boot-nonce identity, bounded calls, local fail-closed deadlines, DynamoDB conditional updates, and fenced outbox completion.
- Invalid watched config preserves the last-good runtime; watcher failures retry and surface degraded health.
- The container build is static, distroless, non-root, structured-log friendly, and healthcheck capable.
- Documentation now states default failover math, deployment identity hazards, QoS loss boundaries, and capacity formulas honestly in most sections.

---

## 12. Remediation order

1. **Fix MQTT-CORE-1 and its documentation/test.** This is the only confirmed cross-cutting defect that lets one producer message create an unbounded session recovery loop.
2. **Delete bootstrap lifecycle duplication.** Reuse `bridge.Supervisor` for preflight, candidate cleanup, apply status, and convergence instead of maintaining a second weaker swap state machine.
3. **Use one clustered predicate.** Apply identical validation for explicit mode and static endpoints.
4. **Close candidate-runtime cleanup.** Stop every failed/uninstalled runtime and abort on uncertain old-runtime Stop.
5. **Decide the broker-path failover contract.** If 30–60 seconds means node-local MQTT isolation too, implement health-triggered step-down and budget it.
6. **Turn Persistent+hostname from warning into admission.**
7. **Add one real proof, not more fake coverage.** Separate processes, real broker, real lease/outbox store, verified owner kill, asserted configured SLO, broker-path isolation, and broker-invalid reload.
8. **Eliminate the accepted goroutine-growth baseline.**
9. **Close the operator gaps:** MQTT alarms, `ConfigDegraded` rollback flow, standalone split-brain, shutdown breach, and consolidated message-loss triage.

The shortest safe route is reuse, not another subsystem: reuse `bridge.Supervisor`, reuse one clustered predicate, and add one production-shaped failover test.

### Release-only follow-up — not a production finding

After the production-readiness findings are closed, run the existing module
release train, publish the command image digest, and switch installation/rollback
documentation from pre-release wording to the verified released references.
