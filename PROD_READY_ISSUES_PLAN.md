# MQTT Production-Readiness Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> `superpowers:subagent-driven-development` or
> `superpowers:executing-plans` to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve every in-repository blocker and high-severity defect in
`PROD_READY_ISSUES.md`, make the documented MQTT production contract truthful,
and leave reproducible release gates for the external operations that cannot be
performed safely inside one source pull request.

**Architecture:** Keep transport-specific recovery and broker identity inside
the Paho adapter, expose only optional generic capabilities through ports, and
fail closed where durable identity, delivery guarantees, or clustered
coordination cannot be proven. Reuse existing stores, factories, clocks,
metrics, deployment constructs, and test helpers. Prefer contract narrowing
over speculative schedulers or consensus systems.

**Tech Stack:** Go 1.25+, Eclipse Paho MQTT v5, DynamoDB, SQLite, AWS CDK/ECS,
Docker BuildKit, GitHub Actions, Mosquitto-backed integration tests.

**Specification:** `PROD_READY_ISSUES.md`

---

## Execution rules

1. Complete tasks in order; later tasks depend on contracts introduced earlier.
2. For each behavior change, write the smallest failing test first and prove the
   failure is caused by the finding.
3. Run the focused command before committing each task.
4. Do not weaken `.go-arch-lint.yml`, skip repository hooks, or hide failures.
5. Do not publish tags, releases, images, or cloud stacks from this branch.
   Those actions are listed under **External release sequence**.
6. A task is complete only when its tests, docs, and audit status are updated in
   the same commit.

## Task 1: Make release-gate tests trustworthy

**Status:** Pending

**Agents/Skills:** thiink:golang-pro, test-automator, thiink:test-reviewer

**Files:**
- Modify: `Makefile`
- Modify: `tests/longrunning/longrunning_test.go`
- Modify: `tests/longrunning/longrunning_helpers_test.go`
- Modify: `tests/longrunning/uc42_broker_resilience_test.go`
- Modify: `tests/longrunning/uc48_broker_multihop_test.go`
- Modify: `tests/integration/e2e_helpers_test.go`
- Modify: `.github/workflows/ci.yml`
- Modify: `TESTS.md`

- [ ] **Step 1: Add failing fixture checks**

Make every production-style MQTT collector return `del.Ack(ctx)`. Capture the
`Receiver.Run` result and fail on any error other than expected context
cancellation. In UC42, assert outbox completion independently:

```go
require.EventuallyWithT(t, func(collect *assert.CollectT) {
	pending, err := rt.OutboxPending(ctx, persistence.OutboxPartitionKey(sessionID, ""))
	require.NoError(collect, err)
	assert.Zero(collect, pending)
}, timeout, poll)
```

- [ ] **Step 2: Prove the existing fixture fails**

```bash
go test -race -count=1 -tags=longrunning ./tests/longrunning \
  -run TestUC42SharedOutboxBrokerRestart
```

Expected before the ACK fix: the collector stalls at the broker receive window.

- [ ] **Step 3: Stop masking test failures**

Remove `|| true` from `test` and `test-integration`, preserve pipeline exit
status, and add `-count=1` to unit, integration, and long-running commands.
Keep report generation, but let the Go command determine success.

- [ ] **Step 4: Add mandatory CI jobs**

Add Docker-backed integration, uncached module tests, and scheduled/manual
long-running jobs. Upload reports on success and failure.

- [ ] **Step 5: Run focused checks**

```bash
make test
make test-integration
```

- [ ] **Step 6: Commit**

```text
test: make production release gates authoritative
```

## Task 2: Preserve distinct MQTT ingress events

**Status:** Pending

**Agents/Skills:** thiink:messaging-expert, thiink:golang-pro,
thiink:test-reviewer

**Finding:** B1

**Files:**
- Modify: `adapters/mqtt/transport/paho/acl_headers.go`
- Modify: `adapters/mqtt/transport/paho/acl_router.go`
- Modify: `adapters/mqtt/transport/paho/headers_test.go`
- Modify: `adapters/mqtt/transport/paho/analysis_errors_headers_test.go`
- Modify: `adapters/mqtt/transport/paho/analysis_receiver_test.go`
- Modify: `adapters/mqtt/transport/paho/benchmark_hotpath_test.go`
- Create: `tests/integration/mqtt_equal_publish_identity_test.go`
- Modify: `UBIQUITOUS.md`
- Modify: `docs/transports/mqtt.md`

- [ ] **Step 1: Write failing identity tests**

Prove that two separate equal-valued publishes receive different envelope IDs,
while all handlers for one publish receive the same ID. Preserve explicit
`mqtt.message-id` and correlation identity.

- [ ] **Step 2: Run the failing tests**

```bash
(cd adapters/mqtt/transport/paho && \
  go test -race -count=1 -run 'Test.*(IDFallback|IngressIdentity|FanoutIdentity)' ./...)
```

Expected: equal topic/payload publishes currently receive the same ID.

- [ ] **Step 3: Replace content-derived identity**

Remove `deriveEnvelopeID`. Generate an RFC 4122 UUIDv4 only when no producer
identity exists. Stamp it once on the received publish before router fan-out:

```go
func ensurePublishIdentity(pub *Publish) error {
	if publishIdentity(pub) != "" {
		return nil
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate MQTT ingress identity: %w", err)
	}
	setPublishIdentity(pub, id.String())
	return nil
}
```

Reuse the repository's existing UUID package and publish-property helper rather
than adding a dependency or a second identity representation.

- [ ] **Step 4: Add real-broker proof**

Publish two byte-identical QoS 1 messages without identity properties through
`shared_outbox`; assert two outbox records, two destination deliveries, and two
source acknowledgements. Reconnect with an explicit producer ID and assert the
redelivery deduplicates.

