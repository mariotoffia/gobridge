# GoBridge — Architecture Review (Merged)

**Date:** 2026-05-11
**Method:** Five specialised reviewers (Clean Architecture, Hexagonal, DDD, Anti-Patterns/Extensibility, Documentation) ran in parallel against the repository at HEAD. Each produced an independent report; this document merges and reconciles them.

**Per-reviewer reports:** This document is a merged synthesis of five independent reviewer passes — Clean Architecture (`clean-arch-reviewer`), Hexagonal (`hexagonal-reviewer`), DDD (`ddd-expert`), Anti-Patterns / Extensibility (`anti-patterns-auditor`), and Documentation (`doc-reviewer`). The individual source reports were not retained; reviewer names are kept here for traceability only.

---

## 1. Headline Verdict

| Dimension | Grade | One-line verdict |
|---|---|---|
| **Clean Architecture** | **A** | Layering machine-enforced; dependency rule clean across all layers. |
| **Hexagonal Architecture** | **A** | Could serve as a teaching reference; per-adapter Go modules + vendor allowlist. |
| **DDD — Strategic** | **A** | Bounded contexts, ubiquitous language, ACLs all exemplary. |
| **DDD — Tactical** | **C+** | Aggregates are anemic; no domain events; invariants live in adapters. |
| **Anti-patterns / Extensibility** | **B+** | Excellent foundations; `runtime/` is borderline god-package and `ports.DefaultRegistry` is a panicking singleton. |
| **Documentation** | **B+** | Voice is exceptional; type-name drift in two flagship docs (`ARCHITECTURE.md`, `PLUGIN.md`). |
| **Overall** | **A−** | Architecturally above average for Go OSS. Sound, solid, extensible, well-documented — with named, well-bounded refactors to push it to A+. |

The user's four review questions answered:

1. **Generally sound and solid?** **Yes.** Inner ring (`domain/`, `ports/`) is genuinely stdlib-only, dependency direction is clean, no cyclic dependencies, concurrency is disciplined (`clock.Clock` injected everywhere, no `time.Sleep` in production code, `make audit-timings` enforces it).
2. **Adheres to DDD, Hexagonal, Clean Architecture?** **Yes, with one tactical-DDD gap.** Strategic DDD, Hexagonal, and Clean Architecture are all enforced by `.go-arch-lint.yml` + custom analyzers (`aclcheck`, `aggcheck`, `cfgshape`, `registrychk`). Tactical DDD is the weakest dimension: aggregates are exported-field structs without invariant-enforcing constructors.
3. **Easy to extend and reason about?** **Adapters: yes** (mechanical, half-day for a new transport). **Core: less so** — `runtime/` has 108 files in one Go package, and the composition lifecycle (`Builder.Prepare → Complete → Runtime.New → AddRoute → Start`) has implicit ordering that requires reading several files to understand.
4. **Documentation proper, easy to understand and extend?** **Mostly yes.** Voice is exceptional, the agent contract (`AGENT.md`) and bounded-context model (`DDD.md`) are best-in-class. Two flagship documents (`ARCHITECTURE.md`, `PLUGIN.md`) carry stale `domain.X` symbol names that won't compile against current code. Adapter layout drift (`adapters/amqp/` missing from two diagrams). No scenarios index page.

---

## 2. Cross-Cutting Strengths (consensus from ≥3 reviewers)

1. **Machine-checked architecture.** `.go-arch-lint.yml` (471 lines, `deepScan: true`, `depOnAnyVendor: false`) plus per-component vendor allowlists makes the architecture's load-bearing rules executable, not aspirational. Every Hex/Clean/DDD reviewer cited this independently as exemplary.
2. **Per-adapter Go modules.** Strongest possible isolation — an SDK upgrade in one adapter cannot leak into another, and consumers pay only for what they import.
3. **Pure inner ring.** Verified by grep: `domain/**` and `ports/**` import only Go stdlib (+ sibling domain packages for `ports/`). No vendor SDK, no `encoding/json`, no `yaml.*`.
4. **Symmetric adapter shape.** Every transport adapter follows the same file convention (`acl_*.go`, `factory.go`, `register.go`, `config.go`, `config_plugin.go`, `receiver.go`, `sender.go`, `doc.go`). Adding a new transport is mechanically reproducible.
5. **Anti-Corruption Layers explicit and consistent.** `acl_*.go` file naming + `aclcheck` analyzer prevents vendor SDK types from leaking past the adapter boundary.
6. **Behavioural conformance suites for ports.** `ports/storetest/{lease,outbox,dlq}.go` is shared by every store implementation (memory, sqlite, dynamodb) — port substitutability is verified, not assumed.
7. **Single, honest composition root.** `cmd/gobridge/main.go` (~200 LOC) is the only place that imports both `bridge` and concrete adapters. Lint-enforced (`cmd: { anyProjectDeps: true }`).
8. **Concurrency hygiene.** `domain/clock/clock.go` deliberately omits `Sleep` so cancellability is type-enforceable; `clock.Clock` is injected everywhere; `make audit-timings` blocks `time.Sleep` in production code.
9. **Ubiquitous language is real.** *Subject* (logical) vs *Address* (transport) is consistently respected across `domain/`, `ports/`, every adapter ACL, and most documentation.
10. **Voice discipline in docs.** Zero hits across all `*.md` for `leverage`, `utilize`, `seamless`, `holistic`, `streamline`, `empower`, `delve`, `pivotal`, `intricate`, `foster`. Direct, terse, lowercase imperatives — uniform across files.

