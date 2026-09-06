# AWS Deployment Profile Generalization — Implementation Plan

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
`go install`-based `DockerImageAsset`; modules published replace-free on the
release train.

**Tech Stack:** Go 1.25+, AWS SDK v2, AWS CDK v2 (Go/jsii), DynamoDB
(+Streams), Docker, ddblocal test harness.

**Spec:** [DESIGN.md](./DESIGN.md) (same directory — read first; D-numbers
below refer to its Decisions). Binary-side companion:
`cmd/gobridge/DESIGN.md` + `cmd/gobridge/TASKS.md` (execute its Task 1
convention work before Chunk 7 here).

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
`Makefile` (`IMAGE_LOCAL_TAG ?= gobridge-aws:local`, seeder/deployment
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
against `parser.FileStore`).

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

- [ ] **Step 1:** Write suite + FileStore harness; run
  `go test ./ports/configstoretest/... ./config/parser/ -run Conformance -v`
  → FileStore passes ConfigStore subset (fix suite, not store, if red —
  the suite pins *existing* behavior).
- [ ] **Step 2:** Commit — `test: ConfigStore conformance suite; FileStore pinned`

### Task 1.2: Validate/Merge/SaveIfVersion on the DynamoDB loader

**Files:** Modify `adapters/aws/config/dynamodb/loader.go` (three methods +
the two interface assertions from DESIGN.md D4); Test:
`adapters/aws/config/dynamodb/store_conformance_test.go` (ddblocal-backed,
Docker-gated like the module's existing tests, calls `configstoretest.Run`).

- [ ] **Step 1:** Failing conformance run:
  `go -C adapters/aws/config/dynamodb test -run Conformance -v` (Docker up)
  → FAIL (methods missing).
- [ ] **Step 2:** Implement: `Validate` → `config.ValidateWithWarnings`;
  `Merge` → `config.DefaultMerge`; `SaveIfVersion` → conditional `PutItem`
  at `expectedVersion+1` with `version = :expected` condition, condition
  failure → `shared.ErrVersionMismatch`. Reuse the marshal/size-cap path of
  `Save`.
- [ ] **Step 3:** Conformance green; module tests green; `make lint`.
- [ ] **Step 4:** Commit — `feat(config/dynamodb): loader implements ConfigStore and ConditionalConfigStore`

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

- [ ] **Step 1:** Failing table-driven tests in `infra/bootstrap_test.go`:
  `TestValidate_ConfigSourceMatrix` covering: empty→file default; file
  without path → error `config_file_path is required`; dynamodb without
  table → error; dynamodb with `config_file_path` set → error; dynamodb +
  `filesystem_replicated` → error; dynamodb + single / ha → ok.
  `TestEffectivePollInterval_DynamoDBDefault` → 30s when source dynamodb and
  unset.
- [ ] **Step 2:** Implement; run `go -C deployment/aws/infra test ./... -v`.
- [ ] **Step 3:** Update the doc field table; run
  `go -C deployment/aws/lib test -run FieldReference -v` (must be green).
- [ ] **Step 4:** `make lint && make test`; commit —
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

- [ ] **Step 1:** Failing unit tests (no Docker; fake ddb client via the
  loader's client interface or ddblocal-gated where unavoidable):
  `TestNewConfigSource_File_KeepsTodaysWiring` (layer name "file",
  store is `*cfgparser.FileStore`, singleWriter true only for control);
  `TestNewConfigSource_DynamoDB_WiresLoaderAsStore` (layer name "dynamodb",
  same object serves Loader/Watcher/store, singleWriter false);
  `TestStartEmpty_NotFoundFromAnySource` (wrapper returns
  `defaultLogicalConfig` on `shared.ErrNotFound`).
- [ ] **Step 2:** Implement; DevMode → `EnsureTable` on startup (dynamodb
  branch only); reuse/lazily build `a.dynamoDBClient` exactly where the HA
  store factory gets it today (`registry.go:116-121` path — locate, do not
  duplicate construction).
- [ ] **Step 3:** Full module tests:
  `go -C deployment/aws/lib test ./... ` green; `make lint && make test`.
- [ ] **Step 4:** Commit — `feat(deploy/aws): bootstrap-selected config source; CAS store lifts single-writer guard`

### Task 3.2: End-to-end reload over DynamoDB (integration)

**Files:** Test `deployment/aws/lib/bootstrap/app_dynamodb_config_test.go`
(ddblocal-backed, Docker-gated): boot App with `config_source: dynamodb`,
`Save` a changed config through the loader, assert runtime swap via the
existing app-test helpers (injected clock; follow
`app_integration_test.go` patterns).

- [ ] Failing test → implement any missing glue → green →
  `make check-all` → commit —
  `test(deploy/aws): dynamodb-sourced config hot reload end to end`

⛳ Review checkpoint.

---

## Chunk 4 — CDK: config table, seeder mode, conditional EFS (D5)

### Task 4.1: Config table + grants + bootstrap stamping

**Files:** Modify `cdk/constructs/internal/gobridgebase/base.go` (read
`Bootstrap.ConfigSource`; provision table when dynamodb; stamp
`ConfigDynamoDB.TableName`; skip EFS when nothing needs it); Create
`cdk/constructs/internal/grants/configsource.go` (grants per DESIGN.md D5);
Modify facade validation (`internal/validation/`): synth error for
`filesystem_replicated` + dynamodb source; Tests: jsii template assertions
beside the existing construct tests (`!race` pattern):
`config_table_test.go`, `efs_conditional_test.go`.

- [ ] **Step 1:** Failing template tests: dynamodb source → template has one
  `AWS::DynamoDB::Table` for config (PK/SK schema, PITR on,
  `DeletionPolicy: Retain`), task-def has **no** EFS volume when yaml has no
  sqlite paths; file source → EFS present exactly as today; worker role has
  read-only table grant; streams mode adds stream + stream-read grants.
- [ ] **Step 2:** Implement; run
  `go -C deployment/aws/cdk test ./constructs/... -v` green.
- [ ] **Step 3:** `make lint && make test`; commit —
  `feat(deploy/aws/cdk): dynamodb config table with conditional EFS and role-scoped grants`

### Task 4.2: Seeder DynamoDB mode

**Files:** Create `cdk/constructs/internal/seeder/seeder-ddb.sh` (modes per
DESIGN.md D5: SeedOnce/Overwrite/AbortDeploy against the `current` item);
Modify `gobridgebase` seeder container wiring (env: `MODE`, `TABLE`, `PK`,
`EXPECTED_HASH`, `ITEM_S3_URI`; synth marshals via
`parser.MarshalBridgeConfigJSON`, ships item JSON as asset); Extend
`seeder/tests/` bash suite with ddb fixtures (LocalStack/ddblocal per
existing `run.sh` harness); Update `seeder/README.md` + `MANIFEST.md`.

- [ ] **Step 1:** Failing bash tests per mode (seed-when-absent; abort exit
  10 on drift; overwrite CAS bump) — `make -C deployment/aws test`.
- [ ] **Step 2:** Implement script + wiring; template test asserts seeder env
  + `ContainerDependencyCondition_SUCCESS` retained.
- [ ] **Step 3:** Suite green; `make lint && make test`; commit —
  `feat(deploy/aws/cdk): seeder seeds the dynamodb config table with drift modes`

### Task 4.3: Local deployment proof

- [ ] Extend the `integration_local` harness with one scenario: DynamoDBHA +
  dynamodb config source, assert bridge converges and a table-write reload
  round-trips (reuse `rollout_waits.go`/`rollout_probe.go` helpers). Run via
  the harness's documented target; then `make check-all`. Commit —
  `test(deploy/aws): efs-free dynamodb-config deployment proof on local harness`

⛳ Review checkpoint.

---

## Chunk 5 — Image source (D6)

### Task 5.1: Sealed `BridgeImageSource`

**Files:** Create `cdk/internal/imgsource/imgsource.go` (+
`Dockerfile.tmpl` via `go:embed`, `imgsource_test.go`); Modify
`cdk/gobridgecdk/` re-exports (`ImageFromRegistry`, `ImageFromEcrRepository`,
`ImageFromGoBuild`, `ImageGoBuildProps`, `DeriveBuildTags` — signatures
verbatim from DESIGN.md D6); Modify `internal/gobridgebase/base.go:136,430`
prop `Image gobridgecdk.BridgeImageSource` (+ all three facades + their
tests + README snippets).

- [ ] **Step 1:** Failing tests: `TestImageFromRegistry_Materializes` (asset
  ref preserved); `TestImageFromGoBuild_RendersDockerfile` (rendered template
  contains `go install -trimpath -tags=… <pkg>@<version>`, digest-pinned
  bases, nonroot user); `TestDeriveBuildTags_MapsKindsBeyondProfileBase`
  (`amqp091→gobridge_amqp091`, `servicebus→gobridge_azure`, base kinds → no
  tag, unknown kind → error); facade template test: nil Image → panic
  message unchanged.
- [ ] **Step 2:** Implement; `go -C deployment/aws/cdk test ./... -v` green.
- [ ] **Step 3:** `make lint && make test`; commit —
  `feat(deploy/aws/cdk): sealed BridgeImageSource; CDK builds the image via go install`

### Task 5.2: Publish the seeder image on the release train

**Files:** Modify `.github/workflows/release.yml` (job: build + push
`ghcr.io/mariotoffia/gobridge-seeder` by digest from
`cdk/constructs/internal/seeder/Dockerfile`); Modify
`seeder/scripts/update-image.sh` + `image.txt` (point at the pushed digest);
Update `MANIFEST.md` (remove the "broken until SeederImage overridden" note
once true).

- [ ] Local proof first: `docker build` the seeder Dockerfile, run the bash
  suite against that image (PyYAML present, exit 50 impossible) → wire the
  release job → `make lint && make test` → commit —
  `fix(deploy/aws): working default seeder image published on the release train`

⛳ Review checkpoint.

---

## Chunk 6 — Publication (D7)

### Task 6.1: Release-train membership

**Files:** Modify `scripts/release/modules.json` (three entries + layers per
DESIGN.md D7; `testutil/testcontent` into `bootstrap_modules`); `RELEASE.md`
(deployment exception + dead-tags note for the orphaned
`aws-filebased-config` v0.3.x tags); `MODULES.md`; `DEVELOPMENT.md:236`.

- [ ] `make modules-check` green; `make verify-release-preparation` green;
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

**Depends on:** the `gobridge_<family>` tag convention from
`cmd/gobridge/TASKS.md` — its pluginsym rework (Task 1) and at least one
family (Tasks 3-4) landed, so the tag names and lint posture exist
repo-wide. (`lib/bootstrap` itself is guarded by `registry_wiring_test.go`,
not pluginsym.)

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
  synth (4.1/4.3), external `go get` consumer (6.2), working seeder default
  (5.2), gates green (every chunk).
- Open questions 1-3 gate Chunks 0, 4 (HA default stays `file`), and the
  streams grants in 4.1 respectively.