- [ ] **Step 5: Document the exact guarantee**

State that no-ID redelivery may duplicate because MQTT cannot prove publish
identity across packet-ID reuse. At-least-once duplication is accepted; silent
collapse is not.

- [ ] **Step 6: Run focused checks**

```bash
(cd adapters/mqtt/transport/paho && go test -race -count=1 ./...)
go test -race -count=1 ./tests/integration -run TestMQTTEqualPublishIdentity
```

- [ ] **Step 7: Commit**

```text
fix(mqtt): preserve distinct equal-valued ingress events
```

## Task 3: Make Full readiness enforce the requested contract

**Status:** Pending

**Agents/Skills:** thiink:messaging-expert, thiink:runtime-expert,
thiink:test-reviewer

**Findings:** H3, H4

**Files:**
- Modify: `domain/connectivity/session.go`
- Modify: `bridge/specs.go`
- Modify: `bridge/session_plan_test.go`
- Modify: `adapters/mqtt/transport/paho/acl_router.go`
- Modify: `adapters/mqtt/transport/paho/session_health.go`
- Modify: `adapters/mqtt/transport/paho/session_health_test.go`
- Modify: `adapters/mqtt/transport/paho/session_reconcile.go`
- Modify: `adapters/mqtt/transport/paho/bug_c4_prodready_test.go`
- Modify: `ports/runtime.go`
- Modify: `UBIQUITOUS.md`
- Modify: `docs/deployment-guide.md`

- [ ] **Step 1: Write failing readiness tests**

Add two planned receiver IDs with only one registered handler and require
Degraded. Add a SUBACK downgrade and require reconcile failure plus non-Full
health.

- [ ] **Step 2: Run the failing tests**

```bash
go test -race -count=1 ./bridge -run TestSessionPlan
(cd adapters/mqtt/transport/paho && \
  go test -race -count=1 -run 'Test.*(Handler|QoSDowngrade|RequiredQoS)' ./...)
```

- [ ] **Step 3: Carry expected receiver identity**

Add `ExpectedReceiverIDs []string` to `connectivity.SessionPlan`.
`sessionPlanFor` must populate a sorted, deduplicated list of receiver IDs.
Expose a router handler-ID snapshot under the existing lock.

- [ ] **Step 4: Reject QoS downgrade**

Keep the downgrade warning and metric, but do not add downgraded filters to
`activeSubs`. Return `shared.ErrQoSNotSupported` containing topic, requested
QoS, and granted QoS. Health must compare each desired filter and required QoS,
not aggregate counts.

- [ ] **Step 5: Run focused checks**

```bash
go test -race -count=1 ./bridge
(cd adapters/mqtt/transport/paho && go test -race -count=1 ./...)
```

- [ ] **Step 6: Commit**

```text
fix(mqtt): enforce complete Full readiness
```

## Task 4: Reject unsafe MQTT and replica identity

**Status:** Pending

**Agents/Skills:** thiink:messaging-expert, thiink:clean-arch-reviewer,
thiink:test-reviewer

**Findings:** B3, H2

**Files:**
- Modify: `ports/plugin_config.go`
- Modify: `adapters/mqtt/transport/paho/config.go`
- Modify: `adapters/mqtt/transport/paho/config_plugin.go`
- Modify: `adapters/mqtt/transport/paho/factory.go`
- Modify: `bridge/supervisor.go`
- Create: `bridge/supervisor_mqtt_identity_test.go`
- Modify: `bridge/supervisor_durable_preflight_test.go`
- Modify: `validate/blueprint_graph.go`
- Modify: `validate/blueprint_graph_test.go`
- Modify: `config/validate_shared_sub_test.go`
- Modify: `docs/transports/mqtt.md`

- [ ] **Step 1: Write failing preflight tests**

Refuse live changes to broker set, effective client ID, session mode, clean
start, and session expiry while proving the old runtime remains active. Prove
credential, TLS, keepalive, and retry tuning do not change durable identity.
Reject clustered `$share` receivers without a per-replica suffix.

- [ ] **Step 2: Add generic optional capabilities**

```go
type DurableSessionIdentityConfig interface {
	DurableSessionIdentity(mode connectivity.SessionMode) (string, error)
}

type ReplicaIdentityConfig interface {
	ReplicaIdentityStrategy() string
}
```

Paho returns a secret-safe SHA-256 fingerprint over the canonical broker set,
effective client ID, mode, clean-start behavior, and expiry. Never include the
raw descriptor in logs or errors.

- [ ] **Step 3: Guard before replacement**

Compare durable identity before any old runtime is stopped or replacement is
built. The destructive-reload option must not bypass MQTT broker-state safety.
Reject `nonce` for Persistent/Exclusive and require `hostname` for durable
clustered `$share` consumers.

- [x] **Step 4: Run focused checks**

```bash
go test -race -count=1 ./bridge ./validate ./config \
  -run 'Test.*(SessionIdentity|Clustered.*Shared|ClientIDSuffix)'
(cd adapters/mqtt/transport/paho && \
  go test -race -count=1 -run 'Test.*(DurableSessionIdentity|ClientIDSuffix)' ./...)
```

- [x] **Step 5: Commit**

```text
fix(mqtt): reject unsafe durable identity changes
```

## Task 5: Persist managed MQTT subscription history

**Status:** Complete

**Agents/Skills:** thiink:messaging-expert, thiink:ddd-expert,
thiink:contract-reviewer, thiink:test-reviewer

**Finding:** B2

