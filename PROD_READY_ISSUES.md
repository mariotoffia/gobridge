# PROD_READY_ISSUES — MQTT Transport Adversarial Review

**Date:** 2026-07-17
**Scope:** `adapters/mqtt/transport/paho` (10,591 production LOC, 26 files) plus its consumption surface: `ports/` contracts, `bridge/`/`runtime/` supervision, lease/managed-subscription stores, `cmd/gobridge`, `Dockerfile`, `deployment/`, and all MQTT-related documentation (`docs/transports/mqtt.md`, ADRs 0003/0009/0010, runbooks, scenarios).
**Method:** Adversarial multi-track review — six independent review tracks (message-loss/delivery guarantees, outage resilience, cluster/failover, runtime reconfiguration, documentation accuracy, container consumability), each required to construct concrete failure sequences with `file:line` evidence and to mark findings CONFIRMED (traced code path) vs PLAUSIBLE (suspicion without a complete trace). Findings were cross-verified against a direct read of the ingress/egress/session/reconcile core before inclusion. Evidence runs: full package unit-test suite under the race detector.

Severity scale: `BLOCKER | HIGH | MEDIUM | LOW | NIT`. Categories follow `LANGUAGE.md`: correctness, security, architecture, resilience, observability, test-gap, maintainability, clarity.

---

## 0. Executive verdict

**The MQTT transport core is among the most carefully engineered adapter code this reviewer has audited — and the product around it is not ready to ship.** Those are two different statements and both matter.

**What is genuinely production grade (verified, not taken on faith):** ack-after-durable-handoff with manual acknowledgment; covered-topic retention instead of ack-drop; epoch-guarded reconcile with applied-plan retry history; jittered forever-reconnect with takeover damping; fenced leases with local-deadline fail-closed step-down; every network operation bounded; race-detector suite green; 20+ regression pins showing prior findings were fixed at the root. The at-least-once claim **holds** for QoS 1/2 on persistent/exclusive sessions against a compliant broker — including across crashes, reconnects, and reloads.

**What blocks production, ranked:**

1. **Packaging (BLOCKER — MQTT-C1/C2/C3):** `go get` fails (replace directives, no module tags), the production binary is unpublished/AWS-only, and shipped artifacts have no metrics on non-AWS platforms. The release procedure exists in `RELEASE.md`; it has never been executed.
2. **Reload false-success (HIGH — MQTT-R1):** a syntactically-valid-but-broker-invalid config takes down a working transport and reports green. Requires a post-swap health barrier or at minimum an applied-but-not-converged signal.
3. **Poison loop (HIGH — MQTT-L1):** any authorized publisher can permanently kill a session (and head-of-line-block its broker queue) with one spec-compliant publish exceeding a local cap. Needs ack-and-DLQ or a documented escape.
4. **Failover defaults (HIGH — MQTT-F1/F2):** the 30–60 s requirement is met only with explicit tuning + `failover_slo`; defaults deliver ~336 s (HA profile) and nothing warns unless the SLO is declared.
5. **Identity/topology traps (HIGH — MQTT-F3, MEDIUM — F4/C5):** Persistent mode + hostname suffix silently strands broker queues on every Deployment rollout; standalone split-brain is warn-only; plain-K8s HA has no store backend.
6. **Observability edges (MEDIUM — MQTT-L2/L4/L5):** one loss path mislabeled as benign, one fully silent drop branch, one silent duplicate window that falsely clears the unsettled gauge.
7. **Documentation (HIGH — DOC1/DOC2, MEDIUM — DOC3/DOC6):** reference docs are excellent and accurate; the troubleshooting page contains two factual errors in its most important entry, and the three "stuck until operator acts" states have no runbooks.

**Answers in one line each:** Production ready — conditional (adapter yes, product no). Docs production ready — conditional (reference yes, troubleshooting/runbooks no). Zero bugs — no (8 confirmed code defects, all edge-path). Cluster — yes for exclusive-lease active/standby (AWS only) and ephemeral+$share scale-out. Resilient to outages — yes, confirmed. Message loss — none for durable QoS 1/2 with shipped delivery modes; QoS 0 by contract; three observability gaps. Easy container consumption — process yes, packaging no. Live reconfigure — controlled restart, not hitless; one false-success gap. Cluster reconfigure — deliberately refused; whole-cohort runbook instead. 30–60 s failover — configurable yes, default no.

**Suggested order of attack:** (1) run the release train (unblocks everything downstream); (2) MQTT-R1 post-swap barrier; (3) MQTT-L1 poison escape + runbook; (4) always-on failover-budget disclosure; (5) the three metric one-liners (L4/L5 + L2 reclassification); (6) troubleshooting.md corrections + missing runbooks; (7) `doc.go` rewrite + file splits (D1/D2).

---

## 1. Evidence collected

