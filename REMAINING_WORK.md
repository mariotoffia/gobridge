# GoBridge Remaining Work

This file consolidates the actionable remainder of the former
`PORTS_DOMAIN_REVIEW.md` and `ENVELOPE_CREDENTIALSET_ENCAPSULATION_PLAN.md`,
which were removed after their content was either executed or preserved here.
It is the single backlog for a future session. Every finding is extracted
faithfully from the original review; nothing has been reworded beyond minor
condensation. The recommended implementation order appears at the bottom,
minus completed items.

---

## Completed

- **Finding 20** — Aggregate lint misses the actual aggregate model: `scripts/aggcheck` now enforces `// aggregate-root` encapsulation (no exported mutable fields; no unguarded pointer transitions); hardened with `//aggcheck:allow-unguarded` per-method allowance and detection of index/element/pointer-deref mutation.
- **Finding 6** — Domain credentials are mutable secret DTOs: `CredentialSet` fields privatised, `NewCredentialSet` constructor, nil-safe `Password()`/`TLS()` accessors, `String()`/`GoString()` redaction, `// aggregate-root` marker.
- **Finding 9** — `DLQEntry` exposes a mutable failure snapshot: all 13 exported scalar fields privatised behind value-receiver accessors, `// aggregate-root` marker, ripple complete.
- **Finding 8** — `OutboxRecord` exposes mutable aggregate state: was already done before this session (it is the reference pattern the plan mirrored).
- **`messaging.Envelope`** fully encapsulated this session (id/payload/createdAt/expiresAt private, defensive-copy `Payload()`, guarded `SetExpiry`/`AssignID`, extension `SetPayload`, JSON wire-format byte-identical, `// aggregate-root` marker). This is the "actual aggregate model" part of finding 20.
- **Behavioral decision** — AMQP 1.0 / Azure SB ingress ACCEPTs received messages whose broker absolute-expiry is already past (permissive, original behavior; dropped downstream by TTL) rather than rejecting at ingress.

---

## Remaining findings

### Critical

#### Finding 1 — Outbox claim authority is ambiguous

`ports/stores.go:31-35`: `OutboxStore.Claim` accepts both `ownerID` and
`token.Owner`, creating two sources of authority. Memory and SQLite write
`claimed_by = ownerID`; DynamoDB writes `claimed_by = ownerID` but completes
against `token.Owner`.

**Fix:** Change `Claim(ctx, partitionKey, ownerID, token, limit)` to
`Claim(ctx, partitionKey, token, limit)`. Feed `token.Owner` into
`OutboxRecord.Claim`. Remove `runtime/outbox.Config.OwnerID` and
`Drainer.ownerID`. Update all store adapters, fakes, integration tests, and
store conformance tests.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 2 — Outbox completion fencing is inconsistent across stores

SQLite (`adapters/native/store/sqliteoutbox/acl_query.go:84-90`) checks only
`claim_version`. Memory (`adapters/native/store/memoryoutbox/store.go:155-175`)
checks only `ClaimVersion()`. DynamoDB
(`adapters/aws/store/dynamodboutbox/acl_store.go:400-417`) checks
`claimed_by = token.Owner AND claim_version = token.Version`. A wrong owner
with the same version succeeds in memory/SQLite and fails in DynamoDB.

**Fix:** Pick one canonical fence and enforce it in every adapter. Recommended
near-term: `status == claimed`, `claimed_by == token.Owner`,
`claim_version == token.Version`. Longer-term: add `LeaseID` to `LeaseToken`
or make it opaque. Add storetest coverage for same-version/different-owner
`Complete`.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 3 — Outbox expiry is unfenced

`ports/stores.go:35`: `OutboxStore.Expire(ctx, before)` accepts no
`LeaseToken`. SQLite, memory, and DynamoDB expiry all flip both pending and
claimed records to expired without owner/token validation. No production caller
today beyond wrappers/tests, but the port permits unfenced mutation of records
currently owned by a valid drainer.

**Fix:** Split expiry into explicit operations: pending-only expiry (no owner
needed) and claimed-record expiry (requires token + ownership validation). Add
storetest coverage for expiry against a claimed record.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 4 — Outbox stores do not validate tokens against the current lease

`ports/stores.go:28-35`: the port says token-accepting mutations must validate
fencing atomically, but `OutboxStore` has no `LeaseID`, lease snapshot, or
opaque current-lease validation contract. Store adapters compare against record
claim metadata, not necessarily the current `LeaseStore` state. This allows a
stale token to claim never-claimed pending records when the store has not yet
observed a newer claim for that partition.