**Files:**
- Modify: `ports/stores.go`
- Modify: `ports/blueprint.go`
- Modify: `ports/factories.go`
- Modify: `config/parser/parse.go`
- Modify: `config/blueprint_marshal.go`
- Modify: `config/merge.go`
- Modify: `config/manager.go`
- Modify: `bridge/builder_prepare.go`
- Modify: `bridge/builder_complete.go`
- Modify: `bridge/specs.go`
- Modify: `bridge/supervisor.go`
- Modify: `adapters/mqtt/transport/paho/session.go`
- Modify: `adapters/mqtt/transport/paho/acl_session.go`
- Modify: `adapters/mqtt/transport/paho/acl_client.go`
- Modify: `adapters/mqtt/transport/paho/session_reconcile.go`
- Modify: `adapters/mqtt/transport/paho/session_lifecycle.go`
- Create: `adapters/native/store/sqlitesubscriptionstate/`
- Create: `adapters/aws/store/dynamodbsubscriptionstate/`
- Create: `ports/storetest/managed_subscriptions.go`
- Modify: `adapters/native/store/factory.go`
- Modify: `adapters/aws/store/acl_factory.go`
- Create: `tests/integration/mqtt_persistent_subscription_migration_test.go`
- Modify: `PLUGIN.md`
- Modify: `docs/transports/mqtt.md`

- [ ] **Step 1: Define and test the narrow store contract**

```go
type ManagedSubscriptionStore interface {
	List(ctx context.Context, storageIdentity string) ([]string, error)
	Remember(ctx context.Context, storageIdentity string, filters []string) error
	Forget(ctx context.Context, storageIdentity string, filters []string) error
}
```

Conformance tests must prove idempotency, independent identities, restart
persistence, and no forgotten filter after partial failure.

- [ ] **Step 2: Implement SQLite and DynamoDB adapters**

Use the existing store factory/config patterns. Remember desired filters before
SUBSCRIBE. Forget only filters confirmed by UNSUBACK. A crash may leave a safe
extra candidate but must never create an undiscoverable broker subscription.

- [ ] **Step 3: Reconcile before traffic**

Load history before persistent/exclusive connect activation, unsubscribe
`history - desired`, retain failed filters for retry, and recycle before normal
dispatch when stale filters may have buffered shared deliveries. If history is
unavailable, fail startup below Full readiness.

- [ ] **Step 4: Guard migration boundaries**

Require a managed-subscription store for non-Ephemeral sessions with
subscriptions. Refuse removal of an entire persistent session through ordinary
live reload; direct operators to the documented drain/unsubscribe/cutover
procedure.

- [ ] **Step 5: Add broker-backed release proof**

Cover removed `sensors/#`, removed `$share/group/sensors/#`, failed UNSUBACK
retry, runtime replacement, process restart with the same client ID, and a peer
that receives all shared-group messages without stale acknowledgements.

- [ ] **Step 6: Run focused checks**

```bash
go test -race -count=1 ./ports/storetest ./bridge
(cd adapters/native/store && go test -race -count=1 ./sqlitesubscriptionstate)
(cd adapters/aws/store && go test -race -count=1 ./dynamodbsubscriptionstate)
(cd adapters/mqtt/transport/paho && \
  go test -race -count=1 -run 'Test.*ManagedSubscription' ./...)
go test -race -count=1 ./tests/integration \
  -run TestMQTTPersistentSubscriptionMigration
```

- [ ] **Step 7: Commit**

```text
feat(mqtt): persist managed subscription history
```

## Task 6: Recover boundedly from unsettled MQTT delivery

**Status:** Complete

**Agents/Skills:** thiink:runtime-expert, thiink:messaging-expert,
thiink:resilience-auditor, thiink:test-reviewer

**Finding:** H5

**Files:**
- Modify: `adapters/mqtt/transport/paho/delivery.go`
- Modify: `adapters/mqtt/transport/paho/receiver.go`
- Modify: `adapters/mqtt/transport/paho/acl_router.go`
- Modify: `adapters/mqtt/transport/paho/acl_session.go`
- Modify: `adapters/mqtt/transport/paho/session_lifecycle.go`
- Modify: `adapters/mqtt/transport/paho/session_reconcile.go`
- Modify: `adapters/mqtt/transport/paho/session_health.go`
- Modify: `adapters/mqtt/transport/paho/config.go`
- Modify: `adapters/mqtt/transport/paho/metrics.go`
- Create: `adapters/mqtt/transport/paho/settlement_monitor.go`
- Create: `adapters/mqtt/transport/paho/session_recovery.go`
- Create: `adapters/mqtt/transport/paho/session_recovery_test.go`
- Create: `tests/integration/mqtt_settlement_recovery_test.go`
- Modify: `UBIQUITOUS.md`
- Modify: `docs/transports/mqtt.md`

- [ ] **Step 1: Write failing disposition tests**

Prove Ack and Retry are mutually exclusive and idempotent, unsupported Retry
leaves the delivery unsettled, concurrent recovery requests coalesce, health
degrades synchronously, drain is bounded, and minimum reconnect interval uses
the injected clock.

- [ ] **Step 2: Implement Paho Retry**

Keep the existing `ports.Delivery.Retry` boundary. Persistent/Exclusive Retry
requests a serialized reconnect that preserves client ID, expiry, and
`clean_start=false`. Ephemeral Retry remains `ErrNotSupported`.

- [ ] **Step 3: Track the protocol window**

Track QoS 1/2 packets per connection epoch from receipt until Ack or epoch
change. Expose unsettled count, oldest age, receive-window utilization, and
recovery reload count through existing health/metrics patterns.

- [ ] **Step 4: Bound recovery**

Drain other settlements for at most 5 seconds and limit recovery reloads to one
per 30 seconds. Missing Session Present during recovery is an error and keeps
readiness degraded. Credential and recovery reloads share one context-aware
serialization gate.

- [ ] **Step 5: Add real-broker outage proof**