---

## 3. Cross-Cutting Weaknesses (consensus from ≥2 reviewers)

| # | Issue | Cited by | Severity |
|---|---|---|---|
| W-1 | **`runtime/` is monolithic** — 108 files in one Go package, `Runtime` struct with 25 fields and 16 `With…` options. | Clean-Arch, Anti-Patterns | **High** |
| W-2 | **Tactical DDD: aggregates are anemic.** `OutboxRecord`, `Envelope`, `LeaseInfo`, `DLQEntry` declared as aggregate roots but lack invariant-enforcing constructors and state-machine methods; transitions live in adapters and can drift between implementations. | DDD | **High** |
| W-3 | **No domain events.** Despite this being a message bridge, there are no first-class domain events (`OutboxClaimed`, `LeaseTransferred`, `DLQRecorded`) — only operational hooks (`DeliveryAttempt`, `AuditEvent`). | DDD | **Medium** |
| W-4 | **`ports.BridgeConfig` carries `yaml:"…" json:"…"` tags inside the inner ring.** Documented trade-off — tags are inert and no encoding runtime is imported by `ports/` — but it's a recognised purity concession. Same root cause as the lone tag on `domain/routing/policy.go:97 AllowRetryDrop`. | Clean-Arch, Hex, DDD | **Low** |
| W-5 | **`ports.DefaultRegistry` is a process-wide mutable singleton that `panic`s on duplicate registration.** Couples test isolation, prevents safe re-import in plugins/embedded use. | Anti-Patterns | **High** |
| W-6 | **Stringly-typed transport coupling in core.** `runtime/route_runner_dispatch.go:33` hard-codes `if transport == "mqtt" { ValidateMQTTTopic(addr) }`. Adding NATS/Kafka topic validation requires editing the runtime. | Anti-Patterns, Hex | **Medium** |
| W-7 | **Hidden builder lifecycle ordering.** `Builder.Prepare → Complete → Runtime.New → AddRoute → Start` is implicit; comments warn about ordering hazards (e.g. credential-refresher must close before sessions). A single `Build(ctx)` would prevent miscomposition. | Anti-Patterns | **Medium** |
| W-8 | **Two parallel registration mechanisms.** Plugins must self-register a `PluginConfig` decoder (`init()` → `ports.DefaultRegistry`) **and** be registered with the supervisor (`sup.RegisterTransport(name, factory)`). SQS even registers two kinds (`"sqs"` and `"aws.sqs"`) to paper over the mismatch. Nothing detects "kind decodable but never wired". | Anti-Patterns, Hex | **Medium** |
| W-9 | **`config/` plays two roles.** Both shared-kernel DTO holder *and* the only inner-ring component allowed to import `yaml.v3`/`mapstructure`. Splitting into `config/parser` (adapter-side) + `config/model` (inner-ring) would eliminate the only inner-ring vendor concession. Also: route-graph validation in `config/validate.go` duplicates `validate/` purpose. | Clean-Arch | **Low** |
| W-10 | **Documentation drift on type names.** `ARCHITECTURE.md` and `PLUGIN.md` still write `domain.Envelope`, `domain.LeaseToken`, `domain.OutboxRecord`, etc., predating the bounded-context split into `messaging/`, `persistence/`, `routing/`, `connectivity/`, `shared/`. Code snippets won't compile. | Docs, Anti-Patterns | **Critical** for those two docs |
| W-11 | **`adapters/amqp/` missing from layout diagrams** in `ARCHITECTURE.md §13` and `DEVELOPMENT.md` (READMEs ship). | Docs | **High** |
| W-12 | **No scenarios index page.** 21 scenarios + 5 CDK scenarios under `docs/scenarios/` with no `README.md` / landing page. | Docs | **High** |
| W-13 | **`bridge/` semantic role unclear.** Positioned as Layer-2 "composition factory" but its job — assembling a runtime from declarative config — reads more like Layer-4 composition. Dependency rule still holds; mostly a docs/labelling mismatch. | Clean-Arch | **Low** |
| W-14 | **Driving-port surface is implicit.** `*runtime.Runtime` and `*bridge.Supervisor` are concrete-typed entry points for HTTP admin; no narrow `RuntimeQuery` / `RuntimeCommand` interfaces in `ports/`. Idiomatic Go, but couples HTTP admin to concrete types. Becomes important the moment a second driving adapter (CLI, gRPC) is added. | Hex | **Medium** (situational) |
| W-15 | **Several core packages lack `doc.go`:** `observability/`, `circuitbreaker/`, `logging/`, `processors/{filter,transform,tenant,circuitbreaker}`. Elevated as non-negotiable in `AGENT.md §2` yet missing package-level overview. | Docs | **Medium** |
| W-16 | **Stray design doc + agent runner-state JSON in repo root** (`2026-05-07-aws-filebased-config-cdk-redesign-design.md`, `*.work-tasklist*.json`) violates `AGENT.md §5.4`. | Docs | **Medium** |
| W-17 | **Naming asymmetry on registration verbs.** `RegisterTransport` (no `Factory` suffix) vs `RegisterStoreFactory` (with) vs `RegisterProcessor`/`RegisterEndpointResolver`/`RegisterDeliveryHook` (instances). Same verb, four mental models. | Anti-Patterns | **Low** |
| W-18 | **`Envelope.Headers map[string]any`** drives reflection / regex / type-coercion ladders in `runtime/condition.go`. A typed `messaging.Headers` value object would eliminate ~30% of that file. | Anti-Patterns | **Medium** |