**Fix:** Bind outbox partitions to lease IDs in the port contract. Either pass
`LeaseID` with the token or make `LeaseToken` opaque and store-verifiable.
Define whether the outbox store must consult the current lease in the same
consistency boundary, or document that only runtime `TokenFn` gates access and
downgrade the store contract accordingly.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 5 — Plugin configs can leak secrets by default

`ports/plugin_config.go:43-58`: `PluginConfig` requires `Kind` and `Validate`,
but no redaction or secret-handling contract. MQTT config
(`adapters/mqtt/transport/paho/config.go:24-25`) contains raw `Username` and
`Password`. Azure Service Bus config
(`adapters/azure/transport/servicebus/acl_client.go:94-99`) carries
`ConnectionString` and `ClientSecret`.

**Fix:** Introduce a secret-safe value object for plugin config secrets.
Enforce redaction via `String`, `GoString`, `MarshalJSON`, and
`slog.LogValuer` where applicable. Extend `cfgshape` or a new analyzer to
reject secret-looking exported string fields in `PluginConfig` types unless
they use approved secret wrappers.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

### High

#### Finding 7 — HTTP API keys are raw strings on a broadly passed blueprint

`ports/blueprint.go:356-365`: `HTTPConfig.AdminAPIKey` and `MonitorAPIKey` are
plain exported strings on `BridgeConfig`, which flows through config, runtime,
and HTTP surfaces.

**Fix:** Replace API key fields with secret-safe value objects. Ensure config
read/admin endpoints never return secret values unless explicitly designed for
write-only semantics.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 10 — Domain imports JSON and owns storage/wire schema

`domain/messaging/envelope.go:3-4` imports `encoding/json`.
`domain/messaging/envelope.go:255-273` owns the durable JSON schema used by
storage adapters. `.go-arch-lint.yml:258-266` says the domain/ports JSON ban
is only enforced by code review for broader `encoding/json` usage.

**Fix:** Move envelope JSON DTO/marshalling to adapter or
persistence-mapping packages. Or explicitly document and lint a narrow
exception if stable domain-owned JSON is an accepted architectural choice.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 11 — Blueprint types are not actually format-neutral

`DDD.md:438-441` documents blueprint as format-neutral.
`ports/blueprint.go:342-347` and related fields carry YAML/JSON struct tags,
which is inconsistent with that claim.

**Fix:** Move wire tags to parser DTOs and map into tag-free port/application
types. Or update `DDD.md`, `ARCHITECTURE.md`, and lint comments to admit that
ports are schema-tagged DTOs.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 12 — Shared kernel contains adapter-specific metric vocabulary

`domain/shared/metrics.go:34-44` and `:75-123` contain SQS, MQTT, Azure
Service Bus, HTTP, and AMQP metric names in `domain/shared`, causing
unnecessary coupling and blast-radius.

**Fix:** Keep only generic metric taxonomy in `domain/shared`. Move
adapter-specific metric constants to adapter/observability packages. Preserve
shared tag keys only when they are truly cross-cutting.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 13 — Processor cancellation can hang shutdown

`runtime/route/chain.go:137-140`: on parent cancellation, the route chain
waits on `<-done` without a bound. A processor that ignores `ctx.Done()` can
hang shutdown and hold in-flight capacity indefinitely.

**Fix:** Wait for a bounded grace period. Return cancellation/timeout after
the grace period and release route capacity. Keep timeout handling distinct
from processor failure classification.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 14 — `BatchSender` cannot report per-message partial failure

`ports/transport.go:64-71`: `SendBatch` returns only `(int, error)`. SQS
(`adapters/aws/transport/sqs/sender.go:104-115`) supports partial success and
continues after failures, returning only an aggregate successful count.

**Fix:** Replace `(int, error)` with per-message results keyed by input index
or envelope ID. Or document and enforce strict prefix-success semantics in
every adapter. Do this before runtime/shared-outbox batching uses
`BatchSender`.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 15 — `ConfigStore` lacks context despite allowing remote stores

`ports/blueprint_validation.go:49-70`: `ConfigStore` methods have no
`context.Context`, while the docs explicitly allow DynamoDB/Vault-style
implementations that require context for network operations.

**Fix:** Add context to `Load`, `Save`, `Validate`, and `Merge`. Thread
request/shutdown contexts through `httpapi/config_txn.go` and config parser
implementations.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 16 — Read-side runtime port exposes mutable DLQ store

`ports/runtime.go:169-171`: `RuntimeQuery` returns the full `DLQStore`,
including `Delete`, `DeleteByFilter`, and `Purge`, violating command/query
separation on a read-side port.