With a persistent session, make processing fail and the DLQ unavailable. Assert
bounded disconnect, broker redelivery of the original message, later-packet
progress, exact producer-ID accounting, and non-Full readiness until recovery.

- [ ] **Step 6: Run focused checks**

```bash
(cd adapters/mqtt/transport/paho && \
  go test -race -count=1 -run 'Test.*(DeliveryDisposition|SessionRecovery|SettlementMonitor)' ./...)
go test -race -count=1 ./tests/integration -run TestMQTTSettlementRecovery
```

- [ ] **Step 7: Commit**

```text
fix(mqtt): recycle persistent sessions on unsettled delivery
```

## Task 7: Remove session-wide ingress failure coupling

**Status:** Complete

**Agents/Skills:** thiink:runtime-expert, thiink:clean-arch-reviewer,
thiink:test-reviewer

**Finding:** H6

**Decision:** At most one ingress receiver and one consuming route per MQTT session. Connection-wide MQTT ACK
ordering means per-route queues cannot provide the requested isolation without
durable staging or multiple broker connections.

**Files:**
- Modify: `ports/transport.go`
- Modify: `adapters/mqtt/transport/paho/factory.go`
- Modify: `adapters/mqtt/transport/paho/factory_test.go`
- Modify: `adapters/mqtt/transport/paho/session.go`
- Modify: `bridge/builder_prepare.go`
- Modify: `bridge/builder_prepare_test.go`
- Create: `tests/integration/mqtt_dedicated_session_isolation_test.go`
- Modify: `UBIQUITOUS.md`
- Modify: `PLUGIN.md`
- Modify: `docs/transports/mqtt.md`

- [ ] **Step 1: Write failing cardinality tests**

Prove Plan rejects two ingress receivers using one MQTT session before stores
or runtime resources are created. Prove registry aliases cannot bypass the
adapter-local reservation.

- [ ] **Step 2: Add a generic capability**

Add `CapDedicatedIngressSession`. Paho advertises it; the bridge validates
receiver/session cardinality by capability, not by transport name. Keep a
factory reservation as defensive enforcement for programmatic callers.

- [ ] **Step 3: Add isolation proof**

Use two MQTT sessions. Block one route and prove the other continues receiving
within its declared latency and readiness bounds.

- [x] **Step 4: Run focused checks**

```bash
go test -race -count=1 ./bridge -run TestDedicatedIngressSession
(cd adapters/mqtt/transport/paho && \
  go test -race -count=1 -run TestFactoryDedicatedReceiver ./...)
go test -race -count=1 ./tests/integration \
  -run TestMQTTDedicatedSessionIsolation
```

- [x] **Step 5: Commit**

```text
fix(mqtt): require isolated ingress sessions
```

## Task 8: Bound MQTT ingress memory by bytes

**Status:** Complete

**Agents/Skills:** thiink:runtime-expert, thiink:golang-pro,
thiink:cost-auditor, thiink:test-reviewer

**Finding:** H7

**Files:**
- Modify: `adapters/mqtt/transport/paho/config.go`
- Modify: `adapters/mqtt/transport/paho/session.go`
- Modify: `adapters/mqtt/transport/paho/acl_router.go`
- Modify: `adapters/mqtt/transport/paho/factory.go`
- Modify: `ports/factories.go`
- Modify: `bridge/builder_prepare.go`
- Modify: `deployment/aws-filebased-config/lib/model/bootstrap.go`
- Modify: `deployment/aws-filebased-config/infra/bootstrap.go`
- Modify: `deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase/base.go`
- Create: `deployment/aws-filebased-config/lib/bootstrap/mqtt_memory_profile.go`
- Create: `tests/longrunning/mqtt_ingress_memory_test.go`
- Modify: `docs/transports/mqtt.md`

- [x] **Step 1: Write overflow and boundary tests**

Cover zero/default normalization, exact budget boundary, one-byte excess,
integer overflow, route concurrency, multiple sessions, and a budget too small
for one packet.

- [x] **Step 2: Implement the byte model**

Use:

```text
packet = ceil(maxPacketSize(maxPayloadBytes) * 1.25)
window = receiveMaximum + dispatchCapacity + routeMaxInFlight + 1
bound  = packet * window
```

Perform division checks before multiplication. Set conservative defaults:
256 KiB payload, 192 receive maximum, 256 MiB ingress budget. Size dispatch
capacity from the effective receive maximum instead of fixed 1024.

- [x] **Step 3: Derive the AWS profile**

Reserve one quarter of task memory for MQTT ingress, divide it across ingress
sessions, derive the largest safe receive maximum, and reject configurations
without 20 percent container headroom.

- [x] **Step 4: Add measured memory proof**

Fill receive and dispatch windows with maximum payload while downstream is
blocked. Assert peak RSS remains below 80 percent of the configured container
limit.

- [x] **Step 5: Run focused checks**

```bash
(cd adapters/mqtt/transport/paho && go test -race -count=1 -run Test.*IngressMemory ./...)
go test -race -count=1 ./bridge -run TestIngressMemory
(cd deployment/aws-filebased-config/lib && \
  go test -race -count=1 ./... -run TestMQTTMemoryProfile)
go test -race -count=1 -tags=longrunning ./tests/longrunning \
  -run TestMQTTIngressMemory
```

- [x] **Step 6: Commit**

```text
fix(mqtt): enforce byte-based ingress budgets
```

## Task 9: Make lease takeover inherit verified observation

**Status:** Completed

**Agents/Skills:** thiink:architect-aws-serverless, thiink:golang-pro,
thiink:resilience-auditor, thiink:test-reviewer

**Finding:** H1