| Evidence | Result |
|---|---|
| `go test -race -count=1 ./...` in `adapters/mqtt/transport/paho` | **PASS**, 0 failures, 34.8 s (`reports/mqtt-paho-race-test.log`) |
| Regression-pin inventory | 20+ `bug_*_test.go` files pin previously found lifecycle/reconcile/overflow bugs; inline code comments carry finding IDs (A-1…A-12, F-1…F-6, HIGH-1/3, M-1…M-4, blocking-#1/2/4, C4/C7), indicating multiple prior review rounds have been folded into code and tests |
| Delivery-guarantee design | Manual acknowledgment (`EnableManualAcknowledgment`): PUBACK/PUBCOMP fires only on `Delivery.Ack` after durable handoff (`delivery.go`, `doc.go`) — true ack-after-durable-handoff for QoS 1/2 on persistent/exclusive sessions |
| Loss observability | Every identified drop path is metered: `MQTTRouterUnmatchedDropped` (orphan cleanup), `MQTTRouterCoveredDropped` (covered QoS 0 overflow), `MQTTRouterCoveredRetained` (retained, NOT lost), `MQTTRouterOverflowDropped` (broker protocol violation), `MQTTRouterStalePurged` (reconnect purge; QoS 1/2 redelivered), `MQTTRouterDropped` (QoS 0 best-effort), `MQTTEventDropped` (lifecycle event eviction) — `metrics.go:1-140` |

---

## 2. Directly verified findings (reviewer's own read)

### MQTT-D1 — `doc.go` contradicts the router on covered-topic handling past grace
`adapters/mqtt/transport/paho/doc.go:52-59` MEDIUM clarity CONFIRMED: the package doc says a still-desired (covered) topic whose handler registers late past the grace window is subject to "(ack-and-drop still applies)". The code does the opposite — and the opposite is correct: `acl_router.go:881-888` (`settleUnmatched`) RETAINS covered publishes un-acked (`retainCovered`, HIGH-1 semantics), and `metrics.go:68-75` states covered QoS 1/2 is "NEVER" ack-dropped. Only true orphans are acked-and-dropped.
**Impact:** an operator reading `doc.go` concludes slow-starting routes lose QoS 1/2 messages after 30 s; they do not. Doc drift in the safety-critical direction (claims worse behavior than reality) — still a trust defect in the primary package contract.
**Fix:** rewrite `doc.go` startup-buffering bullet to match `settleUnmatched`/`retainCovered` semantics: covered ⇒ retained un-acked; orphan ⇒ ack-drop + unsubscribe; covered QoS 0 overflow ⇒ best-effort drop.

### MQTT-D2 — Five files violate the repo's own 500-line hard limit
MEDIUM maintainability CONFIRMED: the project rulebook (user-level CLAUDE.md: "Length of a code file must never exceed 500 lines") is exceeded by: `acl_router.go` (2096), `config.go` (1298), `session_lifecycle.go` (1169), `session_reconcile.go` (1118), `acl_session.go` (618).
**Impact:** the two largest files are also the highest-risk concurrency cores (router dispatch/settlement; reconcile state machine). Size makes the documented lock-order rules (`r.mu` vs `s.mu` vs `reloadGate`) harder to hold in one head; the bug-pin history shows this is exactly where regressions cluster.
**Fix:** mechanical split along existing seams (router: dispatch / pending-buffer / settlement / stats; config: options / validation / defaults). No behavior change.

### MQTT-D3 — Ack issued after a reconnect silently reports success
`adapters/mqtt/transport/paho/acl_router.go:1109-1122` + `delivery.go:33-36` LOW correctness (documented residual) CONFIRMED: when a delivery settles after the underlying connection cycled, `client.Ack` returns `ErrPacketNotFound` and the adapter maps it to **success** — the broker will redeliver, and the duplicate must be absorbed downstream.
**Impact:** correct at-least-once behavior, but it means `Delivery.Ack == nil` does NOT mean "broker slot freed", and duplicate volume after reconnect storms is invisible at this layer (no dedicated metric for `ErrPacketNotFound`-mapped acks).
**Fix (optional hardening):** count these as `MQTTAckAfterReconnect` (or a tag on an existing counter) so operators can correlate duplicate floods with reconnects.

### MQTT-D4 — QoS 0 loss vectors are inherent, deliberate, and all metered
LOW resilience (accepted design) CONFIRMED: QoS 0 can be dropped at four points — dispatch-queue full under flood (`acl_router.go:1250-1258`), pending-buffer overflow during grace (`bufferLocked`), eviction to make room for QoS 1/2 (`evictOldestQoS0Locked`), stale purge on reconnect (`purgeStalePendingLocked`). All are counted; `doc.go:77-85` documents the QoS 0 flood ceiling as a deliberate trade-off ("prefer QoS 1 for traffic that matters").
**Impact:** none for correctly configured routes; operational requirement is alerting on `MQTTRouterDropped`/`MQTTRouterCoveredDropped`.

### MQTT-D5 — Durable modes forbid multi-broker failover URLs (by design; must be understood for HA)
`adapters/mqtt/transport/paho/config_identity.go:15-42` MEDIUM architecture CONFIRMED: `ValidateSessionMode` rejects Persistent/Exclusive sessions configured with more than one canonical broker endpoint — durable managed-subscription history is keyed to a single broker-session domain. Multi-URL failover is only available to Ephemeral sessions.
**Impact:** broker-level HA for durable MQTT sessions must come from a broker cluster behind one stable endpoint (DNS/LB), not from client-side URL lists. Not a bug — but a hard deployment constraint that must be prominent in the docs' HA guidance.

### MQTT-D6 — Ingress poison / oversize packet fail-closes the whole session
`adapters/mqtt/transport/paho/ingress_conn.go:295-305`, `session_lifecycle.go:273-291` LOW resilience PLAUSIBLE (loop bounded by broker compliance): an inbound packet exceeding `max_payload_bytes`/metadata caps latches the session terminal (`transitionTerminal`), the supervisor restarts it, and with `clean_start=false` a broker could redeliver the same oversize QoS 1/2 publish → repeated terminal/restart cycle. The CONNECT packet advertises Maximum Packet Size, so a spec-compliant broker never delivers the oversize packet — the loop is reachable only with a non-compliant broker.
**Impact:** correct fail-closed posture; residual risk is a restart loop against a broken broker, which is observable (`MQTTRouterDropped` + terminal error events) but has no dedicated runbook entry.

---

## 3. Consumability in container platforms (Docker / Kubernetes / ECS)

All findings below were independently re-verified against the repo (tags, go.mod files, Dockerfile, bootstrap sources) before inclusion.

### MQTT-C1 — Library/binary consumption via `go get` is broken at this revision
`RELEASE.md:7-12`, `adapters/mqtt/transport/paho/go.mod:21-23`, `cmd/gobridge/go.mod` (13 `replace` entries) **BLOCKER architecture CONFIRMED**: every published module carries relative `replace` directives and `v0.0.0` requires; only root tags `v0.1.0`/`v0.2.0` exist, no path-prefixed module tags. `RELEASE.md` itself admits: "clean external consumption still fails. Do not present the installation examples as working." The complete release machinery (per-module tag train, replace-stripping stage, external-consumer smoke gate) is documented in `RELEASE.md:130-287` but has never been executed.
**Impact:** GoBridge is currently clone-and-`go.work` only. "Easy to consume as a library" — **no**, until the first compliant release train runs.

### MQTT-C2 — The production binary is image-only and AWS-flavored
`cmd/gobridge/main.go:1-16` **HIGH clarity CONFIRMED**: the only tag-eligible binary module is a self-declared "DEMONSTRATION / REFERENCE binary … NOT the production binary" that rejects non-MQTT/non-native-store configs at startup. The production composition root (`deployment/aws-filebased-config/lib/cmd/gobridge-filebased` — what the `Dockerfile` ships) lives in a module `RELEASE.md` declares internal-only/never-tagged.
**Impact:** non-AWS consumers must write their own composition root to get a production-shaped process. The demo binary is fine for a single MQTT bridge, but it says so loudly in a WARN banner on every boot.

### MQTT-C3 — No metrics path for non-AWS platforms
`deployment/aws-filebased-config/lib/bootstrap/metrics.go:29ff` **HIGH observability CONFIRMED**: shipped exporter options are exactly `noop` and `cloudwatch`. Zero Prometheus hits in any non-test Go file; the OTel adapter modules exist but are not linked into either shipped binary. All the careful loss-accounting metrics of §1 are invisible on plain Kubernetes unless the consumer builds a custom composition root wiring `adapters/otel/metrics`.
**Impact:** the observability story that makes the loss model operable (alert on `MQTTRouterCoveredDropped` etc.) cannot be turned on from the shipped artifacts outside AWS.

### MQTT-C4 — Worst-case shutdown exceeds the default 30 s Kubernetes grace period
`deployment/aws-filebased-config/lib/bootstrap/refs.go:96` (`stopCtx` from fresh `context.Background()`, default 30 s drain) stacked after the app's own 30 s `Stop` budget (`app.go:576`, `app.go:204-205`) **MEDIUM resilience CONFIRMED**: SIGTERM → exit can exceed 60 s worst case. `docs/deployment-guide.md` mandates `terminationGracePeriodSeconds: 60`; a pod left at the K8s default 30 s can be SIGKILLed mid-drain. Delivery guarantees survive (unsettled deliveries fall back to broker redelivery — at-least-once holds), but drained-shutdown intent is defeated.
**Fix:** derive the drain context from the app shutdown context (single budget), or validate/assert the two budgets sum below a configured grace period.

### MQTT-C5 — HA coordination stores are AWS-only
`deployment/aws-filebased-config/lib/bootstrap/config.go:252-269` **MEDIUM architecture CONFIRMED**: exclusive-session leases and `shared_outbox` require a distributed store; the only distributed implementations are DynamoDB-backed. SQLite-over-shared-volume is explicitly rejected in code ("cannot serialize cross-instance writers safely" — correct). On plain K8s the only multi-replica shape is stateless shared-subscription scale-out; active-standby failover is AWS-only today.

### MQTT-C6 — Legacy readiness path breaks HA standbys
`httpapi/monitor.go:99-108,127` **LOW resilience CONFIRMED**: bare `/api/v1/monitor/ready` requires `ServiceLevel Full` and returns 503 for a healthy standby by design. An operator wiring the bare path as a readinessProbe in an active-standby pair keeps the standby permanently unready; the correct `?level=connected` form is documented in the handler comment and the deployment guide, not surfaced as a validation warning.

### MQTT-C7 — Deployment artifacts: none for K8s; image build needs the whole repo
`Dockerfile:41` (`COPY . .`, forced by C1's replace directives), `docs/deployment-guide.md:526-558` **LOW clarity CONFIRMED**: no Helm chart, Kustomize, K8s manifests, or docker-compose exist; the guide provides honest inline snippets labeled "requires your own image". CDK provisioning exists for ECS/EFS/DynamoDB only.

### Positives verified on this dimension
- Image: multi-stage, digest-pinned, distroless static nonroot (`USER 65532`), CGO-free static build, CA certs/tzdata present (TLS MQTT works), self-contained `HEALTHCHECK` via the binary's `-healthcheck` flag — no shell/curl in image.
- Probes: `/live` keyed on runtime terminal state; readiness is genuinely wired to MQTT session health (traced `handleReady → ReadinessLevel → readinessLevelFromSessions`; a disconnected non-standby session caps readiness below `connected` → 503 during broker outage — correct K8s behavior).
- SIGTERM: `signal.NotifyContext` → ordered teardown (watch loop → config manager → HTTP/SSE → transports → runtime) with settle-before-cancel drain; exit 0 clean / 1 error; demo binary exits 2 on second signal.
- Logs: structured JSON to stdout, hot-reloadable level. Config file watching is ConfigMap-symlink-safe (dir-level fsnotify + 30 s hash resync).
- MQTT scale-out identity: `client_id_suffix: hostname|nonce` (`config.go:1209-1213`) gives per-replica-unique client IDs for `$share` groups.

---

## 4. Cluster operation and failover

Key claims re-verified against `bridge/failover_budget.go`, `adapters/mqtt/transport/paho/config_plugin.go:84-105`, `config.go:33-52`, `bridge/builder_prepare.go:436-455`, `bridge/supervisor.go:687-712`.

### MQTT-F1 — Default worst-case exclusive failover is ~336 s (HA profile) to ~10 min (standalone defaults), not 30–60 s
`bridge/failover_budget.go:210` **HIGH resilience CONFIRMED**: failover budget = `leaseTTL + pollBoundaries + acquireCallBudget + postTakeoverActivation + startupAllowance`. With the auto-selected clustered HA profile (lease TTL 45 s, poll 5 s, call timeout 3 s): 45 + 12.5 + 39 + **240** ≈ 336.5 s. The 240 s term is MQTT post-takeover activation — `config_plugin.go:84-105`: 2×`connect_timeout`(30 s) + 4×`reconcile_timeout`(30 s) + 2×`unmatched_grace`(30 s).
**The 30–60 s requirement is achievable but only by explicit tuning** (e.g. `lease_ttl: 15s`, `connect_timeout: 3s`, `reconcile_timeout: 2s`, `unmatched_grace: 2s` ⇒ ~50 s worst case), and declaring `failover_slo: 60s` makes the build fail when the configuration cannot meet it — the correct operator workflow. **Controlling keys:** `lease_ttl`, `acquire_poll_interval`, `renew_call_timeout`, `max_renew_fails`, `startup_allowance`, plus MQTT `connect_timeout`, `reconcile_timeout`, `unmatched_grace`.

### MQTT-F2 — The failover budget is only checked when `failover_slo` is declared
`bridge/failover_budget.go:69-71` **MEDIUM observability CONFIRMED**: `if failoverSLO == 0 { continue }`. An operator who selects `deployment_mode: clustered` gets the 45 s lease TTL and may reasonably assume ~45 s failover; the real ~336 s worst case is stated nowhere at build or runtime unless an SLO was declared. The corrective warning lives in a Go comment (`runtime/session/config.go:141-143`).
**Fix:** always compute and log/expose the worst-case failover budget at startup (info line + deep-health field), independent of `failover_slo`.

### MQTT-F3 — `client_id_suffix: hostname` strands Persistent-mode broker queues on Deployment/ECS reschedules
`adapters/mqtt/transport/paho/config.go:38-45` **HIGH correctness CONFIRMED**: the comment claims hostname is "STABLE across restarts of the same replica". True for StatefulSets; false for K8s Deployments and ECS tasks, where every rollout mints a new pod/task name → new client_id → new broker session. The old session (subscriptions + queued QoS 1/2) is orphaned until `session_expiry_interval` (default 86 400 s) and no other instance can drain it. Only Exclusive mode (stable shared client_id + lease) hands the queue to a survivor.
**Impact:** for Persistent mode on Deployments, every rollout can strand up to 24 h of broker-queued messages per replaced pod — they eventually expire undelivered (loss by timeout, invisible to the bridge).
**Fix:** document Persistent+hostname as StatefulSet-only; recommend Exclusive mode or Ephemeral+`$share` for Deployments; consider a startup warning when `client_id_suffix: hostname` is combined with `session_mode: persistent`.

### MQTT-F4 — Standalone split-brain is warn-only
`bridge/builder_prepare.go:436-455` **MEDIUM architecture CONFIRMED**: two replicas each configured `deployment_mode: standalone` with a process-local lease store each own every exclusive session — real dual consumption. Clustered mode hard-fails on non-distributed stores, but `deployment_mode` is a self-declared assertion decoupled from actual replica count; the defense is a prominent `SPLIT-BRAIN RISK` Warn log (fires for any exclusive-session-on-local-lease config, which is correct). Cannot be closed from inside one process — but the log level makes it easy to miss.
**Fix (docs/ops):** alert on the log message; document that `standalone` + exclusive sessions requires `replicas=1` enforced at the orchestrator.

### MQTT-F5 — Lease loss ⇒ pod restart (single-use paho session)
`runtime/session/manager_lease.go:47-60,905-918` **MEDIUM resilience CONFIRMED**: paho sessions are single-use; a lease step-down closes the session, and re-acquire hits Start-after-Close → `ErrSessionUnrecoverable` → terminal → orchestrator restart. Fail-closed migration paths deliberately retain the lease to natural TTL. Correct for safety; the practical failover bound therefore includes pod-restart latency (budgeted only when `startup_allowance` is set).

### MQTT-F6 — ManagedSubscriptionStore has no fencing of its own
`ports/stores.go:299-303` **MEDIUM correctness PLAUSIBLE**: `List/Remember/Forget` carry no lease token; write-safety rests on the exclusive lease serializing writers plus durable-identity keying. Two Persistent-mode replicas misconfigured with the same effective client_id (same durable identity, no lease) can interleave Remember/Forget and tear down a live filter. Guarded by validation and the takeover-storm symptom, not store-level fencing.

### Verified positives (cluster dimension)
- Lease fencing is genuinely strong: monotonic token versions (Renew never bumps), local-deadline fail-closed step-down before any Current-read mitigation, close-source-before-release ordering, takeover requiring a full TTL of CAS-persisted observation evidence (skew-immune), outbox commits fenced by token version + per-partition high-water-mark. Split-brain **commit** is prevented; duplicate **delivery** during handoff is the documented at-least-once residual.
- Lease-store (DynamoDB) outage ⇒ fail-closed halt of all exclusive consumption cluster-wide until the store returns — no duplicate consumption.
- Scale-out identity: `client_id_suffix: hostname|nonce`; nonce is crypto-random and ephemeral-only; exclusive mode rejects any suffix (stable shared identity is its contract). Takeover storms are damped (first takeover free, then 1 s→64 s exponential penalty, 30 s-stability decay) and `$share`+collision escalates to an Error log on first occurrence.
- Shared subscriptions: supported, `$share` stripped for dispatch, No-Local forced off per spec, broker rejection surfaces as an observable reconcile failure (not silent).
- Durable modes are locked to one broker-session domain (`config_identity.go:20-42`) — client-side multi-URL failover is ephemeral-only, by design (see MQTT-D5).
- Observability: lease transfer/renew/expiry metrics + audit events, `MQTTSessionTakeover`, connect/reconcile latency metrics exist. **Gap:** no end-to-end failover-duration metric and no MQTT ingress duplicate-detection metric (duplicates delegated to downstream idempotency by contract).

---

## 5. Runtime reconfiguration

Architectural headline (verified): GoBridge does **not** hot-patch a running MQTT transport. `Session.Reconcile` runs only within a runtime's lifetime; a config change is a supervisor-driven **full runtime stop → rebuild → start**, and because the MQTT factory declares `CapExclusiveIdentity` (`factory.go:37`), every MQTT-containing config change takes the prepare-commit path: the old runtime is fully stopped **before** the new one is built.

### MQTT-R1 — Reload success is declared before the new session ever reaches the broker; no rollback for broker-invalid configs
`bridge/supervisor.go:1156-1165` + `runtime/bridge_start.go:44-46` **HIGH correctness CONFIRMED**: `applyPrepareCommit` stops the working runtime, builds the new one (paho `NewSession` does not dial), calls `newRt.Start(ctx)` — which "returns immediately" and dials/reconciles in background goroutines — and returns success. The config manager advances the running fingerprint and emits `MetricConfigReloads{success}`. If the new config is syntactically valid but broker-invalid (ACL-denied topic, wrong rotated credentials), the transport is now down, `recoverOldOrWedge` never fires (it covers only synchronous build/complete/Start errors), and every convergence signal reports green. Recovery: per-session supervision retries, the `Terminal()` liveness backstop, or operator revert.
**Failure sequence:** commit config with broker-denied topic → old runtime drained+disconnected → new runtime starts (async) → SUBACK rejection loops in the background → reload reported successful.
**Fix directions:** post-swap health barrier (hold the success ack until sessions reach `connected`/`subscribed` within a budget, else auto-revert), or at minimum surface "applied-but-not-converged" as a distinct state in `MetricConfigDegraded`/deep-health (today the divergence is visible only in session-level health, not as running≠desired).

### MQTT-R2 — Every MQTT config change is a full-transport outage window
`bridge/supervisor.go:899-903,1263-1271` **HIGH correctness CONFIRMED (by design)**: any reload → SwapPrepareCommit → all MQTT sessions disconnect for drain(≤30 s default) + build + dial + CONNACK + SUBACK. During the window: persistent/exclusive QoS 1/2 is broker-queued and replayed (no loss); **QoS 0 on any topic is lost; ephemeral sessions lose everything published in the window** (broker discards the session at disconnect).
**Impact:** "reconfigure while running without dropping messages" holds **only** for QoS 1/2 on clean_start=false sessions. Docs must state this as a controlled-restart semantic, not a hitless reload.

### MQTT-R3 — One permanently rejected subscription downs the whole exclusive session forever
`session_reconcile.go:99-106,755-774` + `acl_session.go:65-95` **MEDIUM resilience CONFIRMED**: partial SUBACK failure (or QoS downgrade) fails the reconcile; for exclusive sessions the deferred handler disconnects the generation and releases the lease; supervision retries forever at the 30 s backoff cap. A permanent broker-side denial of one topic ⇒ indefinite connect→subscribe→reject→disconnect churn; the healthy sibling routes on that session never stay up. Fail-closed, observable (`MetricReconcileFailures`, readiness below Full), never a silent partial — but no per-topic quarantine and no self-heal. (Same finding surfaced independently by the resilience track — see MQTT-O2.)

### MQTT-R4 — No reload debounce by default
`bridge/supervisor.go:293` **MEDIUM resilience CONFIRMED**: default `ReconfigStrategy` is `NewDirectStrategy()`; N rapid config writes = up to N full swap outage windows (MQTT-R2 each time). `DebouncedStrategy`/`WindowedStrategy` exist but are opt-in. Storms do converge to the last config (latest-wins watch loop, content-equal no-op, `lifecycleMu` serialization).

### MQTT-R5 — Unmanaged wildcard-route removal leaks the broker subscription forever
`session_reconcile.go:298-313` **MEDIUM correctness CONFIRMED (documented residual)**: across a restart/swap, a removed **wildcard** subscription on an unmanaged persistent session survives on the broker; orphan cleanup unsubscribes only exact concrete topics, so the wildcard's traffic is delivered, acked, and dropped forever (`MQTTRouterUnmatchedDropped` rises). The managed-subscription store exists precisely to close this — **when configured**.
**Fix (docs/ops):** make the managed store effectively mandatory for persistent sessions with wildcard filters; alert on steadily rising `MQTTRouterUnmatchedDropped`.

### MQTT-R6 — Managed route-removal can deliberately brick the session until config revert
`session_lifecycle.go:686-694` **MEDIUM resilience CONFIRMED (by design)**: a broker-pinned QoS 1 delivery matching a removed filter detected during managed cleanup latches the session terminal with an error instructing: restore the old configuration, drain, retry the cutover. Fail-closed against loss — correct — but a reload can end in a state only a revert fixes; the runbook must be prominent.

### Verified positives (reconfiguration dimension)
- Two-phase validation of everything knowable pre-broker: invalid configs dropped keeping last-good; durable-identity changes (client_id/broker URL/mode/expiry) **refused** on live reload before touching the old runtime (`supervisor.go:716-736`); lease session_id renames refused; clustered deployments refuse per-process live reload entirely with a documented whole-cohort runbook (`supervisor.go:687-710`).
- Credential rotation: validated against the plaintext gate before mutation, applied via full Reload (one reconnect; QoS 1/2 continuity via clean_start=false); reactive re-resolve on CONNACK 0x86/0x87; the Reload-fails-while-broker-down zombie (F-1) self-heals via events-close → supervised re-Start (pinned by `bug_reload_events_test.go`).
- Old-runtime stop settles accepted in-flight deliveries before cancelling; anything unsettled is left un-acked for broker redelivery — never silently acked.
- Session-scope convergence machinery is genuinely sound: single `reloadGate`, `connEpoch` guards against reconnect-interleaved reconciles, applied-plan history retries failed unsubscribes, reclassify-pending prevents wedged retained publishes (all pinned by regression tests).
- Failed-reload observability is strong for synchronous failures: reload metrics, degraded gauge, running/applied version fingerprints, `GET /api/v1/admin/config` (effective redacted config), distinct commit outcomes. The blind spot is exactly MQTT-R1's async class.

---

## 6. Outage resilience (broker outages, partitions, credential expiry, slow brokers)

### MQTT-O1 — In-flight egress QoS 1/2 is lost on process crash; QoS 2 is not exactly-once across restart
`adapters/mqtt/transport/paho/acl_session.go:329-351` **HIGH resilience CONFIRMED (documented ceiling)**: the session leaves autopaho's `cfg.Session` nil ⇒ default **in-memory** packet store; un-acked outbound PUBLISH/PUBREL state dies with the process (`client_id`/`clean_start=false` resume broker-side state only). The code documents this as deferred finding M-6/HIGH-5, and the production contract routes durable egress through the bridge outbox (`shared_outbox`/idempotent replay); `Sender.NonDurableEgress()` reports the boundary and both wired delivery modes are loss-safe today.
**Residual:** a hand-wired direct QoS≥1 route without an outbox (library consumers) silently loses in-flight egress on crash. The deferred alternative (file-backed `session.SessionManager`) remains unimplemented.

### MQTT-O2 — Permanent SUBACK rejection / QoS downgrade never converges
`session_reconcile.go:755-774` **MEDIUM resilience CONFIRMED**: same defect class as MQTT-R3 seen from the outage lens — a broker granting QoS 0 for a requested QoS 1, or rejecting one filter, produces an indefinite 30 s-capped restart flap; `ServiceLevel` never reaches Full; operator intervention required. Observable but not self-healing.

### MQTT-O3 — Circuit-breaker sender uses the concurrency-unsafe token-less API
`cb_sender.go:60-70` vs `circuitbreaker/breaker.go:169,194` **MEDIUM resilience PLAUSIBLE→CONFIRMED usage mismatch**: the breaker's own doc says concurrent callers "should use BeforeRequestToken / AfterRequestToken"; `CircuitBreakerSender` uses `BeforeRequest`/`AfterRequest` while route `max_in_flight` drives concurrent sends. A late outcome from a pre-transition request is accounted against the current generation (stale probe release / spurious re-open). Breaker fidelity only — no data loss; the breaker is also optional (not wired by default for MQTT egress).
**Fix:** switch `cb_sender.go` to the token API (mechanical).

### MQTT-O4 — Event-channel eviction can defer a reconnect reconcile one connect cycle
`session_health.go:190-219` **LOW resilience CONFIRMED (by design)**: under a disconnect/reconnect storm the 16-slot event channel's drop-oldest eviction can discard an unconsumed `SessionConnected`; subscriptions re-establish on the following connect edge. Bounded, metered (`MQTTEventDropped` — alert if non-zero in steady state).

### MQTT-O5 — Error classification by substring match on paho error strings
`errors.go:34-52` **LOW resilience CONFIRMED**: typed checks first, then case-insensitive substring matching with a documented upgrade checklist (F-10). A paho version bump can silently reclassify errors into the `ErrUnavailable` fallback and change retry behavior. Keep the checklist in the release procedure.

### MQTT-O6 — Unbounded `Disconnect(context.Background())` on cleanup paths
`acl_session.go:247-248,264,560` **LOW resilience CONFIRMED (mitigated)**: preceded by `cmCancel()` (cancels the CM root context) so Disconnect should return promptly; residual only if the SDK ignores the cancelled context.

### Verified positives (outage dimension)
- Reconnect: equal-jitter exponential (base 10 s, cap 2 m, factor 2, jitter [d/2, d)), **retries forever**, jittered at both the MQTT layer and the supervision restart layer — thundering herd addressed; backoff resets naturally on success.
- Every network op bounded: keepalive 30 s, per-attempt reconnect 30 s, connect 30 s, each SUBSCRIBE/UNSUBSCRIBE 30 s (floor-coerced, cannot be disabled), publish ≤ configured sender timeout (30 s via factory; 60 s safety net only with no deadline). A publish cannot block forever. A ctx-ignoring wedged reconcile is escalated by an independent goroutine-raced ceiling to `ErrSessionUnrecoverable` → pod restart (caps parked goroutines at one).
- Reconnect correctness: `activeSubs` reset to empty before `SessionConnected`; full authoritative re-subscribe every connect edge; `connEpoch` invalidates stale write-backs; partial SUBACKs and short SUBACKs are failures, never silently accepted.
- Ingress across outage: un-acked QoS 1/2 redelivered by broker (at-least-once); stale pending purged per epoch and metered; covered topics retained un-acked.
- Credentials: MQTT authenticates only at CONNECT (expiry-while-connected is benign); revocation → CONNACK 0x86/0x87 → rate-limited reactive re-resolve; repeated refresh failure degrades observably and retries forever — no stall.
- Health: outage ⇒ `Connected=false`, `ServiceLevel None`, readiness red; **`/live` never trips on a transient outage** (only `ErrSessionUnrecoverable` escalates to terminal → restart). Correct K8s semantics.
- Hygiene: no per-reconnect goroutine leaks (grace/dispatch workers started once, timer-reset re-arm); pending buffer bounded by count (receive_maximum) + 64 MiB QoS 0 byte ceiling; recovery goroutines coalesced and rate-limited (30 s min interval); lock-order rules (`r.mu` never → `s.mu`; OnConnectionUp never takes `reloadGate`; `s.mu` never held across broker round-trips) verified honored; race-detector suite green.

---

## 7. Documentation

The reference material is unusually accurate: **every config key and every default in `docs/transports/mqtt.md` matches the code exactly** (verified against `config.go:383-728`), the ingress-memory worked example is arithmetically exact, and the delivery-guarantee section is honest to the point of bluntness (QoS 2 not exactly-once across restart, ephemeral loss windows, per-route guarantee matrix). Scenario docs 01/03 are copy-paste runnable. The Docker/K8s/ECS section of `deployment-guide.md` (probes with `?level=` semantics, shutdown sequence, exit codes, ConfigMap-safe mounting) is production-ready. The defects are concentrated in `troubleshooting.md` — the operator's first stop:

### MQTT-DOC1 — `troubleshooting.md:465` documents the wrong takeover reason code
**HIGH correctness CONFIRMED**: claims `MQTTSessionTakeover` fires on `0x8E/0x8F` ("session taken over"). `0x8F` is Topic Filter Invalid, explicitly NOT counted (`session_lifecycle.go:1040,1053-1057`, `metrics.go:127-128`). Wrong reason code in the exact failure mode the entry exists to diagnose. Also cites `session_lifecycle.go:189,223` — emission is at `:1107`.

### MQTT-DOC2 — `troubleshooting.md:463` documents a dispatch queue size that doesn't exist
**HIGH correctness CONFIRMED**: claims `defaultDispatchSize=1024`; code is `int(DefaultReceiveMaximum)` = **192**, overridden by effective `receive_maximum` per session (`acl_router.go:299`, `config.go:411`). Contradicts `mqtt.md:735` (which is correct). Operators sizing backpressure against 1024 mis-tune by 5×.

### MQTT-DOC3 — Six emitted metrics have no operator documentation
**MEDIUM completeness CONFIRMED**: `MQTTPublishFailures` (the primary egress-error counter!), `MQTTPublishLatency`, `MQTTHandlerPanics`, `MQTTReconcileLatency`, `MQTTEventDropped`, `MQTTRouterStalePurged` appear in no doc with meaning/alert guidance. 21 metrics emitted; ~15 documented.

### MQTT-DOC4 — `doc.go` stale covered-topic claim (= MQTT-D1)
**MEDIUM correctness CONFIRMED** independently by the docs track: `mqtt.md:141` links to `paho/doc.go` as the authoritative mechanism description, and that file's "(ack-and-drop still applies)" contradicts both the code and `mqtt.md`'s own (correct) description.

### MQTT-DOC5 — Wrong code cross-references
**LOW correctness CONFIRMED**: `mqtt.md:336` cites `factory.go:59-66` for client_id/broker-URL requirements (actual: `config_plugin.go:177-193`); `scenarios/18-observability.md:97` says `MQTTRouterDropped` is untagged (it carries `session_id`). Plus: `MQTTConnectLatency` is declared (`metrics.go:10`) but never emitted — a latent trap for future alerting.

### MQTT-DOC6 — Missing operator content
**MEDIUM completeness CONFIRMED**: no troubleshooting entry for the fail-closed plaintext-credentials startup error (a likely first-run failure); no MQTT-specific capacity/throughput runbook (receive_maximum/max_in_flight/payload → msg/s); no consolidated MQTT log-event catalogue; no runbook for the ingress-poison terminal loop (MQTT-D6) or the managed-migration brick-until-revert state (MQTT-R6); failover-budget arithmetic (MQTT-F1) not surfaced in the HA docs (the honest "measurements required" caveat exists in `mqtt.md:84-119`, but the ~336 s default worst case is stated nowhere).

---

## 8. Message loss and delivery guarantees

Highest-severity claims re-verified: `runtime/session/manager.go:244-252` (Start before first Reconcile), `config.go:431-434` (`maxIngressUserProperties = 128`), `acl_session.go:474-492` (clean_start warning asymmetry).

### MQTT-L1 — Publisher-triggerable poison loop: a spec-compliant publish can permanently kill the session
`acl_router.go:1128-1173` + `session_lifecycle.go:273-275` **HIGH resilience CONFIRMED**: a QoS 1 publish with >128 user properties (`maxIngressUserProperties`, `config.go:434`) is small, within the CONNECT-advertised Maximum Packet Size, and forwarded by any compliant broker — user-property *count* is a purely local cap the broker cannot enforce. The router's `rejectIngressPacket` fires `ingressPoison` → `transitionTerminal`: the session latches terminal, `Start` refuses forever in-process. The packet is never acked and never DLQ'd; after supervisor/process restart the broker **redelivers the same packet** on session resume → terminal again, indefinitely. All routes sharing the session are head-of-line blocked at the broker. Observable (Error log, `MQTTRouterDropped`, `SessionError`) but there is **no automated escape** — no ack-and-DLQ path for a poison ingress packet.
This supersedes MQTT-D6's assessment: the trigger does **not** require a non-compliant broker; any authorized publisher can induce it (accidentally or deliberately — a DoS vector against the bridge).
**Fix:** for cap violations that are *representational* (user-property count, metadata size) rather than memory-unsafe (packet/payload size already rejected pre-decode), prefer ack-and-DLQ (or ack-drop + dedicated poison metric) over session-terminal; at minimum, add a runbook for breaking the loop (publish removal / session purge on broker, or temporary cap raise).

### MQTT-L2 — Live backlog can be acked-dropped and its topic unsubscribed before the first Reconcile of a process lifetime
`session_reconcile.go:423-447` (`topicCoveredLocked`) + `runtime/session/manager.go:244-252` **HIGH correctness PLAUSIBLE (narrow trigger, verified structure)**: `Manager.Run` calls `Start` first, `Reconcile` second; `s.plan` is stashed only inside Reconcile. Between CONNACK (where a persistent broker replays the offline QoS 1/2 backlog) and the plan stash, `topicCoveredLocked` covers nothing (activeSubs reset on connect-up, plan nil, managed history empty when no managed store is configured). If the first Reconcile is delayed past `unmatched_grace` (30 s) — reloadGate held by a concurrent operation, or the `SessionConnected` event evicted under an event storm — the grace sweep classifies live backlog as ORPHAN: **PUBACKed, dropped, and the live topic UNSUBSCRIBED** until a later reconcile re-subscribes.
Worse, the loss is **miscategorized**: counted on `MQTTRouterUnmatchedDropped`, which `metrics.go:43-58` documents as "BENIGN cleanup" — an operator alerting per the metric docs will not treat it as loss.
**Fix:** treat "no plan has ever been stashed this process lifetime" as covered-everything (retain, don't orphan-drop) until the first Reconcile stashes a plan; the managed-subscription store already closes this for managed sessions.

### MQTT-L3 — Emit-error strands un-acked deliveries with no in-process recovery
`receiver.go:126-137` **HIGH resilience CONFIRMED**: when `emit` returns an error, `Receiver.Run` cancels and returns without settling the triggering delivery. The session stays connected; MQTT brokers do not redeliver on a live connection; nothing on this path calls `requestRecovery`. The un-acked packet head-of-line-blocks paho's contiguous-prefix ack stream; as un-acked slots accumulate toward `receive_maximum`, ingress wedges. Only a connection teardown (from any other cause, or supervisor escalation → restart) releases it.
Observable indirectly — `MQTTUnsettled`, `MQTTOldestUnsettledAge`, `MQTTReceiveWindowUtilization` rise — but no explicit "stranded delivery" event, and no automated recycle.
**Fix:** on emit-error exit, either settle-with-Retry (request a bounded session recycle, as durable `Delivery.Retry` already does) or emit a dedicated stranded-delivery warning tying the wedge to its cause.

### MQTT-L4 — Discard-mode ingress drop is the router's only fully silent drop branch
`acl_router.go:1236-1239` (and the `dispatchCore` twin at `:1324-1328`) **MEDIUM observability CONFIRMED**: during a recovery recycle (`discarding=true`), publishes still arriving on the old socket are released with **no metric, no log, no ack** — in contrast to the adjacent epoch-mismatch branch which counts `MQTTRouterStalePurged`. QoS 1/2 is redelivered by the resumed session (safe); **QoS 0 is silently lost**.
**Fix:** count the discard branch on `MQTTRouterStalePurged` (or a `reason=discarding` tag).

### MQTT-L5 — Ack-after-reconnect: silent duplicate window that also falsely clears the unsettled gauge
`acl_router.go:1109-1122` **MEDIUM correctness CONFIRMED** (upgrades MQTT-D3): `ErrPacketNotFound` → mapped to success → `trackAcknowledgement` removes the entry from the unsettled map, the RouteRunner records a successful ack, outbox/ledger evict — while the broker is guaranteed to redeliver. On a `direct_hold` route with no downstream dedup this is a duplicate egress with zero signal; neither metric nor log records the event.
**Fix:** count these (`reason=ack_after_reconnect`) — the information is available at exactly that branch.

### MQTT-L6 — `session_mode: persistent` + `clean_start: true` silently wipes the offline backlog every restart
`acl_session.go:474-492` **MEDIUM correctness CONFIRMED**: Exclusive+CleanStart is overridden to false with a warning; Persistent+CleanStart=true is honored silently — every process restart discards broker-queued QoS 1/2. Config-as-requested, but the analogous `SessionExpiryInterval=0` misconfiguration *does* warn (`session.go:299-307`); this one should too.

### MQTT-L7 — Retry = whole-session recycle; duplicates unmeasured; Session-Present=false goes terminal
`session_lifecycle.go:370-408,937-949` **MEDIUM resilience CONFIRMED (by design)**: a durable QoS 1/2 `Delivery.Retry` triggers an async, rate-limited (30 s min interval) session recycle that redelivers **every** unsettled delivery on the shared session (duplicates for innocent in-flight messages; dedup only on `shared_outbox` routes). If the broker answers Session Present=false (state lost/expired during the outage) the recovery correctly refuses to fake continuity and goes terminal. Recycles are counted (`MQTTSessionRecoveryRecycle`); the induced duplicates are not.

### MQTT-L8 — Shutdown abandons queued work silently
`acl_router.go:619-629` (dispatchLoop exit), `:1826-1829` (flush under discarding), `:1794-1801` (takePendingLocked under closing) **LOW observability CONFIRMED**: at Close, buffered `dispatchCh` items and flush-taken pending entries are released without emit or counter. QoS 1/2 redelivered to the next session owner (safe); QoS 0 lost, uncounted. Close-time only.

### MQTT-L9 — QoS 0 egress loss is invisible by construction and unflagged by the durability reporter
`sender.go:163-165` **LOW observability CONFIRMED**: `NonDurableEgress()` returns false for QoS 0 ("makes no delivery claim"), so the bridge's egress-durability advisory machinery raises nothing, and a socket-death loss after a successful write has no signal. Protocol-inherent; the gap is only that no startup advisory says "this route's egress is fire-and-forget".

### Verified positives (delivery-guarantee dimension)
- The core at-least-once claim **holds** for QoS 1/2 on persistent/exclusive sessions against a compliant broker: `EnableManualAcknowledgment` + ack-only-from-`Delivery.Ack` after outbox persist/broker accept (every terminal path in `runtime/route/dispatch.go` converges on ack-after-durable-handoff); crash windows resolve to broker redelivery + outbox version-fence dedup (ADR 0009); covered topics are retained un-acked, never ack-dropped; epoch stamping purges stale twins; a crash between bridge-settle and PUBACK **cannot lose** (settle happens only after `Send` returned, and `Send` blocks until PUBACK/PUBCOMP) — the reverse window yields a fenced duplicate.
- Backpressure is deadlock-free and bounded end-to-end: QoS 1/2 blocks (bounded by broker Receive-Maximum flow control), QoS 0 sheds (metered), blocked callbacks wake on `queueChanged`/`stop`.
- The historical bug fixes (pending-overflow QoS asymmetry, epoch purge, covered-retention) are structurally complete in current code; the surviving siblings the fixes missed are exactly MQTT-L2 (covered() nil-plan blind spot) and MQTT-L4 (unmetered discard twin of the metered stale-purge).
- Timeout coverage: no timeout path acks without processing — every expiry resolves to un-acked→redelivery or terminal→`SessionError`.

---

## 9. Consolidated answers to the review questions

**1. Is the code production ready?**
**Conditional yes — the MQTT adapter itself is unusually hardened; the packaging around it is not.** The adapter core (ack-after-durable-handoff, epoch-guarded reconcile, jittered forever-reconnect, bounded everything, fenced leases) shows evidence of multiple absorbed review cycles and passes its race suite. What blocks "production ready" as a product: the release train has never run (MQTT-C1, `go get` fails), the production binary is image-only/AWS-flavored (MQTT-C2), non-AWS metrics don't exist in shipped artifacts (MQTT-C3), and the reload-success-before-broker-truth gap (MQTT-R1).

**2. Is the documentation production ready?**
**Conditional yes.** Reference material is exceptional — every config key and default verified accurate, delivery guarantees honest, container ops section complete. Blockers to "operator with docs alone": two factual errors in `troubleshooting.md`'s takeover/backpressure entries (MQTT-DOC1/2), six undocumented metrics including the primary egress-error counter (MQTT-DOC3), the stale `doc.go` contract text (MQTT-D1), and missing runbooks for the three "stuck until operator acts" states (poison loop, managed-migration brick, permanent SUBACK rejection).

**3. Do we have zero bugs?**
**No.** Confirmed defects: MQTT-L1 (publisher-triggerable terminal loop), MQTT-L3 (stranded un-acked deliveries), MQTT-L4 (silent discard drop), MQTT-L5 (unsettled gauge falsely cleared + silent duplicate), MQTT-R1 (reload false-success), MQTT-F3 (hostname-suffix stranding on Deployments), MQTT-O3 (breaker token API misuse), MQTT-L6 (missing clean_start warning), plus the doc bugs. One high-impact PLAUSIBLE: MQTT-L2 (pre-reconcile orphan sweep). None of these is in the steady-state hot path — the race suite and 20+ regression pins have scrubbed that — they live at the edges: first-connect, poison input, reload-commit, operator misconfiguration.

**4. Can this run in a cluster?**
**Yes, in exactly two shapes; no in others.** (a) Exclusive-lease single-active (active/standby) on a distributed lease store — DynamoDB only today (MQTT-C5); fencing is genuinely sound (split-brain *commit* prevented; duplicate *delivery* during handoff is documented at-least-once). (b) Ephemeral + `$share` scale-out with per-replica `client_id_suffix`. **Not**: naive N-replica Persistent consumption (per-replica broker sessions strand queues on reschedule — MQTT-F3), and `standalone`-declared replicas on a local lease store are real split-brain with only a Warn log (MQTT-F4).

**5. Is the code resilient — outages and coming back on track?**
**Yes — confirmed.** Jittered exponential forever-reconnect (both MQTT and supervision layers), every network op bounded, full authoritative re-subscribe from empty state per connect edge, epoch guards against stale write-backs, credential revocation handled reactively, liveness never trips on transient outages, no goroutine/memory leaks across reconnect cycles, F-1 zombie self-heals. The two non-self-healing states are not outages: permanent broker subscription disagreement (MQTT-O2 flap-forever) and the poison loop (MQTT-L1).

**6. Do we miss/lose messages — and how is it recorded/reported/handled?**
**QoS 1/2 on persistent/exclusive sessions: no loss** in steady state, crash, reconnect, or reload — resolved by broker redelivery + outbox fencing; the deliberate exceptions are metered and loud (orphan cleanup `MQTTRouterUnmatchedDropped`, broker-protocol-violation `MQTTRouterOverflowDropped` with "MESSAGE LOST" warn). Timeouts everywhere resolve to redelivery-or-terminal, never ack-without-processing.
**QoS 0: droppable on ~8 paths**, mostly metered (`MQTTRouterDropped`, `MQTTRouterCoveredDropped`), by protocol contract.
**The gaps:** two silent drop paths (recycle-window discard MQTT-L4, shutdown abandonment MQTT-L8 — QoS 0 only), one mislabeled loss (MQTT-L2 counted as benign cleanup), one silent duplicate window (MQTT-L5), duplicates from session-recycle unmeasured (MQTT-L7), in-flight egress dies with the process absent the outbox (MQTT-O1 — both shipped delivery modes are covered; hand-wired library routes are not), and Persistent-mode broker-side queues stranded by identity churn expire invisibly (MQTT-F3).

**7. Easy to consume as a single standard process on Docker/K8s/ECS?**
**The process: yes** — distroless nonroot static image, self-contained healthcheck, JSON stdout logs, correct MQTT-aware probes, drain-then-exit SIGTERM. **The consumption: not yet** — `go get` broken (MQTT-C1), production composition root unpublished (MQTT-C2), no non-AWS metrics (MQTT-C3), shutdown can exceed default K8s 30 s grace unless the documented 60 s is applied (MQTT-C4), no Helm/manifests (MQTT-C7), ~6–7 manual steps from zero.

**8. Can we reconfigure it while running, resiliently?**
**As a controlled restart, yes; as a hitless reload, no.** Every MQTT config change is a full stop→rebuild→start (outage window; QoS 1/2 clean_start=false traffic bridged by broker queuing — zero loss, possible duplicates; QoS 0 and ephemeral traffic in-window lost — MQTT-R2). Everything knowable pre-broker is validated two-phase with fail-closed refusals (identity changes, backlog stranding). The genuine resilience gap is MQTT-R1: a broker-invalid config commits as success after the working runtime is already gone, with green convergence telemetry. Secondary: no debounce by default (MQTT-R4), one rejected topic downs its whole exclusive session indefinitely (MQTT-R3), managed route-removal can brick-until-revert by design (MQTT-R6). Operator rule: verify session health after every reload; the reload success signal alone is insufficient.

**9. Can we reconfigure the cluster while running, resiliently?**
**Deliberately no — and that is the resilient answer.** Per-process live reload of a clustered deployment is refused fail-closed (`supervisor.go:687-710`) because a rolling reload would split the cohort across config versions with no version barrier; the supported procedure is an externally coordinated whole-cohort stop/drain/deploy/start (`docs/runbooks/cluster-config-rollout.md`). Rolling restarts with an *unchanged* config are supported (lease transfer + connect-after-lease standbys). Durable-identity and lease-name changes are refused on any live reload.

**10. Does it fail over in a cluster within a configurable 30–60 s?**
**Configurable: yes. Default: no — ~336 s worst case with the clustered HA profile, ~10 min with standalone defaults** (MQTT-F1). The budget is lease TTL + poll boundaries + acquire-call budget + MQTT post-takeover activation (240 s of the default total: 2×connect + 4×reconcile + 2×grace). A ~50 s worst case is reachable with explicit tuning, and declaring `failover_slo: 60s` makes the build **fail** if the config cannot meet it — the right workflow, but it is opt-in and the default silence is a trap (MQTT-F2). Practical bound also includes pod-restart latency when a lease-losing instance must recycle (MQTT-F5).

---

## 10. Consolidated issue register

| ID | Severity | Status | One-line |
|---|---|---|---|
| MQTT-C1 | BLOCKER | CONFIRMED | `go get` consumption broken: replace directives + no module tags; release train never run |
| MQTT-R1 | HIGH | CONFIRMED | Reload success committed before new session reaches broker; no rollback for broker-invalid configs |
| MQTT-L1 | HIGH | CONFIRMED | Spec-compliant publish (>128 user props) → permanent terminal-session poison loop; publisher-triggerable |
| MQTT-L3 | HIGH | CONFIRMED | Emit-error strands un-acked deliveries; no in-process recovery; ingress can wedge |
| MQTT-F1 | HIGH | CONFIRMED | Default worst-case failover ~336 s (HA) / ~10 min (standalone); 30–60 s only via explicit tuning + `failover_slo` |
| MQTT-F3 | HIGH | CONFIRMED | Persistent + `client_id_suffix: hostname` strands broker queues on every Deployment/ECS rollout |
| MQTT-O1 | HIGH | CONFIRMED (documented) | In-flight egress QoS 1/2 lost on crash (in-memory autopaho store); safe only via bridge outbox modes |
| MQTT-C2 | HIGH | CONFIRMED | Production binary image-only + AWS-flavored; published binary is demo-only |
| MQTT-C3 | HIGH | CONFIRMED | No metrics exporter for non-AWS platforms in shipped artifacts |
| MQTT-L2 | HIGH | PLAUSIBLE | Pre-first-Reconcile orphan sweep can ack-drop live backlog + unsubscribe live topic; counted as benign |
| MQTT-DOC1 | HIGH | CONFIRMED | troubleshooting.md: wrong takeover reason code (0x8F) in the takeover diagnosis entry |
| MQTT-DOC2 | HIGH | CONFIRMED | troubleshooting.md: nonexistent `defaultDispatchSize=1024` (actual 192) |
| MQTT-R2 | HIGH | BY DESIGN | Every MQTT config change = full-transport outage window; hitless only for QoS 1/2 durable traffic |
| MQTT-F2 | MEDIUM | CONFIRMED | Failover budget validated only when `failover_slo` declared; default silence misleads |
| MQTT-F4 | MEDIUM | CONFIRMED | Standalone split-brain (2 replicas, local lease) defended by Warn log only |
| MQTT-F5 | MEDIUM | CONFIRMED | Lease loss ⇒ pod restart (single-use session); extends practical failover bound |
| MQTT-F6 | MEDIUM | PLAUSIBLE | ManagedSubscriptionStore has no fencing; safety rests on lease + identity keying |
| MQTT-R3/O2 | MEDIUM | CONFIRMED | One permanently rejected subscription → indefinite whole-session churn; no per-topic quarantine |
| MQTT-R4 | MEDIUM | CONFIRMED | No reload debounce by default; N config writes = N outage windows |
| MQTT-R5 | MEDIUM | CONFIRMED | Unmanaged wildcard-route removal leaks broker subscription forever |
| MQTT-R6 | MEDIUM | BY DESIGN | Managed route-removal can brick session until config revert (fail-closed; needs runbook) |
| MQTT-O3 | MEDIUM | CONFIRMED | CircuitBreakerSender uses concurrency-unsafe token-less breaker API |
| MQTT-L4 | MEDIUM | CONFIRMED | Recycle-window discard drop: only fully silent drop branch in router (QoS 0 lost, uncounted) |
| MQTT-L5 | MEDIUM | CONFIRMED | Ack-after-reconnect mapped to success: silent duplicate + falsely cleared unsettled gauge |
| MQTT-L6 | MEDIUM | CONFIRMED | Persistent+clean_start=true silently wipes offline backlog each restart (no warning) |
| MQTT-L7 | MEDIUM | BY DESIGN | Retry recycles whole session; induced duplicates unmeasured; Session-Present=false → terminal |
| MQTT-C4 | MEDIUM | CONFIRMED | Worst-case shutdown exceeds default K8s 30 s grace (stacked budgets) |
| MQTT-C5 | MEDIUM | CONFIRMED | HA lease/outbox stores are DynamoDB-only; plain-K8s active/standby impossible |
| MQTT-D1/DOC4 | MEDIUM | CONFIRMED | doc.go covered-topic text contradicts code (claims ack-drop; code retains) |
| MQTT-D2 | MEDIUM | CONFIRMED | 5 files exceed repo's 500-line limit incl. both concurrency cores |
| MQTT-D5 | MEDIUM | BY DESIGN | Durable modes locked to one broker-session domain; HA must be broker-side |
| MQTT-DOC3 | MEDIUM | CONFIRMED | 6 emitted metrics undocumented incl. `MQTTPublishFailures` |
| MQTT-DOC6 | MEDIUM | CONFIRMED | Missing runbooks: poison loop, migration brick, SUBACK flap, plaintext-cred error, capacity |
| MQTT-D3 | LOW | CONFIRMED | (folded into MQTT-L5) |
| MQTT-D4 | LOW | BY DESIGN | QoS 0 loss vectors inherent + metered |
| MQTT-D6 | LOW | superseded | (superseded by MQTT-L1) |
| MQTT-O4 | LOW | BY DESIGN | Event eviction defers reconcile one connect edge (metered) |
| MQTT-O5 | LOW | CONFIRMED | Error classification by substring match on paho strings (pinned + checklisted) |
| MQTT-O6 | LOW | MITIGATED | Unbounded Disconnect on cleanup (preceded by ctx cancel) |
| MQTT-L8 | LOW | CONFIRMED | Shutdown abandons queued QoS 0 silently (Close-time only) |
| MQTT-L9 | LOW | CONFIRMED | QoS 0 egress loss invisible; no advisory flags fire-and-forget routes |
| MQTT-C6 | LOW | CONFIRMED | Legacy bare `/ready` path keeps HA standbys permanently unready |
| MQTT-C7 | LOW | CONFIRMED | No K8s deployment artifacts; image build needs whole repo |
| MQTT-DOC5 | LOW | CONFIRMED | Wrong code cross-refs in docs; `MQTTConnectLatency` declared but never emitted |