**Fix:** Split `DLQReader` from `DLQAdmin`. Or expose explicit runtime
query/command methods instead of returning the driven persistence port.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 17 — Domain events are not owned by aggregate transitions

`domain/events/doc.go:1-29` is a standalone domain component absent from the
DDD bounded-context table in `DDD.md:9-18`.
`domain/events/outbox.go:36-56`: outbox events are fabricated by public
constructors; `OutboxRecord.Claim`, `Complete`, and `Expire` do not raise or
return events. `ports/event_publisher.go:9-13` says aggregates raise events.

**Fix:** Either colocate events with owning bounded contexts and have
aggregate transitions record/return them, or rewrite the event model as
application-service facts, not aggregate-raised domain events. Add
`domain/events` to DDD/ubiquitous-language docs if it remains a standalone
context.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 18 — External idempotency semantics are ambiguous

`domain/messaging/headers.go:12-19`: idempotency, ordering, dedup, source,
route, tenant, and forwarding headers use the reserved `x-bridge.*` namespace.
Reserved headers are stripped from untrusted ingress
(`domain/messaging/envelope.go:65-79`, `runtime/route/runner.go:336-342`),
but some reserved headers are intentionally sent over egress for
bridge-to-bridge propagation (`adapters/mqtt/.../analysis_more_test.go:25-35`).

**Fix:** Separate trusted bridge-internal headers from externally accepted
idempotency/dedup keys. Model external idempotency as a first-class field or
explicitly accepted non-reserved header. Document which `x-bridge.*` headers
may cross bridge-to-bridge boundaries and which must never be visible to
non-bridge consumers.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 19 — Full lint gate is currently broken

`reports/golangci.log:1-3`: `make lint` fails at `golangci-lint`.
`.golangci.yml:22`: repo config uses version `"2"`, but the installed tool is
v1. Downstream custom-analyzer reports are stale until this is fixed.

**Fix:** Pin/provision golangci-lint v2 in local tooling and CI. Add a
version preflight before running module lint loops.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 21 — Plugin registration docs and lint exemptions are stale

`ports/plugin_config.go:77-82` documents explicit per-composition-root
registration and no process-wide singleton. `PLUGIN.md:569-604` still
describes `init()` registration and duplicate panics.
`.golangci.yml:458-473` exemptions still describe `DefaultRegistry` and
adapter `init()` self-registration.

**Fix:** Update `PLUGIN.md` to the explicit `Register(reg *ports.Registry) error`
model. Remove or narrow stale `gochecknoglobals` and `gochecknoinits`
exemptions.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 22 — Plugin/CDK checkers mirror registration manually

`scripts/pluginsym/main.go:41-45` and `:118-132`: plugin symmetry checker
builds a hard-coded registry list. `scripts/registrychk/main.go:148-163`:
registry coverage checker does the same.

**Fix:** Parse actual composition-root `Register(reg)` calls. Or centralize
registry construction in importable code used by both runtime and checkers.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

### Medium

#### Finding 23 — Adapter dependency rules are too broad

`.go-arch-lint.yml:690-712`: transport adapters may depend on almost every
domain context, including persistence, routing, and events.

**Fix:** Narrow `mayDependOn` per adapter role and technology. A transport
sender/receiver should not gain persistence/routing dependencies without an
explicit architecture-policy change.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 24 — Architecture mapping tests do not include negative dependency fixtures

`scripts/lint-arch-mapping-test.sh`: the mapping test proves
package-to-component mapping, not forbidden edges.

**Fix:** Add negative fixtures or depguard mirrors for core forbidden edges:
domain to ports; ports to HTTP/DB/SDK; adapter to sibling adapter; runtime
leaf to forbidden parent/sibling; adapter to config except config adapters.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 25 — `cfgshape` documentation overstates test enforcement

`scripts/cfgshape/analyzer.go:23-31` says plugin `Validate` methods need
same-package test references. `scripts/cfgshape/analyzer.go:486-495`: that
enforcement is intentionally disabled, making the header misleading.

**Fix:** Re-enable a reliable test-reference check, or update the analyzer
docs and `LINT.md` to avoid false confidence.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 26 — ACL lint checks imports, not exported SDK-shaped types

`scripts/aclcheck/analyzer.go:62-69`: `aclcheck` blocks direct SDK imports
outside ACL files, but an ACL file can still export SDK-derived
aliases/wrappers for non-ACL files to use, bypassing the intent of the check.

**Fix:** Add type-origin checks. Or ban exported SDK-derived aliases/types
from ACL files unless explicitly annotated.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 27 — Retry backoff lacks route-level jitter