**Files:**
- Modify: `UBIQUITOUS.md`
- Modify: `adapters/aws/store/dynamodblease/acl_store.go`
- Create: `adapters/aws/store/dynamodblease/acl_store_observation_test.go`
- Modify: `runtime/session/config.go`
- Modify: `runtime/session/config_test.go`
- Modify: `ports/blueprint.go`
- Modify: `ports/plugin_config.go`
- Modify: `adapters/mqtt/transport/paho/config_plugin.go`
- Modify: `bridge/builder_complete.go`
- Modify: `bridge/builder_complete_test.go`
- Modify: `tests/longrunning/uc3_cluster_failover_test.go`
- Modify: `docs/scenarios/08-clustered-exclusive-sessions.md`
- Modify: `docs/runbooks/node-down-failover.md`

- [x] **Step 1: Write persisted-observation tests**

Use two stores and fake clocks. Prove replacement inherits elapsed confirmation,
competing observers cannot double-count, renewal/release/takeover clear evidence,
CAS loss restarts local timing, legacy expiry changes reset evidence, and clock
skew cannot cause early takeover.

- [x] **Step 2: Persist observation on the lease row**

Condition every update on the exact owner/version/renewed/expires tuple and
observation value. Add only locally measured monotonic elapsed time. Never
enable DynamoDB TTL on the lease table.

- [x] **Step 3: Add failover-budget validation**

For declared SLOs, reject:

```text
lease TTL
+ 2 * max(1ms, ceil(1.25 * acquire poll))
+ (1 + ceil(lease TTL / min jittered poll)) * renew call timeout
+ complete post-takeover transport activation
+ startup allowance
> failover SLO
```

Use an optional aggregate transport timing capability; nested connect and
reconcile phases are counted exactly once. Do not advertise a 60-second preset until measurements prove it.

- [x] **Step 4: Measure the actual holder**

Update UC3 to query the lease, stop the verified owner, require owner/version
change and successor `ServiceLevelFull`, and report warm/cold p50, p95, p99,
and maximum separately.

- [x] **Step 5: Run focused checks**

```bash
(cd adapters/aws/store/dynamodblease && go test -race -count=1 ./...)
go test -race -count=1 ./runtime/session ./bridge
go test -race -count=1 -tags=longrunning ./tests/longrunning \
  -run TestUC3ClusterFailover
```

- [x] **Step 6: Commit**

```text
fix(ha): persist takeover observation evidence
```

## Task 10: Fail closed on clustered live reload

**Status:** Completed

**Agents/Skills:** thiink:runtime-expert, thiink:clean-arch-reviewer,
thiink:test-reviewer

**Finding:** H8

**Files:**
- Modify: `bridge/convert.go`
- Modify: `bridge/supervisor.go`
- Modify: `bridge/supervisor_test.go`
- Modify: `deployment/aws-filebased-config/lib/bootstrap/app.go`
- Modify: `deployment/aws-filebased-config/lib/bootstrap/app_test.go`
- Modify: `docs/scenarios/10-dynamic-reconfiguration.md`
- Create: `docs/runbooks/cluster-config-rollout.md`

- [x] **Step 1: Write failing no-swap tests**

For current or proposed clustered deployment, attempt a non-no-op live reload
and prove the runtime, config, running version, and applied reference remain
unchanged. Require failed swap event/metric. The destructive-reload option must
not bypass the guard. A no-op re-emit is acknowledged WITHOUT a swap (runtime /
config / version / reference preserved) and is detected before the guard; the
shared predicate and the canonical (fingerprint-equivalent) no-op comparison are
unit-tested at their boundaries (nil / static-endpoint / version-only).

- [x] **Step 2: Add the shared guard**

Expose `IsClusteredDeployment` from bridge conversion logic (reused by the
builder store-distribution guard) and apply it in both Supervisor and AWS
composition-root reload paths after no-op detection but before Plan/build/stop —
in the Supervisor, ahead of the durable-identity preflight and the paused
handling so a paused or destructive-mode clustered reload cannot slip through.

- [x] **Step 3: Document external cohort rollout**

Specify stage, validate, quiesce ingress, drain/stop all members, commit, start
all members, verify Full/version barrier, re-enable ingress, and whole-cohort
rollback. Do not describe local config CAS as cluster consensus.

- [x] **Step 4: Run focused checks**

```bash
go test -race -count=1 ./bridge -run TestSupervisorClusteredReload
(cd deployment/aws-filebased-config/lib && \
  go test -race -count=1 ./... -run TestClusteredReload)
```

- [x] **Step 5: Commit**

```text
fix(config): reject uncoordinated cluster reloads
```

## Task 11: Ship a DynamoDB-coordinated ECS HA profile

**Status:** Complete

**Agents/Skills:** thiink:architect-aws-serverless, thiink:cost-auditor,
thiink:security-auditor, thiink:test-reviewer

**Finding:** H10

**Files:**
- Modify: `deployment/aws-filebased-config/UBIQUITOUS.md`
- Modify: `deployment/aws-filebased-config/lib/bootstrap/registry.go`
- Modify: deployment module manifests for the ECS endpoint resolver
- Create: `deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha/data.go`
- Create: `deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha/ha.go`
- Create: `deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha/data_test.go`
- Create: `deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha/ha_test.go`
- Modify: `deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment/attachment.go`
- Modify: `deployment/aws-filebased-config/cdk/constructs/gobridgealarms/alarms.go`
- Modify: `deployment/aws-filebased-config/cdk/constructs/internal/singleton/singleton.go`
- Modify: `deployment/aws-filebased-config/cdk/constructs/internal/grants/dynamodb.go`
- Create: `deployment/aws-filebased-config/cdk/integration/ha_fixture.go`
- Create: `deployment/aws-filebased-config/cdk/integration/ha_failover_test.go`
- Modify: `deployment/aws-filebased-config/README.md`
- Modify: `docs/aws-deployment/overview.md`