---

## 4. Consolidated Findings — by Severity

### Critical
- **C-1** Stale `domain.X` symbols in `ARCHITECTURE.md` (lines 278, 299, 364, 528, 532, 570-572, 587-591, 979) and `PLUGIN.md` (lines 24, 40, 55, 124, 138, 140, 188-204, 285, 290, 291, 311, 326, 351-354, 370, 376, 377, 394, 398, 406, 447, 449, 465, 474). Rewrite to `messaging.X`, `persistence.X`, `routing.X`, `connectivity.X`, `shared.X` per the actual `domain/` tree. *(Docs F#1, F#2; AP-031)* - DONE
  **Status:** Resolved 2026-05-11. Rewrote 51 stale `domain.X` references in `ARCHITECTURE.md` and `PLUGIN.md` to `messaging.*`, `persistence.*`, `routing.*`, `connectivity.*`, `shared.*` per the actual `domain/` subpackage layout. Round-1 fixes: `PLUGIN.md` import block (line 431) corrected to `domain/messaging`; `ARCH_REVIEW.md` broken per-reviewer report links replaced with synthesis paragraph preserving five reviewer names. Reviewer (`code-reviewer`) verified zero remaining `\bdomain\.[A-Z]` matches and confirmed every subpackage-qualified symbol resolves to a real declaration. **Files:** `ARCHITECTURE.md`, `PLUGIN.md`, `ARCH_REVIEW.md`. **Review:** APPROVED after 1 fix-round by `code-reviewer` (codex: unavailable).

### High
- **H-1** Split `runtime/` into sub-packages (`route`, `outbox`, `dlq`, `session`, `cluster`, `credentials`); leave only `Runtime` coordinator + lifecycle in the root. *(AP-001; Clean-Arch F4)* - BLOCKED

  **Status:** BLOCKED 2026-05-11 — scope too large for single dispatch; requires sub-task decomposition into H-1a..H-1g per implementer plan. Investigation by `refactoring-specialist` (no production files touched) reported ~6.1K LOC production + ~22K LOC tests under `runtime/`, 30 production files cross-referencing private types, and 85 external caller files in `bridge/`, `httpapi/`, `cmd/`, `tests/integration/`, `tests/longrunning/`, `adapters/native/**` — plus arch-lint and `ARCHITECTURE.md`. A single-shot move would leave `make test` / `make lint` red for an extended window, violating the "branch is always green" rule in `AGENT.md` §5.

  **Recommended decomposition (sequenced, each independently green-able):**
  1. **H-1a — `runtime/dlq`**: extract `DLQ`, `DLQEntry`, related stores/handlers; lowest fan-out, safe leaf to validate the splitting pattern.
  2. **H-1b — `runtime/credentials`**: move `NewPollBasedWrapper` and credential-refresh wiring; closes out L-21 follow-on (`bridge/builder.go:74`). Folds `runtime/credential_refresher_hook.go` (19 lines) into `Runtime.AttachCredentialCloser` as a minor cleanup.
  3. **H-1c — `runtime/cluster`**: extract cluster/leader-election/coordination pieces; few external callers.
  4. **H-1d — `runtime/session`**: extract session lifecycle; depends on cluster boundary being settled.
  5. **H-1e — `runtime/outbox`**: extract `OutboxRecord` and outbox machinery. **Must land before H-2** (which promotes `OutboxRecord` to a real aggregate root) so the new aggregate lives in its destination package from day one.
  6. **H-1f — `runtime/route`**: extract route runner + dispatch. **Open architectural question for the architect:** where do `instrumented*.go` and `processor_chain.go` belong — under `route/`, in a dedicated sub-package, or in `runtime/internal/chain`? Resolve before this sub-task is dispatched.
  7. **H-1g — arch-lint + docs**: update `scripts/archlint`, `ARCHITECTURE.md` §13, `DEVELOPMENT.md` layout block, and any `doc.go` headers to reflect the new package boundaries; tighten arch-lint rules to forbid backsliding.

  **Outstanding decisions for the human:**
  - Confirm the sub-task ordering above or re-shuffle (e.g., bring `cluster` before `dlq` if leader-election is on the critical path for some other work).
  - Resolve the `instrumented*.go` / `processor_chain.go` placement question before H-1f is dispatched.
  - Decide whether to re-index `ARCH_REVIEW.md` to add H-1a..H-1g as first-class sidecar tasks (recommended) or to track them in a separate child document.

  **Files in flux:** none — investigation only; no production code touched.

  **Resume options:**
  - (a) Re-author `ARCH_REVIEW.md` H-1 as seven sub-task bullets H-1a..H-1g and re-run the indexer; the runner will then process each in turn.
  - (b) Park H-1 entirely, work the rest of the backlog, return to the split as a dedicated multi-PR program after H-2..H-6 land.

  **Review:** N/A — implementer returned BLOCKED before any reviewer was dispatched. Orchestrator deferred per explicit user instruction to mark blocked and proceed to the next pending task rather than retry as a single dispatch.
- **H-2** Promote `OutboxRecord` to a real aggregate root with `Claim()`, `Complete()`, `Expire()`, `IsClaimable()` methods returning typed `*shared.BridgeError`. Adapter logic reduces to "load → call → persist". *(DDD R1)* - DONE

  **Status:** Resolved 2026-05-11. `OutboxRecord` is now a real aggregate root: identity exported and frozen at construction; lifecycle fields (`status`, `claimedBy`, `claimedAt`, `claimVersion`, `replayCount`, `completedAt`) un-exported and reachable only through `Claim`, `Complete`, `Expire`, and `IsClaimable`. Persistence boundary uses `OutboxSnapshot` DTO + `RehydrateFromSnapshot`; the port now traffics `[]*persistence.OutboxRecord` and the three adapters (`memoryoutbox`, `sqliteoutbox`, `dynamodboutbox`) reduce to load → call → persist over snapshots. State-machine methods return four typed `*shared.BridgeError` sentinels (`ErrInvalidOutboxRecord`, `ErrOutboxNotClaimable`, `ErrOutboxNotInClaimedState`, `ErrOutboxAlreadyTerminal`), all classified `ErrorPermanent`. New table-driven coverage in `domain/persistence/outbox_state_test.go` exercises every state-machine branch (construction, `IsClaimable`, `Claim` legal/stale-token-takeover/equal-token-reject/terminal-reject, `Complete`, `Expire`, snapshot round-trip). 45 files changed (+822 / −535); `make test` and `make lint` green.

  **Follow-ups (not blockers; logged for future passes):**
  - `OutboxRecord.Expire(now time.Time)` discards the `now` argument and the aggregate stores no `expiredAt`; either drop the parameter or persist the timestamp on the snapshot.
  - Invariant duplication: `sqliteoutbox` / `dynamodboutbox` encode `Claim` / `Complete` preconditions in SQL / DDB conditional expressions. Add a contract test under `ports/storetest` that drives every state-machine branch through every adapter, or doc-comment the SQL/DDB clauses that must mirror the aggregate.
  - `NewOutboxRecord` does not validate `RouteID`; harmless today but worth tightening if route IDs become routable handles.
  - `Snapshot()` shallow-copies `DispatchHeaders` and `Envelope.Headers`; doc-comment should call out aliasing for callers that mutate snapshots.
  - (Carried from implementer notes) `M-7` Envelope getter w/ defensive copy; `lastError` field on the aggregate; explicit `claimedUntil` deadline; `H-1e` relocation; Drainer-fake stale-takeover cleanup.
- **H-3** Add a constructor `messaging.NewEnvelope(...)` that validates ID, normalises headers (strip reserved-prefix overrides), stamps `CreatedAt` via injected clock; un-export `Subject`/`Headers` so callers must use methods. *(DDD R2)*
- **H-4** De-globalise `ports.DefaultRegistry`: keep for back-compat, add `bridge.WithRegistry(*ports.Registry)`, replace `panic` on duplicate kind with `ErrDuplicateKind`. *(AP-003)*
- **H-5** Add `adapters/amqp/transport/{amqp091,amqp10}/` rows to `ARCHITECTURE.md §13` and `DEVELOPMENT.md` layout blocks (or replace duplicates with a link to the README block to prevent future drift). *(Docs F#3)*
- **H-6** Add `docs/scenarios/README.md` grouping the 21+5 scenarios by concept (transports, processors, ops, config, observability, AMQP, CDK). *(Docs F#4)*

### Medium
- **M-1** Promote `ports.AddressValidator` capability returned by `TransportFactory`; remove the `"mqtt"` literal from `runtime/route_runner_dispatch.go:33`. Precondition for adding NATS/Kafka without core changes. *(AP-005)*
- **M-2** Introduce typed `messaging.Headers` value object. Migrate `runtime/condition.go` to typed accessors. *(AP-004)*
- **M-3** Single `Builder.Build(ctx)` entry point; keep internal `prepare()`/`complete()` private. *(AP-009)*
- **M-4** CI plugin-symmetry analyzer (mirror `scripts/registrychk`) verifying every registered kind has a wired factory and vice versa. *(AP-008)*
- **M-5** Promote anonymous `interface{ SetRouteID(string) }` style assertions to named, documented interfaces in `ports/`. *(AP-007)*
- **M-6** Introduce `domain/events/` with first-wave events (`OutboxRecordClaimed/Completed/Expired`, `LeaseAcquired/Renewed/Lost`, `DLQEntryRecorded/Redriven`, `BlueprintCommitted`, `CredentialRotated`) carrying `schema_version`. Add `EventPublisher` port; have `httpapi` audit consume typed facts. *(DDD R4)*
- **M-7** Snapshot enforcement on `OutboxRecord` and `DLQEntry` — deep-clone the embedded `Envelope` in constructors; expose via `Snapshot() *messaging.Envelope` accessor. *(DDD R5)*
- **M-8** Move `2026-05-07-aws-filebased-config-cdk-redesign-design.md` to `_design/`; gitignore the `*.work-tasklist*.json` runner-state files. *(Docs F#5)*
- **M-9** Either fix README Quick Start to register the typed config decoder, or replace it with a link to scenario 1. *(Docs F#6)*
- **M-10** Add `doc.go` to `observability/`, `circuitbreaker/`, `logging/`, and the four `processors/*` packages — ~10–20 lines each, mirroring `runtime/doc.go`. *(Docs F#8)*
- **M-11** Add `docs/troubleshooting.md` keyed off `shared.ErrorCode` constants. *(Docs F#7)*
- **M-12** Verify panic-recovery scope around the goroutine spawned at `runtime/processor_chain.go:119`. A processor panic in a fan-out goroutine must not take down the host; if recover is not present, add it with metric + structured log. *(C-005)*

### Low
- **L-1** Drop the lone `json/yaml` tag from `domain/routing/policy.go:97 AllowRetryDrop`, or explicitly declare it parses via a config-side DTO. *(DDD F-03 / R3)*
- **L-2** Split `config/` into `config/parser` (vendor-touching) + `config/model` (inner-ring). Or move parsing to `adapters/native/config/file` entirely. *(Clean-Arch F2)*
- **L-3** Move route-graph validation from `config/validate.go` into the existing `validate/` package. *(Clean-Arch F5)*
- **L-4** Reposition `bridge/` semantically — either docs-rename to "library-mode composition root" or move `Builder` into `cmd/`. *(Clean-Arch F3)*
- **L-5** Add `ports.RuntimeQuery` / `ports.RuntimeCommand` interfaces — only worth doing if a second driving adapter (CLI, gRPC) is on the roadmap. *(Hex F-3)*
- **L-6** Add `IsValid()` helpers on routing enums (`DeliveryMode`, `DispatchMode`, `AckBoundary`, `ExpiredAction`, `FailureAction`) to prevent `RoutePolicy{DeliveryMode: "wat"}` slipping past `WithDefaults()` no-op branch. *(DDD F-10 / R8)*
- **L-7** Add a *Blueprint / Configuration* supporting subdomain entry to `DDD.md`/`UBIQUITOUS.md` cataloguing `BridgeConfig`, `SessionDef`, `BindingDef`, `RouteDef`, `BridgeSettings`, `BlueprintValidationError`, `ConfigStore`. Add the context-map edge (Blueprint → Routing/Connectivity/Persistence, Customer/Supplier). *(DDD F-08 / R7)*
- **L-8** Naming pass: `Builder.RegisterTransport → RegisterTransportFactory` for symmetry with `RegisterStoreFactory`; collapse `bridge.WithLogger` and `runtime.WithLogger` to a single forwarding option. *(AP-010, AP-012)*
- **L-9** Pick one doc format — convert `docs/typed-plugin-config.adoc` and `_design/error-wrapping-policy.adoc` to Markdown, or document the AsciiDoc carve-out in `AGENT.md §1` / `README.md`. *(Docs F#9)*
- **L-10** Add diagrams: PLUGIN.md sequence ("config YAML → decoder → factory → adapter → runtime"), Builder dispatch flow (`bridge/builder*.go`), OutboxDrainer / SharedOutbox flow, clustered lease state machine (acquire / renew / step-down). *(Docs F#10, F#14, F#15)*
- **L-11** Link `spec/httpapi/*.yaml` (OpenAPI for Admin/Monitor) from `README.md` doc index and `ARCHITECTURE.md §10` ("Machine-readable spec: …"). *(Docs F#12, F#13)*
- **L-12** Generate the layout block from `find adapters -mindepth 3 -maxdepth 3 -type d` (run by `make`) so it cannot drift — root cause of W-11. *(Docs §8)*
- **L-13** Add `docs/scenarios/_template.md` with required sections so new scenarios stay structurally consistent (today they range from 90 to 462 lines). *(Docs §8)*
- **L-14** Replace `runtime/route_config.go:119` `panic("runtime: crypto/rand unavailable")` with a constructor-time error returned from `New`, surfaced once at composition. *(AP-011)*
- **L-15** `RoutePolicy.DefaultBackoffPolicy` is a mutable package-level `var`. Replacement `NewDefaultBackoffPolicy()` already exists; remove the var on next major release to close the global-mutability hole. *(DDD F-11 / R9)*
- **L-16** Domain-events vs operational-events terminology: `ports/hooks.go` calls `DeliveryAttempt`/`DeliveryOutcome` "delivery lifecycle events"; `ports/audit.go` calls its struct `AuditEvent`. Neither is a DDD domain event. When M-6 lands, namespace strictly (`domain/events.Event`) so the two concepts cannot be confused. *(DDD F-12)*
- **L-17** README "Project Structure" omits `validate/` (it exists). Add a row. *(Docs F#11)*
- **L-18** Sentinel-error vs structured-error inconsistency: `processors/filter/filter.go:12 ErrRouteRequired = errors.New(...)` (sentinel) vs `domain/shared.BridgeError` everywhere else for per-message errors. Either convert filter setup-time errors to `*BridgeError` with code `ErrCodeInvalidPayload`, or document the convention that setup errors are intentionally unwrapped sentinels. *(AP-014)*
- **L-19** `MatchCondition.Value any` + stringly-typed `Field` and `Operator` in `runtime/condition.go:34-38` could become a small typed sum (`Operator` enum + discriminated `Value` union). Eliminates the entire `coerce`/`reflect` ladder in `condition.go`. *(AP-013)*
- **L-20** Capability strings (`ports/transport.go:152-162` — `CapStatefulSession`, `CapSourceRedelivery`, …) are stringly-typed but at least centralized. Adding a capability requires touching `runtime/validator.go` — discoverable seam, acceptable. Flagged for visibility only. *(AP-030)*
- **L-21** Cross-module sibling import `bridge/builder.go:74 b.pushCredStore = runtime.NewPollBasedWrapper(...)` exports `runtime.NewPollBasedWrapper` solely for the composition root. Acceptable today; if H-1 (split `runtime/`) lands, this should move to `runtime/credentials`. *(AP-020)*
- **L-22** `RouteRunnerConfig` (`runtime/route_runner.go:58-87`) is a 22-field data bag with `WithDefaults`-style logic spread across `newRouteRunner` (~15 default-fill `if cfg.X <= 0 {…}` branches). Collapse via a `defaults()` method on the config. *(AP-006)*
- **L-23** Concurrency: `closeRefresher` in `runtime/bridge.go:218-224` does not accept `ctx` — closer signature loses cancellation. Acceptable since the closer is in-process and bounded; worth adding ctx for symmetry. *(C-001)*
- **L-24** Concurrency: `runtime/bridge.go:228-238` `done` goroutine could outlive `Stop` if ctx fires *and* `wg.Wait` never completes (mitigated by `closeTimeout` line 245). Annotate or restructure. *(C-002)*
- **L-25** Concurrency: no timeout around `closeRefresher` (`runtime/bridge.go:223`). If a watcher is stuck, `Stop` may hang past the user's ctx. Add bounded timeout. *(C-006)*
- **L-26** Concurrency: mixed loop-variable shadowing in `runtime/bridge_start.go:209-219` (`d := drainer`, `e := entry`) — defends a pre-Go 1.22 pattern in a Go 1.25 project. Either trust 1.22 scoping and remove shadowing for consistency, or keep shadowing everywhere. *(C-003)*

### Info
- **I-1** Domain test files (`domain/**/_test.go`) freely import `testify`. Test files are excluded from arch-lint (`excludeFiles: _test\.go`) — inner-ring purity is enforced only on production code. Acceptable; if absolute purity is desired, add a separate lint pass without that exclusion. *(Clean-Arch F6)*
- **I-2** `httpapi/audit.go:7`, `httpapi/admin.go:11` import `domain/messaging` and `domain/shared` directly to read envelope/error fields. Allowed and correct, but means HTTP boundary depends on multiple domain bounded contexts. Confirm intent. *(Clean-Arch F7)*
- **I-3** `_test.go` adapter cross-imports in `httpapi` (`httpapi/admin_dlq_redrive_test.go` imports `adapters/native/store/memorydlq`). Allowed by lint exclusion; acceptable test-double usage. *(Hex F-6)*
- **I-4** `ports.PluginConfig.Kind() string` discriminator (`ports/plugin_config.go:26-37`) is a string-typed registry routing key. Pragmatic and necessary — Go has no sum types. The cleanest plugin-discriminator design available; flagged for visibility only. *(Hex F-5)*
- **I-5** `ports/blueprint.go` YAML/JSON tags are inert metadata (no encoding runtime imported by `ports/`). Add a CONTRIBUTING checklist item: "Adding a new field to `ports/blueprint.go`? Confirm `cfgshape` covers it." *(Hex F-1)*
- **I-6** `BridgeConfig` carries fields like `ConfigWatch *ConfigWatchDef` whose only consumer is the file-watcher adapter. Add inline doc comments indicating the *port consumer* of each subtree. *(Hex F-2)*
- **I-7** Driving adapters are not co-located with driven adapters: `httpapi/` lives at top level, config-source adapters under `adapters/`. Either move `httpapi/` → `adapters/http/admin/`, or document the convention in `ARCHITECTURE.md`. *(Hex F-4)*
- **I-8** Repository ports (`LeaseStore`, `OutboxStore`, `DLQStore` in `ports/stores.go`) live in `ports/` rather than next to their aggregates. Deliberate codebase convention (centralise outward contracts) but DDD-purists locate the repository interface inside the same package as its aggregate root. Either accept in `DDD.md` explicitly, or move into domain packages and have `ports/` re-export type aliases. *(DDD F-07 / R6)*
- **I-9** `domain/connectivity` mixes `credentials.go` (auth material) and `session.go` (desired session shape) — they share a package only because adapters consume both. Optional split into `domain/credentials` and `domain/sessions` reduces coupling surface. Trade-off: one more package + one more arch-lint block. *(DDD F-09 / R10)*
- **I-10** Use cases are not named in the codebase as a separate ring. Operations the bridge offers as a system (start, stop, inject, redrive, redrive-by-filter, rotate-credentials, transfer-lease, commit-blueprint) would be discoverable if grouped under a `usecases/` or `app/` package. Optional — this is a framework, not an application, so terse naming may legitimately stay. *(DDD F-13 / R11)*
- **I-11** `validate/c.out` is a coverage artifact tracked in VCS. Add to `.gitignore` and remove. *(Docs F#16)*
- **I-12** `tests/_test_index.md` (1295 lines) looks auto-generated; if so, add a "DO NOT EDIT — generated by …" header. If hand-maintained, that's maintenance debt. *(Docs F#17)*
- **I-13** `AGENT.md §1` doc table does not list the agent-runner skills/agents folder under `.claude/`. Probably intentional ("not contributor-facing"); confirm and document. *(Docs F#18)*
- **I-14** Heading-style consistency: reference docs use numbered sections (`## 1.`, `## 2.`); guides and scenarios use unnumbered. Consistent within categories. Minor. *(Docs §7)*
- **I-15** Line wrapping inconsistency: some files (e.g. `AGENT.md`) hard-wrap at ~75 columns; others (`ARCHITECTURE.md`) use long lines. Minor, low priority. *(Docs §7)*
- **I-16** `init()` registry race: `Registry.Register` takes `mu.Lock()` for safety even though Go init runs single-threaded. Defensive; no concrete hazard. *(C-004)*
- **I-17** "Two services share data" anti-pattern: inapplicable. Each adapter is a separate Go module with its own data plane; no shared schemas. ✓ *(AP-021)*
- **I-18** Inner ring has zero detected cyclic dependencies. The arch-lint configuration with `deepScan: true` is doing its job. *(AP §Dep Graph)*

---

## 5. Recommended Roadmap

### Phase 1 — Now (1 PR each, ≤2 weeks total)
1. **C-1** — Sweep `domain.* → <context>.*` in `ARCHITECTURE.md` and `PLUGIN.md`. *Single afternoon; restores accuracy of the two flagship docs.*
2. **H-5** — Insert `adapters/amqp/` into the two layout blocks (or link to README).
3. **H-6** — Create `docs/scenarios/README.md`.
4. **M-8** — Move stray design doc out of repo root; gitignore runner-state JSONs.
5. **M-9** — Fix README Quick Start (or link to scenario).
6. **M-10** — Add `doc.go` to the seven core packages missing it.

These six items are pure documentation/hygiene and unblock contributor onboarding. None require code refactor.

### Phase 2 — Next (1–2 sprints)
7. **H-1** — Split `runtime/` into sub-packages. *Highest-leverage code change; reduces cognitive load and makes inter-subsystem deps lintable.*
8. **H-4** — De-globalise the plugin registry; replace `panic` with error.
9. **M-1** — Promote `ports.AddressValidator`; remove `"mqtt"` literal from runtime.
10. **M-3** — Single `Builder.Build(ctx)` entry point.
11. **M-4** — CI plugin-symmetry analyzer.
12. **M-11** — Add `docs/troubleshooting.md`.
13. **M-12** — Verify processor-chain goroutine panic recovery.

### Phase 3 — Then (tactical-DDD push)
14. **H-2** — Promote `OutboxRecord` to a real aggregate root with state-machine methods.
15. **H-3** — Add `messaging.NewEnvelope(...)` constructor; un-export `Subject`/`Headers`.
16. **M-2** — Introduce typed `messaging.Headers` value object.
17. **M-7** — Snapshot enforcement on `OutboxRecord`/`DLQEntry`.
18. **M-6** — Introduce `domain/events/` and an `EventPublisher` port.

### Phase 4 — Continuous
19. All Low-severity items as opportunistic polish.
20. Naming pass (W-17 / L-8).
21. Diagram backfill (L-10).
22. Concurrency hygiene polish (L-23, L-24, L-25, L-26).
23. Info-level items as part of routine cleanup.

---

## 6. What NOT to Change

Based on consensus:

- **Do not loosen `depOnAnyVendor: false`** in `.go-arch-lint.yml`. This is the single most valuable architectural guarantee in the repo.
- **Do not consolidate adapters under one arch-lint component.** The per-technology split is what prevents AWS-SDK leaks into MQTT and similar accidents.
- **Do not allow `bridge` or `httpapi` to import `config` directly.** Current IoC via `ports.ConfigStore` / `ports.BlueprintValidator` is correct.
- **Do not let `runtime/` keep absorbing responsibilities.** Even before splitting it (H-1), be wary of new files landing in that package.
- **Do not turn the fluent voice of the docs into corporate-speak.** The terse, direct style is a real asset.

---

## 7. Bottom Line

GoBridge is a **sound, solid, well-architected Go codebase** that adheres to DDD (strategically), Hexagonal Architecture, and Clean Architecture — with all three enforced by `.go-arch-lint.yml` + custom analyzers, not just documented. It is **easy to extend at the adapter boundary** (mechanically reproducible), **moderately hard to reason about in `runtime/`** (108 files / one Go package), and **well-documented with two specific drift problems** (stale `domain.X` symbols in `ARCHITECTURE.md`/`PLUGIN.md`, missing `adapters/amqp/` in layout diagrams).

The named refactors — split `runtime/`, de-globalise the registry, promote `OutboxRecord`/`Envelope` to real aggregates, add domain events, fix the documentation drift — are all well-bounded and the codebase is structurally ready for them. None require breaking architectural decisions.

**Final aggregated grade: A−** with a credible path to **A+**.