`domain/routing/policy.go:61-66`: `BackoffPolicy` has no jitter field.
`runtime/route/retry.go:14-32`: retry delay is deterministic exponential
backoff, risking synchronized retry storms under shared failure conditions.

**Fix:** Add `JitterFactor` or similar to `BackoffPolicy`. Apply jitter to
route retry delays.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 28 — Ordering semantics are not represented in the outbox port

`domain/messaging/headers.go:18`: `HeaderOrderingKey` exists.
`ports/stores.go:33-36`: `OutboxStore.Claim` has no per-key ordering contract.
`runtime/outbox/loop.go:101-125`: drain batches can process records
concurrently depending on drainer configuration, which can violate ordering.

**Fix:** Add explicit ordered-route policy and per-key serialization rules.
Reject ordered routes with unsafe concurrency, or make the drainer enforce
per-ordering-key sequencing.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 29 — `LeaseInfo` is a mutable DTO despite aggregate-root documentation

`DDD.md:165-198`: `LeaseInfo` is documented with aggregate-root/fencing
semantics. `domain/persistence/lease.go:13-20`: `LeaseInfo` is a public
mutable data struct with mutable `Endpoints`.

**Fix:** Either model lease lifecycle as an aggregate with guarded transitions
and copied endpoints, or document `LeaseInfo` as a snapshot DTO and make
`LeaseStore` own the invariants.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 30 — `Receiver.Run` contract underspecifies callback semantics

`ports/transport.go:22-27`: `Receiver.Run` does not state whether `emit` may
be called concurrently, what an `emit` error means, or how delivery ownership
is handled after callback return.

**Fix:** Specify concurrency, backpressure, settlement, and retry
implications. Add adapter conformance tests for callback error behavior.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 31 — `EventPublisher.Publish` cannot surface failures

`ports/event_publisher.go:15-22`: `Publish` returns no error and says
slow-sink policy is an implementation concern. Event loss is invisible to
application services unless every implementation logs/metrics consistently.

**Fix:** Decide whether domain events are best-effort observability or
reliable facts. If reliable, return an error or route through an outbox. If
best-effort, rename/document accordingly and require metrics for drops.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

### Low

#### Finding 32 — Layer table wording contradicts the domain context map

`ARCHITECTURE.md:85-88`: layer table says `domain/` may import standard
library only. `DDD.md:47-57` and `.go-arch-lint.yml:316-325` allow
domain-context imports such as persistence to messaging/shared and routing to
messaging/shared.

**Fix:** Reword to "no outer-layer project packages and no vendor; only
documented domain context-map edges."

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 33 — `domain/events` exists in lint but not in DDD bounded-context overview

`.go-arch-lint.yml:130-139`: `domain_events` is a declared Layer 1 component.
`DDD.md:9-18`: the bounded-context overview lists six contexts and omits
events.

**Fix:** Add events to the bounded-context table or remove the standalone
domain-events component.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 34 — Plugin docs still mention panic-based duplicate registration

`PLUGIN.md:603-604`: docs say duplicate `reg.Register` panics.
`ports/plugin_config.go:93-116`: implementation returns `ErrDuplicateKind`.

**Fix:** Update docs and examples to match the actual error return.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

#### Finding 35 — Store conformance tests miss owner divergence

`ports/storetest/outbox.go:236-253` covers wrong version.
`ports/storetest/outbox.go:430-449` covers older token after newer claim.
Neither covers same-version/different-owner completion or
`claim-time ownerID != token.Owner`.

**Fix:** Add explicit divergence cases before changing the port signature.

> Verify: the in-flight working-tree changes may already address this — confirm against current code before starting.

---

## Recommended order

The following is the original review's recommended implementation order, minus
the already-completed items (findings 6, 8, 9, 20 and `Envelope`
encapsulation):

1. Fix the golangci-lint v2 toolchain mismatch so `make lint` is meaningful (finding 19).
2. Remove `ownerID` from `OutboxStore.Claim` and `runtime/outbox.Config` (finding 1).
3. Standardize completion fencing across memory, SQLite, and DynamoDB (finding 2).
4. Split or token-guard outbox expiry (finding 3).
5. Add lease/partition binding to outbox fencing semantics (finding 4).
6. Introduce secret-safe value objects for HTTP API keys and plugin configs (findings 5, 7).
7. Resolve JSON/tag format-neutrality decisions for domain and ports (findings 10, 11).
8. Tighten custom linters and stale docs (findings 19, 21, 22, 25, 26).
9. Narrow adapter dependency rules and add negative architecture fixtures (findings 23, 24).