- [x] **Step 1: Establish glossary names**

Define `dynamodb_coordinated_ha`, `TopologyDynamoDBCoordinatedHA`,
`GoBridgeDynamoDBHA`, `DynamoDBHAProps`, and `DynamoDBHAData` before code uses
them.

- [x] **Step 2: Add CDK assertion tests**

Require three DynamoDB tables with exact key/index schemas, lease TTL disabled,
on-demand billing, retained production data, two-AZ placement, explicit
failover budget, stable exclusive MQTT identity, shared outbox, CloudWatch
metrics, and at least one warm standby.

- [x] **Step 3: Implement the separate facade**

Reuse `gobridgebase.New` for one control task and at least two workers. Keep
`GoBridgeCluster` unchanged and explicitly documented as independent
filesystem scale-out.

- [x] **Step 4: Restrict IAM**

Grant task roles only required data-plane actions plus `DescribeTable` and
`DescribeTimeToLive`; exclude table creation, mutation, TTL mutation, and
deletion. Scope resources to table and index ARNs.

- [x] **Step 5: Add alarms and endpoint identity**

Register the ECS endpoint resolver for clustered profiles. Add task-count,
DynamoDB throttle/system-error, lease, outbox, DLQ, and measured
failure-to-Full alarms. Missing duration samples are non-breaching; the release
gate must prove samples exist.

- [x] **Step 6: Add credentialed integration harness**

Stop the exact ECS task holding the lease, wait for STOPPED, require lease
owner/version change and successor Full readiness, and emit warm/cold
percentiles. Fail clearly when required sandbox variables are absent.

- [x] **Step 7: Run source-safe checks**

```bash
(cd deployment/aws-filebased-config/lib && go test -race -count=1 ./...)
(cd deployment/aws-filebased-config/cdk && go test -race -count=1 ./...)
```

- [x] **Step 8: Commit**

```text
feat(aws): add DynamoDB-coordinated bridge HA
```

## Task 12: Correct production claims and pin container inputs

**Status:** Complete (spec-review round 6 resolved; make lint + make test green)

**Agents/Skills:** thiink:doc-markdown-writer, thiink:doc-markdown-reviewer,
thiink:security-auditor

**Findings:** D1, D2, D3, D4, D5, D6

**Files:**
- Modify: `docs/transports/mqtt.md`
- Modify: `docs/adr/0003-mqtt-persistent-session-hygiene.md`
- Modify: `docs/adr/0009-durable-outbound-mqtt-session-state.md`
- Modify: `docs/adr/0010-mqtt-loop-prevention-contract.md`
- Modify: `docs/adr/0011-cluster-client-id-uniqueness.md`
- Modify: `docs/runbooks/node-down-failover.md`
- Modify: `docs/timing-audit.md`
- Modify: `adapters/mqtt/transport/paho/analysis_receiver_test.go`
- Modify: `Dockerfile`
- Modify: `docs/aws-deployment/overview.md`
- Modify: `deployment/aws-filebased-config/README.md`
- Modify: `deployment/aws-filebased-config/cdk/constructs/internal/seeder/scripts/update-image.sh`
- Modify: `deployment/aws-filebased-config/cdk/constructs/internal/seeder/MANIFEST.md`
- Modify: `deployment/aws-filebased-config/cdk/constructs/internal/seeder/tests/run.sh`
- Create: `deployment/aws-filebased-config/cdk/constructs/internal/seeder/tests/fixtures/bin/crane`
- Create: `deployment/aws-filebased-config/cdk/constructs/internal/seeder/tests/fixtures/bin/docker`
- Create: `deployment/aws-filebased-config/cdk/constructs/internal/seeder/tests/fixtures/bin/curl`
- Modify: `DEVELOPMENT.md`
- Modify: `TESTS.md`

- [x] **Step 1: Replace absolute delivery claims**

Add a source QoS/session × delivery mode × producer identity × store durability
× outage-duration matrix. State every configured loss boundary and ambiguous
send duplicate boundary.

Done: `docs/transports/mqtt.md` now carries the single canonical
"Source-to-destination guarantee matrix"; the "neither wired delivery mode
loses a message" claim is removed there and in
`docs/adr/0009-durable-outbound-mqtt-session-state.md`, which links the matrix
instead of duplicating it. The matrix was expanded into distinct rows (verified
against code): durable vs **volatile** (in-memory, test-only) outbox store; the
`shared_outbox` Persist boundary (pre-Persist has no durable record, so QoS0/
Ephemeral loses while a QoS1/2 source redelivers; post-Persist the drainer
replays independently of the source session); **missing** vs **reused** producer
ID; and exact policy boundaries. Round-3 review corrected the loss boundaries to
the code: pending records have no store-retention TTL (retention compacts only
terminal records), capacity is a pre-Persist admission gate, and `shared_outbox`
poison requires `ReplayCount > MaxReplayAttempts` AND `ReplayBudget` elapsed
(15m default) while `direct_hold` poisons on the count cap alone. The prohibited
vague qualifier was removed from mqtt.md (spec-review finding).
`docs/aws-deployment/overview.md` EFS-reload "without dropping in-flight messages"
qualified by source redelivery and bounded/aborted drain. The terminal-failure
rows were corrected: `configured drop policy` was not a delivery mode, and the two
wired modes settle differently. `direct_hold`: on a successful DLQ write (or a
recorded `drop`) the **source delivery** is settled/ACKed; a DLQ **write failure**
leaves the source unsettled so it redelivers. `shared_outbox`: the source was
already ACKed right after Persist, so terminal handling **completes the outbox
record** (`OutboxStore.Complete`) only after a successful DLQ write or `drop`; a
DLQ write failure leaves the record pending/claimed so the drainer retries.
Verified in `runtime/route/dispatch.go` (`settleTerminal`→`ackDelivery`) and
`runtime/outbox/retry.go` (`completeTerminal`→`OutboxStore.Complete`).

