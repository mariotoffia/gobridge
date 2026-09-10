# AWS Deployment Profile Generalization — Implementation Plan

## Overview

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> `superpowers:subagent-driven-development` (recommended) or
> `superpowers:executing-plans` to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the AWS deployment profile source its bridge config from file
*or* DynamoDB, let the CDK build the container image (with selectable plugin
families), and make the three modules consumable by external CDK apps.

**Architecture:** Bootstrap-declared config source behind one `App` seam;
DynamoDB loader completed into a CAS config store; facades provision the
config table + conditional EFS; sealed `BridgeImageSource` with a
versioned-module `DockerImageAsset`; modules published replace-free on the
release train.

**Tech Stack:** Go 1.25+, AWS SDK v2, AWS CDK v2 (Go/jsii), DynamoDB
(+Streams), Docker, ddblocal test harness.

**Spec:** [DESIGN.md](./DESIGN.md) (same directory — read first; D-numbers
below refer to its Decisions). The reference binary's implemented family-tag
and registration contract is in
[PLUGIN.md](../../PLUGIN.md#binary-composition-build-tags).

**Current scope:** implement embedded initial configuration and remove the
configuration-seeder dependency. This work stays on the current branch without
delegation, worktrees, stash, reset, commits, or pushes. The parent owns code
integration and final gates. Historical checked steps record earlier work;
they do not certify the redesigned lifecycle. Publication and optional-family
tasks remain open.

## Skill protocol (applies to every task)

| Situation | Required skill |
|---|---|
| Implementing any code step | `superpowers:test-driven-development` — failing test first, minimal green, refactor |
| Any test/lint failure or unexpected behavior | `superpowers:systematic-debugging` — root cause before fix; never weaken a test to pass it |
| Before claiming a task done | `superpowers:verification-before-completion` — run the named commands, read output |
| End of each chunk (⛳) | `superpowers:requesting-code-review` → `superpowers:receiving-code-review`; loop fix → re-review until no findings |
| Starting execution | `superpowers:using-git-worktrees` |
| All chunks done | `superpowers:finishing-a-development-branch` |

## Global constraints

- `make lint` and `make test` green after every chunk; `make check-all` after
  Chunks 4, 5, 7 (Docker-backed integration).
- No code file over 500 lines (`wc -l` before commit).
- No plan/task identifiers in code, comments, test names, file names, or
  commit messages (`scripts/lint-planning-refs.sh`). Tests named after
  behavior.
- Naming: check `UBIQUITOUS.md` (root + local) before introducing any term.
- Tests: injected clocks, `testutil/wait`, Docker-gated via `testing.Short()`
  + probe — never a bare `integration` build tag (`TESTS.md` §5.2).
- Adapter conventions: compile-time interface assertions
  (`var _ ports.X = (*T)(nil)`), functional options.
- Paths below assume Chunk 0's rename; if D1 is vetoed, keep
  `deployment/aws-filebased-config/` everywhere.

---

## Chunk 0 — Rename to `deployment/aws` (D1)

**Decision gate:** confirm D1 with the user before executing this chunk.

### Task 0.1: Move + re-path the module tree

**Files:** `git mv deployment/aws-filebased-config deployment/aws`;
`git mv deployment/aws/lib/cmd/gobridge-filebased deployment/aws/lib/cmd/gobridge-aws`;
module lines in `deployment/aws/{infra,cdk,lib}/go.mod`; every in-repo import
of the old module paths; `go.work` entries; root `Dockerfile`
(`ARG BINARY_MODULE=deployment/aws/lib`, `BINARY_PKG=./cmd/gobridge-aws`);
`Makefile` (`IMAGE_LOCAL_TAG ?= gobridge-aws:local`, image/deployment
targets); `.github/workflows/*` path references; env constants
`EnvBootstrapJSON = "GOBRIDGE_BOOTSTRAP_JSON"` /
`EnvBootstrapFile = "GOBRIDGE_BOOTSTRAP_FILE"` in `lib/bootstrap/config.go`
and every doc/test naming the old names.

- [ ] **Step 1:** `git grep -l 'aws-filebased-config\|gobridge-filebased\|GOBRIDGE_FILEBASED'`
  — record the full list; that list is the change set (docs included).
- [ ] **Step 2:** Apply moves + edits; `make dev` to regenerate `go.work`.
- [ ] **Step 3:** Verify: the same grep returns only `RELEASE.md`'s
  dead-tags note (added in Chunk 6) and git history references — nothing
  else; `make build && make lint && make test` green.
- [ ] **Step 4:** Commit — `refactor: rename deployment profile to deployment/aws; profile binary to gobridge-aws`

⛳ Review checkpoint.

---

## Chunk 1 — DynamoDB loader becomes a config store (D4)

### Task 1.1: ConfigStore conformance suite

**Files:** Create `ports/configstoretest/suite.go` (+ `doc.go`); Test:
`config/parser/store_conformance_test.go` (runs the ConfigStore subset
against `parser.FileStore`); update FileStore version assignment and pin
admin commit/rollback behavior.

**Interfaces produced:**
```go
package configstoretest
// Run exercises the ports.ConfigStore contract; if the store also
// implements ports.ConditionalConfigStore, the CAS cases run too.
func Run(t *testing.T, newStore func(t *testing.T) ports.ConfigStore)
```
Cases: Load-after-Save round-trip; Save bumps `Version`; Validate returns
warnings not errors for valid config; Merge(base, overlay) honors
`config.DefaultMerge`; missing document → `shared.ErrNotFound` (file:
`os.ErrNotExist` accepted via `errors.Is` bridge — assert either);
CAS: `SaveIfVersion` with stale version → `shared.ErrVersionMismatch`,
with current version → success.

- [x] **Step 1:** Write suite + FileStore harness; run
  `go test ./ports/configstoretest/... ./config/parser/ -run Conformance -v`
  → FileStore passes ConfigStore subset. User-approved behavior change:
  stores assign the persisted version plus one and update the caller's
  `Version` only after success. Rollback restores content under a new version.
- [x] **Step 2:** Commit — `feat(config): versioned FileStore saves and ConfigStore conformance suite`

### Task 1.2: Validate/Merge/SaveIfVersion on the DynamoDB loader

**Files:** Modify `adapters/aws/config/dynamodb/loader.go` and `acl_params.go`;
add `store.go` (three methods + interface assertions, keeping loader.go
within the file-size limit); preserve integer precision in the shared JSON
marshaller. Test:
`adapters/aws/config/dynamodb/store_conformance_test.go` (ddblocal-backed,
Docker-gated like the module's existing tests, calls `configstoretest.Run`).

- [x] **Step 1:** Failing conformance run:
  `go -C adapters/aws/config/dynamodb test -run Conformance -v` (Docker up)
  → FAIL (methods missing).
- [x] **Step 2:** Implement: `Validate` → `config.ValidateWithWarnings`;
  `Merge` → `config.DefaultMerge`; `SaveIfVersion` → conditional `PutItem`
  at `expectedVersion+1` with `version = :expected` condition (zero can also
  create an absent row or adopt a versionless row), condition failure →
  `shared.ErrVersionMismatch`. Share the marshal/size-cap path with `Save`;
  keep JSON, row, and caller versions consistent.
- [x] **Step 3:** Conformance green; module tests green; `make lint`.
- [x] **Step 4:** Commit — `feat(config/dynamodb): loader implements ConfigStore and ConditionalConfigStore`

⛳ Review checkpoint.

---

## Chunk 2 — Bootstrap discriminator (D2)

### Task 2.1: Fields, defaults, validation matrix

**Files:** Modify `deployment/aws/infra/bootstrap.go` (constants
`ConfigSourceFile`/`ConfigSourceDynamoDB`, `ConfigDynamoDBSettings`, two new
`BootstrapConfig` fields, `DefaultDynamoDBPollInterval`, `Normalized()`
empty→file, `Validate()` matrix from DESIGN.md D2); mirror aliases in
`deployment/aws/lib/model/bootstrap.go` (type aliases — verify only); Modify
`docs/aws-deployment/configuration.md` field table (same commit — pinned by
`lib/bootstrap/bootstrap_field_reference_test.go`).

**Interfaces produced:** exact field/JSON names from DESIGN.md D2 —
`config_source`, `config_dynamodb.{table_name,watch_mode,stream_poll_interval}`.

- [x] **Step 1:** Failing table-driven tests in `infra/bootstrap_test.go`:
  `TestValidate_ConfigSourceMatrix` covering: empty→file default; file
  without path → error `config_file_path is required`; dynamodb without
  table → error; dynamodb with `config_file_path` set → error; dynamodb +
  `filesystem_replicated` → error; dynamodb + single / ha → ok.
  `TestEffectivePollInterval_DynamoDBDefault` → 30s when source dynamodb and
  unset.
- [x] **Step 2:** Implement; run `go -C deployment/aws/infra test ./... -v`.
  Reject DynamoDB at `App.Start` until source wiring is available, rather
  than letting the file loader start an empty runtime. Covered by
  `lib/bootstrap/config_test.go`; bootstrap schema validation still accepts it.
- [x] **Step 3:** Update the doc field table; run
  `go -C deployment/aws/lib test -run FieldReference -v` (must be green).
- [x] **Step 4:** `make lint && make test`; commit —
  `feat(deploy/aws): bootstrap config_source discriminator with dynamodb settings`

---

## Chunk 3 — App source seam (D3)

### Task 3.1: `newConfigSource` + start-empty generalization

**Files:** Create `deployment/aws/lib/bootstrap/config_source.go`
(`configSource` struct + `(*App).newConfigSource` per DESIGN.md D3); Modify
`startup.go` (`:99-104` and `:211` replaced by the seam; `configSingleWriter`
folded into the seam's `singleWriter`); Modify `config.go`
(`optionalFileSource` → source-agnostic `startEmptySource` wrapper matching
`shared.ErrNotFound` **or** `os.ErrNotExist`); Tests:
`config_source_test.go`, extend `start_empty`-related tests.

**Interfaces produced:**
```go
type configSource struct {
    layer        config.Layer
    store        ports.ConfigStore
    singleWriter bool
}
func (a *App) newConfigSource(ctx context.Context) (configSource, error)
```

- [x] **Step 1:** Failing unit tests (no Docker; fake ddb client via the
  loader's client interface or ddblocal-gated where unavoidable):
  `TestNewConfigSource_File_KeepsTodaysWiring` (layer name "file",
  store is `*cfgparser.FileStore`, singleWriter true only for control);
  `TestNewConfigSource_DynamoDB_WiresLoaderAsStore` (layer name "dynamodb",
  same object serves Loader/Watcher/store, singleWriter false);
  `TestStartEmpty_NotFoundFromAnySource` (wrapper returns
  `defaultLogicalConfig` on `shared.ErrNotFound`).
- [x] **Step 2:** Implement; DevMode → `EnsureTable` on startup (dynamodb
  branch only); reuse/lazily build `a.dynamoDBClient` exactly where the HA
  store factory gets it today (`registry.go:116-121` path — locate, do not
  duplicate construction). Ignore stale DynamoDB admin/watch applies under
  the App lock so delayed commits cannot replace a newer running config.
- [x] **Step 3:** Full module tests:
  `go -C deployment/aws/lib test ./... ` green; `make lint && make test`.
- [x] **Step 4:** Commit — `feat(deploy/aws): bootstrap-selected config source; CAS store lifts single-writer guard`

### Task 3.2: End-to-end reload over DynamoDB (integration)

**Files:** Test `deployment/aws/lib/bootstrap/app_dynamodb_config_test.go`
(ddblocal-backed, Docker-gated): boot App with `config_source: dynamodb`,
`Save` a changed config through the loader, assert runtime swap via the
existing app-test helpers (injected clock; follow
`app_integration_test.go` patterns).

- [x] Failing test → implement any missing glue → green →
  `make check-all` → commit —
  `test(deploy/aws): dynamodb-sourced config hot reload end to end`
  Poll and Streams reloads cover seeded and empty startup; no additional
  production wiring was needed.

⛳ Review checkpoint.

---

## Chunk 4 — CDK: config table and conditional EFS (D5)

### Task 4.1: Config table + grants + bootstrap stamping

**Files:** Modify `cdk/constructs/internal/gobridgebase/base.go` (read
`Bootstrap.ConfigSource`; provision table when dynamodb; stamp
`ConfigDynamoDB.TableName`; skip EFS when nothing needs it); Create
`cdk/constructs/internal/grants/configsource.go` (grants per DESIGN.md D5);
Modify facade validation (`internal/validation/`): synth error for
`filesystem_replicated` + dynamodb source; Tests: jsii template assertions
beside the existing construct tests (`!race` pattern):
`config_table_test.go`, `efs_conditional_test.go`.

- [x] **Step 1:** Failing template tests: dynamodb source → template has one
  `AWS::DynamoDB::Table` for config (PK/SK schema, PITR on,
  `DeletionPolicy: Retain`), task-def has **no** EFS volume when yaml has no
  sqlite paths; file source → EFS present exactly as today; worker role has
  read-only table grant; streams mode adds stream + stream-read grants.
- [x] **Step 2:** Implement; run
  `go -C deployment/aws/cdk test ./constructs/... -v` green.
- [x] **Step 3:** `make lint && make test`; commit —
  `feat(deploy/aws/cdk): dynamodb config table with conditional EFS and role-scoped grants`

### Task 4.2: Historical DynamoDB seeder — superseded

The earlier implementation added a DynamoDB init container, S3 JSON asset,
and drift modes. The approved direction removes that machinery. Task 5 now
owns strict in-process creation for both targets. Config-table provisioning,
conditional EFS, and read-only worker grants remain required.

### Task 4.3: Local deployment proof

- [x] Extend the `integration_local` harness with one scenario: DynamoDBHA +
  dynamodb config source, assert bridge converges and a table-write reload
  round-trips (reuse `rollout_waits.go`/`rollout_probe.go` helpers). Run via
  the harness's documented target; then `make check-all`. Commit —
  `test(deploy/aws): efs-free dynamodb-config deployment proof on local harness`

⛳ Review checkpoint.

---

## Chunk 5 — Image source and embedded initialization (D6)

**State:** implemented and reviewed. Native file embedding replaces the
Linux-limited `GOFLAGS` payload path. Large Linux builds, the local HA deployment
proof, and the complete unit/static/integration gates pass.

### Task 5.1: Sealed `BridgeImageSource`

The build path has been rechecked after the Linux failure.

- [x] Automatically embed the facade's parsed `BridgeConfig` for Go builds.
  Stage `initial-config-<rawSHA>.base64` as pure data; hash the unencoded document.
- [x] Both commands populate `main.initialConfigBase64` with `go:embed` on
  fixed `initial-config.base64`, empty by default.
  Root Make and Docker accept `INITIAL_CONFIG_FILE` as YAML or JSON.
- [x] `scripts/buildconfig` uses only the Go standard library to generate
  Base64 plus a Go overlay, leaving original command source files unchanged.
  Remove the old Go-environment-file helper; no payload may enter flags or env.
- [x] Both commands expose `-initial-config-digest` before runtime/network
  startup, returning only SHA-256 of the embedded bytes. The CDK build verifies
  `/gobridge-filebased -initial-config-digest` against the staged document.
  Missing embed-file/probe support or a mismatched digest fails the build.
- [x] Preserve pinned bases, platform selection, and tags. Config-bearing builds
  download the requested package via Go, copy its owning module to a writable
  directory, fill the embed file, and `go build` with small metadata flags.
  No Git checkout; no-config builds retain `go install package@version`.
- [x] Registry/ECR images remain unchanged; `BridgeConfig` still drives
  validation, grants, and dependencies, not automatic overwrite.
- [x] Document that compatible public `lib` publication and optional-family
  wiring remain prerequisites.

### Task 5.2: Remove the configuration-seeder runtime dependency

Historical note: the earlier task published a working amd64/arm64 image to
Docker Hub. That artifact is not deleted. Its publication does not remain
a runtime or release prerequisite.

- [x] Remove sidecars, init containers, scripts, image pins/publication,
  `SeederImage`, drift-mode APIs, `ConfigAsset`, and config S3 download grants.
- [x] Remove release instructions and scopes for seeder-image publication.
- [x] Preserve managed-subscription baseline seeding and HA rollout
  generation-zero baseline creation.
- [x] Both file and DynamoDB HA baseline recognition use
  `DeploymentBaselineContentDigest`, excluding only the top-level version;
  actual committed artifacts retain their stored version and full digest.

### Task 5.3: Strict creation and observable absence

- [x] Verify `config.Initialize(ctx, target, source, admit)` leaves source
  unread for existing targets and isolates admission mutations. Mutable custom
  plugin configs require `ports.FreezableConfig`; deeply immutable scalar value
  configs do not. `parser.NewInlineSource` supplies fresh logical snapshots.
- [x] Source is `ports.Loader`; target optionally implements
  `ports.ConfigInitializer.CreateIfAbsent(ctx, cfg) (bool, error)`.
  Definitive absence alone permits creation. Target version is 1, source
  version is ignored, and the winner is reread.
- [x] Existing invalid and legacy versionless documents are never overwritten.
  `SaveIfVersion(..., 0)` is not a strict creation primitive.
- [x] With valid bootstrap, start control-plane liveness but not readiness;
  keep data-plane resources idle until valid config activates. Verify reference
  `-admin-addr` and API-key environment startup, legacy boot-file HTTP settings,
  optional monitor/TLS flags, and deprecated `-start-empty` without a runtime.
- [x] After activation, confirmed absence stops intake, drains safely, releases
  standalone resources, and goes idle for a later rebuild. Clustered deletion
  or uncertain teardown must signal process exit and replacement.
  Read timeout/auth/unavailability retains last successful config as degraded.
- [x] First activation is a process latch, not readiness. HA standbys can
  activate without Full. Same-process idle never reseeds; a fresh process may
  initialize an absent target. Do not add a durable tombstone.
- [x] Extend existing watchers with `ConfigObserver`, `ConfigObservation`,
  `ConfigObservationKind`, `ConfigPresent`, `ConfigMissing`, `ConfigReadError`.
  Preserve observation order and recreated versions; no second polling service.
- [x] Verify `Manager.Observe` uses one authoritative layer, rejects overlays,
  and preserves the old `Watch` API. `Manager.NotifyIdle` acknowledges completed
  quiescence without discarding a newer desired snapshot. Do not claim EFS
  durability from local-file tests.
- [x] Keep worker config access read-only in runtime and IAM. Verify authenticated
  `POST /api/v1/admin/config` with complete typed YAML/JSON and strict creation,
  including conflict, committed-not-applied, and ambiguous-commit outcomes.
  S3 config adapter remains deferred.

### Task 5.4: Embedded SQS selection

- [x] Embed stable physical `queue_name` or optional `queue_tags` with
  `queue_name_prefix`, never unresolved URL tokens.
- [x] Reuse existing `GetQueueUrl` for names. Tags use native SQS SDK
  `ListQueues` pagination and `ListQueueTags`, scoped to account and region.
  One match succeeds, none retries/not-ready, multiple error, permission
  failures stay errors.
- [x] Retain CDK queue handles for precise grants and dependencies; additional
  discovery reads only for tag mode. No generic token resolver or required
  two-phase deployment.
- [x] Verify the final APIs: `QueueRegistry.BindQueueTags(name, tags, prefix)`,
  `ResolveQueue`, and `QueueRef.PhysicalName`, `QueueTags`, `QueueNamePrefix`.
  Tag-selected bindings use `sqs.QueueAddress` (`sqs:queue`).
- [x] Verify shared-base `GrantSQSConfig` and image-builder
  `ValidateEmbeddedSQSConfig` wiring. Reject all embedded queue URLs.
  `ScanForPlaintextSecrets` must remain explicit rather than a default ban.

### Task 5.5: Proof and public documentation

- [x] Cover creation races, invalid existing targets, deletion/recreation,
  degraded reads, standby activation, restart, and logical-reference copying.
- [x] Prove native image embedding and no seeder resources in synth/local deployment.
- [x] Local proof builds this checkout with each staged asset's exact
  `.base64` payload; `GOBRIDGE_LOCAL_IMAGE` must pass the embedded-digest check.
  Credentialed fixtures require a compatible published `GOBRIDGE_INT_VERSION`
  and build per-fixture images rather than accepting `GOBRIDGE_INT_IMAGE`.
- [x] Document literal credentials as allowed and artifact-visible; Base64
  is not secrecy. Leave references unresolved in the stored logical copy.
- [x] Re-run final code review, Linux large-payload builds, `make test`, and `make check-all`.
  Run `TestLocal_DynamoDBConfigHotReload` through the local deployment harness
  and confirm runtime initialization and subsequent table-write reloads.

⛳ Review checkpoint.

---

## Chunk 6 — Publication (D7)

### Task 6.1: Release-train membership

**Files:** Modify `scripts/release/modules.json` (three entries + layers per
DESIGN.md D7; `testutil/testcontent` into `bootstrap_modules`); `RELEASE.md`
(deployment exception + dead-tags note for the orphaned
`aws-filebased-config` v0.3.x tags); `MODULES.md`; `DEVELOPMENT.md:236`.

- [x] `make modules-check` green; `make verify-release-preparation` green;
  dry-run `make release VERSION=vX.Y.Z` shows the three modules in
  dependency order. Commit —
  `feat(release): publish deployment/aws infra, lib and cdk modules`

### Task 6.2: External-consumer proof + docs

**Files:** Extend `smoke-released-modules` (scratch module importing
`gobridgecdk` + `gobridgesingle`, `GOWORK=off go build`); Modify `README.md`
("Consuming from your own CDK app" — exact `go get` lines);
`docs/scenarios/cdk/01-quickstart-default-vpc.md` (drop repo-clone; use
`ImageFromGoBuild`); `docs/aws-deployment/container-image.md` +
`cdk-constructs.md`.

- [ ] Smoke target red until go.mod staging works, then green; docs updated;
  `make lint && make test`. Commit —
  `docs(deploy/aws): external consumer workflow; smoke-tested go get path`

⛳ Review checkpoint. **First release after this chunk executes
`RELEASE.md`'s normal train** — `make release VERSION=… CONFIRM=1` from a
clean `release/*` branch; never move a tag.

---

## Chunk 7 — Profile binary families (D8)

**Depends on:** the implemented `gobridge_<family>` tag convention and
per-file registration rules in
[PLUGIN.md](../../PLUGIN.md#binary-composition-build-tags).
`lib/bootstrap` itself is guarded by `registry_wiring_test.go`, not pluginsym.

### Task 7.1: Stub pairs in `lib/bootstrap`

**Files:** Create `deployment/aws/lib/bootstrap/plugins_amqp091.go` (+ stub),
`plugins_amqp10.go` (+ stub), `plugins_azure.go` (+ stub) — each extends the
decoder registry (`config.go:188-199` seam: a `registerExtraDecoders(reg)`
hook) and the factory maps in `registry.go` before their loops (constant
keys allowed here — this root is guarded by `registry_wiring_test.go`, not
pluginsym); Modify `lib/go.mod` (amqp091, amqp10, servicebus requires at
published tags); root `Dockerfile` (`ARG GO_BUILD_TAGS=""` →
`go build -tags`).

- [ ] Failing tagged tests per family in `registry_wiring_test.go` style
  (alias→same-factory `require.Same`; decoder kinds present) —
  `go -C deployment/aws/lib test -tags gobridge_amqp091 -run FactoryRegistry -v`;
  untagged run asserts families absent → implement → all four combinations
  green → `docker build --build-arg GO_BUILD_TAGS=gobridge_amqp091 .` builds
  → `make lint && make test` → commit —
  `feat(deploy/aws): optional amqp091, amqp10 and azure families in the profile binary`

⛳ Review checkpoint.

---

## Chunk 8 — Documentation sweep + plan retirement

**Files:** `ARCHITECTURE.md` (drop every "(planned)" marker — the sections
are then true); local `UBIQUITOUS.md` (glossary additions from DESIGN.md +
de-EFS-ify *Bridge config*, *Logical state*, *Config file path*); root
`UBIQUITOUS.md` deployment section; `docs/config-stores.md` (profile note:
dynamodb reachable as base under the profile); `docs/aws-deployment/`
(configuration.md topology table "Config source" row, storage-and-secrets,
topologies, overview component table); `PLUGIN.md` cross-note for profile
families. Delete `DESIGN.md` + `TASKS.md` (this file) — and confirm no other
file references them (`git grep -l 'DESIGN.md\|TASKS.md' deployment cmd`).

- [ ] Docs updated in plain English, no plan references; `make lint`
  (planning-refs gate) + `make test` green; delete the four planning files
  (this module's two + `cmd/gobridge`'s two if that plan is also complete).
  Commit — `docs: deployment profile documents config sources, image source and external consumption; plans retired`

⛳ **Final review** on the whole branch, then
`superpowers:finishing-a-development-branch`.

## Self-review (spec coverage)

- D1→Chunk 0, D4→Chunk 1, D2→Chunk 2, D3→Chunk 3, D5→Chunk 4, D6→Chunk 5,
  D7→Chunk 6, D8→Chunk 7, glossary/docs→Chunk 8.
- DESIGN.md Acceptance bullets: dynamodb boot+reload+CAS (3.2), EFS-free HA
  synth (4.1/4.3), external `go get` consumer (6.2), embedded initialization and
  missing-config lifecycle (5.1–5.5), gates green (every chunk).
- Open questions 1-3 gate Chunks 0, 4 (HA default stays `file`), and the
  streams grants in 4.1 respectively.