- [x] **Step 2: Correct subscription, failover, and ACK text**

State that persistent restart does not remove unknown wildcard/shared filters;
warm and cold takeover have distinct bounds; fencing cannot undo a completed
destination send; production Paho uses manual acknowledgement.

Done: ADR 0003 corrected (empty-plan reconcile unsubscribes known managed subs;
a same-`client_id` restart resumes the broker session so unknown filters
survive). `docs/runbooks/node-down-failover.md` fencing wording fixed
(prevents stale-owner outbox mutation, cannot undo an accepted destination
send). `docs/timing-audit.md` identity precedence corrected to the per-publish
UUID fallback. `analysis_receiver_test.go` reworded/renamed as a legacy router
seam, not a production settlement-conformance statement.

- [x] **Step 3: Correct ADR provenance**

Use `git log --follow` and implementing commits to set decision/effective dates.
Future decisions remain Proposed, never future-dated Accepted.

Done: future date 2026-08-14 replaced with the real commit dates and roles.
ADR 0009 → decision recorded 4d8d76d (2026-07-13); implementation
(`NonDurableEgressReporter`/`egressDurabilityAdvisory`) in 9d8effb (2026-07-10).
ADR 0010 → 4d8d76d (2026-07-13). ADR 0011 → decision recorded 4d8d76d
(2026-07-13); original takeover (`noteSessionTakeover`) 761a048 (2026-07-03),
suffix uniquifier `e48e879` (2026-07-10), penalty/nonce hardening 4d8d76d.
ADR 0003 covered-retention addendum → implementation 438139a (2026-07-10),
refactor 4d8d76d (2026-07-13), addendum text 9d8effb (2026-07-10). All remain
Accepted because the implementation is present at or before the audited revision
(2026-07-13). Round-2 review corrected the coarse "landed together"/"Implemented"
lines to distinct commit roles verified with `git log -S`/`git show`.

- [x] **Step 4: Pin image indexes**

Resolve and commit top-level multi-platform index digests for Go 1.25 bookworm
and distroless static Debian 12 nonroot. Repair the existing digest script so
JSON parsing receives stdin, verifies amd64/arm64, and never recommends an
unpinned tool install.

Done via `docker buildx imagetools inspect` (no tools installed):
`golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58`
and `gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b`,
both verified as OCI indexes including linux/amd64 + linux/arm64. `Dockerfile`
pinned (GO_VERSION ARG removed; stale "until then mutable" comments removed).
`update-image.sh` rewritten: the upstream aws-cli image has no floating
`2` tag (registry-verified), so the script discovers the highest concrete `2.x.y`
tag (`crane ls` or ECR Public v2 API via curl), resolves that tag's top-level
index via crane or docker buildx, verifies amd64+arm64, computes the digest from
verified bytes, and fails closed (never pins the mutable `2`). The two-file update
uses **staged fail-closed validation/replacement**: it validates exactly one
`FROM <repo>` line and both destinations' writability, stages both outputs in
same-directory temp files, and only then renames both — a zero-FROM,
multiple-FROM, missing target directory, or read-only Dockerfile aborts (exit 4)
with both `image.txt` and the Dockerfile checksums unchanged. (The two renames are
sequential, not multi-file atomic.) The script does not check tool versions — it
validates resolver output — and rejects a forced `UPDATE_IMAGE_TOOL` other than
`crane`/`docker` (exit 2); versions tested for this workflow are crane v0.21.7 /
docker buildx v0.34.1, and no unpinned/`latest` install is suggested. Seeder tests
(24) cover BOTH resolver paths, the forced-tool rejection, and the staged
fail-closed cases with fake crane/docker/curl. Real docker path validated live:
resolved `aws-cli:2.35.24@sha256:f7e6c7fb…` and rewrote both files. Workflow
recorded in DEVELOPMENT.md, TESTS.md, MANIFEST.md, overview.md, deployment README.

- [x] **Step 5: Review docs**

```bash
git diff --check
```

- [x] **Step 6: Commit**

```text
docs: state MQTT production boundaries exactly
```

## Task 13: Prepare reproducible module and image releases

**Status:** Pending (quality-review remediation implemented; awaiting review
acceptance, and public H9 closure still requires staged tags and proxy smoke)

**Agents/Skills:** thiink:golang-pro, git-workflow-manager,
thiink:contract-reviewer, thiink:test-reviewer

**Finding:** H9

**Files:**
- Modify: `RELEASE.md`
- Modify: `.github/workflows/release.yml`
- Modify: `.github/workflows/ci.yml`
- Modify: `Makefile`
- Modify: `README.md`
- Modify: all published module `go.mod`/`go.sum` files only where versions are
  resolvable at source-review time

- [x] **Step 1: Encode the module DAG**

Release root first; then 26 direct-root modules; then `adapters/aws/store`,
`adapters/native/store`, and `httpapi`; finally `cmd/gobridge`. Include internal
test-helper pseudo-version bootstrap and the rule never to invent pseudo-version
timestamps or hashes.

The planned count of 24 was stale: the DynamoDB and SQLite managed-subscription
implementation modules make the repository-derived layer-1 count 26 (31
published modules total).

- [x] **Step 2: Add manifest verification**

For every published module, reject local `replace`, exact `v0.0.0`, and
all-zero pseudo-versions. Run `GOWORK=off go mod download`, `go mod verify`,
build, and uncached tests.

- [x] **Step 3: Add external-consumer smoke gates**

After tags exist, create an empty temporary module, fetch the Paho adapter
through `proxy.golang.org,direct`, list it, and install `cmd/gobridge`.

- [x] **Step 4: Make image publication depend on the final module**

Publish only from a successful stable `cmd/gobridge/v*` release. Enable BuildKit
SBOM and maximum provenance on digest-only output, scan both immutable
platform-child digests, attach the exact digest to the command GitHub Release,
and move only mutable `latest` when the release is still the highest stable
protected command tag. No semver container tag is created.

- [x] **Step 5: Keep pre-release docs truthful**

Do not replace the README external-consumption warning until the public proxy
smoke test succeeds.

- [x] **Step 6: Run source-safe checks**

```bash
make verify-release-preparation
# Expected rejection until migration/tags exist:
make verify-published-modules RELEASE_VERSION=v0.3.0
git diff --check
make lint
make test
```

- [x] **Step 7: Commit**

```text
build: enforce staged multi-module releases
```

**Quality-review remediation (pending acceptance):**

- [x] Remove Make/shell tag injection and reject incompatible path majors.
- [x] Bind public module origins to exact tag commits and contain manifest paths.
- [x] Isolate/retry proxy-only smoke, then prove direct-only resolution.
- [x] Add signal, command, smoke, and workflow deadlines.
- [x] Delay GitHub Release creation until applicable smoke succeeds.
- [x] Pin all workflow actions plus Buildx, BuildKit, and QEMU inputs.
- [x] Scan both digest-only platform children and record the index digest.
- [x] Re-check highest stable command tag immediately before moving `latest`.
- [x] Resume reruns from the exact recorded release digest without rebuilding.

## Task 14: Add exact-accounting chaos and release proofs

**Status:** Pending

**Agents/Skills:** thiink:test-designer, test-automator,
thiink:resilience-auditor, thiink:test-reviewer

**Files:**
- Create: shared producer-ID accounting helper under `tests/testutil/`
- Modify: `adapters/mqtt/transport/paho/integration_bugfix_test.go`
- Modify: `adapters/mqtt/transport/paho/integration_high_fixes_test.go`
- Create: `adapters/mqtt/transport/paho/integration_connection_failures_test.go`
- Create: `tests/longrunning/chaos_store_outages_test.go`
- Create: `tests/longrunning/chaos_client_takeover_test.go`
- Create: `tests/longrunning/chaos_process_kill_test.go`
- Create: `tests/longrunning/mqtt_settlement_chaos_test.go`
- Modify: `tests/longrunning/uc46_broker_edge_test.go`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Add one accounting helper**

Compare producer IDs end to end and report separate missing, duplicate,
reordered, DLQ, and intentionally dropped sets. Reuse it in every loss test.

- [ ] **Step 2: Replace weak reconnect tests**

Use the same client ID, `clean_start=false`, persisted broker state, queued QoS
1/2 messages, real disconnect/reconnect, and Session Present. Force reconcile
timeout and assert bounded degradation/retry.

- [ ] **Step 3: Add fault matrices**

Cover broker failure before/after CONNACK, DNS/TLS/credential failure, DLQ,
outbox, and lease outages independently and together, duplicate client-ID
storms, SIGKILL at source-ACK/outbox/send boundaries, and maximum payload at
full windows.

- [ ] **Step 4: Add the 100,000-message identity gate**

Publish 100,000 distinct equal-valued messages without producer IDs and require
100,000 outputs with no missing events. Separately prove producer-ID
redelivery deduplication.

- [ ] **Step 5: Run release suites**

```bash
(cd adapters/mqtt/transport/paho && go test -race -count=1 ./...)
make test-integration
make test-long-running
```

- [ ] **Step 6: Commit**

```text
test: add MQTT production chaos release gates
```

## Task 15: Final repository and adversarial gates

**Status:** Pending

**Agents/Skills:** thiink:adversarial-reviewer, thiink:code-reviewer,
thiink:security-auditor, thiink:resilience-auditor

**Files:**
- Modify only files required to fix confirmed review findings
- Modify: `PROD_READY_ISSUES.md`

- [ ] **Step 1: Run all local gates**

```bash
make lint
make test
make test-integration
make test-long-running
make check-all
```

- [ ] **Step 2: Run adversarial review**

Review every finding, release gate, changed public contract, crash boundary,
configuration migration, IAM grant, and documentation claim. Report only
reproducible defects with exact file/symbol references.

- [ ] **Step 3: Fix every confirmed issue test-first**

Add the smallest regression test for each code defect, make the root-cause fix,
and rerun the focused and full gates. Documentation-only findings require
`git diff --check`.

- [ ] **Step 4: Update the audit**

For each finding, record Resolved, Narrowed by enforced contract, or External
release evidence required. Do not claim zero bugs or production approval
without the credentialed AWS, registry-scan, public-module, and measured SLO
evidence.

- [x] **Step 5: Commit**

```text
fix: close adversarial production-readiness findings
```

## External release sequence — SKIPPED in the source branch

These actions are intentionally outside automated branch execution because
they are externally visible, effectively irreversible, credentialed, or depend
on the merged commit:

1. Push the root and path-prefixed module tags in `RELEASE.md` dependency order.
2. Wait for Go proxy/checksum ingestion and run the empty-module smoke tests.
3. Create GitHub Releases without reusing a failed tag.
4. Build, scan, attest, and publish the GHCR image by immutable digest.
5. Move `latest` only for a stable release after all module and scan gates pass.
6. Deploy the credentialed AWS HA fixture, kill the verified leaseholder, and
   collect enough warm/cold samples for the declared percentile.
7. Configure required checks, protected environments, branch protection,
   registry scanning, OIDC release roles, VPC/subnets, and broker secrets.
8. Promote the image only after the audit records the external evidence.

The pull request may merge with these actions pending only if it says
**release candidate, not production-approved**. H9 and the measured H1/H10
release gates close only after this sequence succeeds.
