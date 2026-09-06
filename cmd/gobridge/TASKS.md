# cmd/gobridge Build-Tag Composition — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> `superpowers:subagent-driven-development` (recommended) or
> `superpowers:executing-plans` to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `cmd/gobridge` a **blank root** — an untagged `go build` links
no transport, store or exporter — whose capabilities are selected at compile
time via additive `gobridge_<family>` build tags, with lint-enforced
registration symmetry preserved.

**Architecture:** Stub-pair files per plugin family inside `package main`;
`main.go` and every other untagged file register nothing and only call the
aggregates in `plugins.go`. `pluginsym` reworked from single-file to per-file
symmetry (which makes every tag combination symmetric by construction) plus a
rule that unconstrained files carry no registrations (which makes the blank
root a lint fact). MQTT and the native stores are ordinary families. The
Kubernetes image keeps today's plugin set through explicit default tags.
The gate and the split it gates land in the same task; observability hooks
land before the cloud transport families because wire functions consume the
metrics exporter (the mqtt/native wire functions ignore it and receive nil
until then).

**Tech Stack:** Go 1.25+, `go/ast` + `go/build/constraint` (pluginsym),
golangci-lint, GitHub Actions, Docker.

**Spec:** [DESIGN.md](./DESIGN.md) (same directory — read it first; §2 has the
canonical family-file shape, the shared wire signature and the seed-function
shape; §4 the pluginsym algorithm and rules R1–R5).

## Skill protocol (applies to every task)

| Situation | Required skill |
|---|---|
| Implementing any step that produces code | `superpowers:test-driven-development` — red, green, refactor; never write implementation before its failing test |
| Any test failure, lint failure, or unexpected behavior | `superpowers:systematic-debugging` — root cause before fix; no "make the test simpler" |
| Task complete, before its commit | `superpowers:verification-before-completion` — run the named commands, read the output, only then claim done |
| After each task group (marked ⛳) | `superpowers:requesting-code-review`, then `superpowers:receiving-code-review` for the findings; loop fix → re-review until no findings |
| Starting execution | `superpowers:using-git-worktrees` for an isolated worktree off `main` |
| All tasks done | `superpowers:finishing-a-development-branch` |

## Global constraints

- `make lint` and `make test` green at the end of **every** task — both from
  repo root; failures pre-dating the branch are still yours to fix on it.
- No code file may exceed 500 lines (`wc -l` before committing).
- Kind names in wiring calls are **string literals** (pluginsym contract);
  factory maps/loops are forbidden in `cmd/gobridge`.
- **Untagged files register nothing.** No `Register`, `RegisterTransport`,
  `RegisterTransportFactory` or `RegisterStoreFactory` call may live outside
  a `plugins_<family>.go` file (pluginsym R5).
- No `init()` registration of decoders/factories; only the one-line
  `compiledFamilies` append is allowed in `init()`.
- No task/plan identifiers in code, comments, test names, or commit messages
  (`scripts/lint-planning-refs.sh` gate). Name tests after behavior.
- Manual test binaries get a `.out` postfix.
- Tests: injected clocks/channels, no `time.Sleep`, per `TESTS.md`. A test
  that pins a family's behaviour carries that family's constraint
  (`//go:build gobridge_<family> || gobridge_all`); blank-root tests are
  untagged.
- Shared wire signature for every family (DESIGN.md §2):
  `func wire<Family>Factories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error`
  — stubs return nil. Store-providing families (native, aws) add
  `func seed<Family>Stores(ctx context.Context, b *bridge.Builder) error`.
- Naming for any new concept: check `UBIQUITOUS.md` first.

---

### Task 1: pluginsym per-file symmetry + the blank root

One task, two commits. Commit A rewrites the gate; commit B empties `main.go`
so the gate can be green. They are one task because the walker, once it sees
the whole package, rejects `seed_managed_subscriptions.go` (wires
`memory`/`sqlite`, registers nothing) — the very hole R5 exists to close —
and the only non-scaffolding fix is the split itself.

**Files (commit A — gate):**
- Modify: `scripts/pluginsym/main.go` (walk dir, per-file parse, constraint
  classification, per-file symmetry R1–R5; keep alias collapse)
- Modify: `scripts/pluginsym/main_test.go` (fixture-driven tests)
- Create: `scripts/pluginsym/testdata/` fixture files (see step 1)
- Modify: `scripts/pluginsym/go.mod` + `go.sum` (requires for sqs, awsstore,
  servicebus, amqp091, amqp10 so `adapterRegistrars` can invoke them; http
  transport is in the root module already required)
- Modify: `scripts/pluginsym/README.md` (document the per-file rules)
- Modify: `Makefile` pluginsym invocation: `-dir cmd/gobridge` replaces `-main`

**Files (commit B — split):**
- Create: `cmd/gobridge/plugins.go` (untagged: `var compiledFamilies []string`
  with the doc comment from DESIGN.md §3; `registerAllDecoders` /
  `wireAllFactories` / `seedAllStores` aggregators from DESIGN.md §2 listing
  mqtt + native)
- Create: `cmd/gobridge/plugins_mqtt.go` + `plugins_mqtt_stub.go`
  (`registerMQTTDecoders` → `paho.Register`; `wireMQTTFactories` wires
  literal `"mqtt"` to `paho.NewFactory(logger)`; `init()` appends `"mqtt"`)
- Create: `cmd/gobridge/plugins_native.go` + `plugins_native_stub.go`
  (`registerNativeDecoders` → `nativestore.Register`; `wireNativeFactories`
  wires `"memory"`/`"sqlite"`; `seedNativeStores(ctx, b)` wires the same two
  literals on the Builder; `init()` appends `"native"`)
- Modify: `cmd/gobridge/main.go` — delete the paho/nativestore imports and
  both literal blocks (`:97-104`, `:211-213`); call `registerAllDecoders(reg)`
  and `wireAllFactories(ctx, sup, logger, nil)` (`nil` until Task 3 supplies
  the exporter); rewrite the package doc comment (`main.go:1-19`) for the
  blank root
- Modify: `cmd/gobridge/seed_managed_subscriptions.go` — replace the two
  `RegisterStoreFactory` literals with `seedAllStores(ctx, b)`; drop the
  `nativestore` import
- Modify: `cmd/gobridge/seed_managed_subscriptions_test.go` — constraint
  `//go:build gobridge_native || gobridge_all`; move the parse-only tests
  (`TestParseManagedSubscriptionBaselines_*`) to an untagged
  `seed_managed_subscriptions_parse_test.go` so they keep running on the
  blank root
- Modify: `deployment/kubernetes/Dockerfile` — `ARG GO_BUILD_TAGS="gobridge_mqtt,gobridge_native"`,
  `-tags "$GO_BUILD_TAGS"` on the build line (the image keeps today's set)
- Test: `cmd/gobridge/plugins_test.go` (blank-root assertions),
  `plugins_mqtt_test.go`, `plugins_native_test.go` (tagged)

**Interfaces (produced, used by every later task):**
- CLI: `pluginsym -dir <pkgdir>`; exit 1 on any violation.
- Rules per non-test file in the dir: (R1) adapter-Register kinds == wired
  literal kinds after alias collapse, wired kinds collected
  receiver-agnostically (Supervisor and Builder calls alike, deduped); (R2)
  an adapter import path may register in exactly one file; (R3) a file whose
  constraint negates a family tag (stub) must contain zero Register/wire
  calls; (R4) a file whose constraint mentions a family tag positively must
  be exactly `gobridge_<family> || gobridge_all`; (R5) a file with no family
  tag in its constraint must contain zero Register/wire calls.
- `aliasMap` additions: `azure.servicebus→servicebus`,
  `amqp.amqp091→amqp091`, `amqp.amqp10→amqp10` (existing: `aws.sqs→sqs`,
  `mqtt.paho→mqtt`).
- `adapterRegistrars` additions: sqs, awsstore, servicebus, amqp091, amqp10,
  httptransport.
- `compiledFamilies []string`; `registerAllDecoders(reg) error`;
  `wireAllFactories(ctx, sup, logger, metrics) error`;
  `seedAllStores(ctx, b) error`; the mqtt and native family functions.

- [ ] **Step 1: Write failing fixture tests.** Fixtures under
  `scripts/pluginsym/testdata/`: `family_ok/` (empty main.go +
  `plugins_aws.go` with matching register+wire, passes), `family_seed_ok/`
  (`plugins_aws.go` wiring `"dynamodb"` on both a Supervisor and a Builder,
  passes — R1 dedupes), `family_asymmetric/` (`plugins_aws.go` registers
  decoders but wires nothing → R1 fail), `stub_with_calls/` (stub file with a
  wire call → R3 fail), `duplicate_adapter/` (same adapter registered in two
  files → R2 fail), `bad_constraint/` (`//go:build gobridge_aws && linux` →
  R4 fail), `untagged_wiring/` (unconstrained file with a
  `RegisterStoreFactory` literal → R5 fail). Test names:
  `TestPerFileSymmetry_<Fixture>`. Run
  `go -C scripts/pluginsym test ./... -run TestPerFileSymmetry -v` — expect
  FAIL (walker not implemented).
- [ ] **Step 2: Implement the walker.** Replace the single-file parse:
  `os.ReadDir`, skip `_test.go`, parse each file, classify the constraint
  with `go/build/constraint.Parse` on the `//go:build` line (no line →
  unconstrained file). Reuse the existing collectors per file; evaluate
  R1–R5; keep the registrar-invocation mechanism per file.
- [ ] **Step 3: Fixture tests PASS.** Update
  `TestBuildRegisteredKinds_LiveAdapters` to walk `../../cmd/gobridge` and
  require `mqtt`/`mqtt.paho` from `plugins_mqtt.go`, `memory`/`sqlite` from
  `plugins_native.go`, and nothing from `main.go` — it FAILS at this point
  (files do not exist yet); that is the red for commit B.
- [ ] **Step 4: Commit A** — `feat(pluginsym): per-file registration symmetry across build-tagged composition files`
  (`make lint` is expected red on `cmd/gobridge` between A and B; the task
  boundary is what must be green).
- [ ] **Step 5: Failing blank-root tests.** Untagged
  `TestBlankRoot_RegistersNoKinds`: fresh `ports.NewRegistry()` through
  `registerAllDecoders`, assert `Kinds()` is empty and `compiledFamilies` is
  empty. Tagged `TestMQTTFamily_RegistersDecoders` (kinds `mqtt`,
  `mqtt.paho`; family `"mqtt"`), `TestNativeFamily_RegistersDecoders` (kinds
  `memory`, `sqlite`; family `"native"`),
  `TestNativeFamily_SeedStoresMatchWiredStores` (a Builder seeded through
  `seedAllStores` resolves the same store names `wireAllFactories` registers
  on a Supervisor). Run untagged and with `-tags gobridge_mqtt` /
  `-tags gobridge_native` → FAIL (nothing compiles yet).
- [ ] **Step 6: Implement the split** — the family pairs, `plugins.go`, the
  `main.go` / seed-file edits, the test-file constraint move, the Dockerfile
  default.
- [ ] **Step 7: Verify.** All runs pass: untagged, `-tags gobridge_mqtt`,
  `-tags gobridge_native`, `-tags gobridge_all`. Then
  `go -C cmd/gobridge build -o /tmp/g.out . && go version -m /tmp/g.out` →
  no `adapters/mqtt`, no `adapters/native/store`, no sqlite modules; rebuild
  with `-tags gobridge_mqtt,gobridge_native` → present. Run the blank binary
  against a yaml naming `type: mqtt` → unknown-kind error (pin this in
  `main_test.go` following its existing patterns). `make lint` — pluginsym
  passes R1–R5 with `main.go` and the seed file at ∅; read
  `reports/pluginsym.log`; `make test` green.
- [ ] **Step 8: Commit B** — `feat(gobridge): blank composition root; MQTT and native stores become build-tag families`

⛳ **Review checkpoint** (`superpowers:requesting-code-review`): the gate is
load-bearing for every later task and this commit changes what a plain
`go build` produces — review the walker, the Dockerfile default and the seed
path before proceeding.

---

### Task 2: Self-describing binary (-version, usage, startup log)

**Files:**
- Modify: `cmd/gobridge/main.go` — `var version, gitSHA string` (`dev` when
  unstamped), `-version` flag, `flag.Usage` text and startup log derived
  from `compiledFamilies` + `reg.Kinds()`; a WARN when both are empty
- Modify: `cmd/gobridge/plugins.go` — `pluginSummary()`, `versionLine()`
- Test: `cmd/gobridge/plugins_test.go`

**Interfaces:**
- Produces: `pluginSummary() string` — format `families=[<sorted>]` — used
  by `-version`, usage, and the startup log; `versionLine() string` —
  `gobridge <version> (<gitSHA>) <pluginSummary()>`.

- [ ] **Step 1: Failing tests.** `TestPluginSummary_NoFamilies`
  (`families=[]`), `TestPluginSummary_SortsFamilies`,
  `TestVersionLine_UnstampedIsDev`,
  `TestStartupLog_WarnsWhenNothingLinked` (follow the existing `main_test.go`
  log-capture patterns; empty registry and empty families → the WARN and the
  `-tags gobridge_` hint). Run
  `go -C cmd/gobridge test -run 'PluginSummary|VersionLine|StartupLog' -v`
  → FAIL.
- [ ] **Step 2: Implement**; replace the hardcoded strings at `main.go:63-70`
  and `main.go:91-92`.
- [ ] **Step 3: Tests pass; `make lint && make test` green.**
- [ ] **Step 4: Commit** — `feat(gobridge): binary reports compiled plugin families and decodable kinds`

---

### Task 3: OTel family (`gobridge_otel`) — observability hooks

Lands before the cloud transport families because their wire functions take
the `metrics ports.MetricsExporter` argument this task makes real (Task 1
passes `nil`).

**Files:**
- Create: `cmd/gobridge/plugins_otel.go` (constraint
  `gobridge_otel || gobridge_all`; `init()` appends `"otel"`) and
  `cmd/gobridge/plugins_otel_stub.go` (`!gobridge_otel && !gobridge_all`)
- Modify: `cmd/gobridge/main.go` — call the hooks **before**
  `bridge.NewSupervisor`; when non-nil add
  `bridge.WithSupervisorMetrics(me)` / `bridge.WithSupervisorTracer(tr)` and
  `httpapi.WithMetrics(me)`; pass the exporter to `wireAllFactories`;
  register both close funcs into the existing bounded-shutdown path (exactly
  once, after supervisor stop); **delete the commented example block** at
  `main.go:215-263`
- Modify: `cmd/gobridge/go.mod`: require `adapters/otel/metrics`,
  `adapters/otel/tracing` at last published tags
- Test: `cmd/gobridge/plugins_otel_test.go` (tagged) + untagged assertions in
  `plugins_test.go`

**Interfaces produced (identical signatures in both files):**
- `newMetricsExporter(ctx context.Context, logger *slog.Logger) (ports.MetricsExporter, func(context.Context) error, error)`
- `newTracer(ctx context.Context, logger *slog.Logger) (ports.Tracer, func(context.Context) error, error)`
- Stub: `(nil, nil, nil)` → `run()` adds no supervisor option (runtime keeps
  Noop defaults). Tagged: `otelmetrics.New(ctx)` / `oteltracing.New(ctx)`
  (endpoints via standard `OTEL_EXPORTER_OTLP_*` env; constructing does not
  dial).

- [ ] **Step 1:** Failing untagged test `TestBlankRoot_NilObservability`
  (both hooks return nils) → failing tagged test
  `TestOTelFamily_ConstructsExporterAndTracer` (non-nil exporter, tracer,
  close funcs; run with `-tags gobridge_otel`).
- [ ] **Step 2:** Implement pair + `run()` wiring + example-block deletion.
- [ ] **Step 3:** Both runs pass; `make lint && make test` green.
- [ ] **Step 4: Commit** — `feat(gobridge): optional OTel exporters behind gobridge_otel build tag`

---

### Task 4: AWS family (`gobridge_aws`)

**Files:**
- Create: `cmd/gobridge/plugins_aws.go` + `cmd/gobridge/plugins_aws_stub.go`
  — copy the exact shape from DESIGN.md §2 (client via
  `awsconfig.LoadDefaultConfig(ctx)` + `dynamodb.NewFromConfig`; literals
  `"sqs"`, `"aws.sqs"`, `"dynamodb"`; metrics threaded into
  `sqsadapter.NewFactory` when non-nil; `seedAWSStores` wires `"dynamodb"`
  on the Builder; `init()` appends `"aws"`)
- Modify: `cmd/gobridge/plugins.go` — aggregators gain the AWS lines
  (including `seedAllStores`)
- Modify: `cmd/gobridge/go.mod` (+ `go.sum`): require
  `adapters/aws/transport/sqs`, `adapters/aws/store`, and
  `github.com/aws/aws-sdk-go-v2/{config,service/dynamodb}` at the versions
  the adapter modules already use (no replaces)
- Test: `cmd/gobridge/plugins_aws_test.go` (tagged) + `plugins_test.go`

**Interfaces:**
- Consumes: Task 1 rules and aggregators; Task 3 `metrics` argument.
- Produces: `registerAWSDecoders(reg *ports.Registry) error`,
  `wireAWSFactories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error`,
  `seedAWSStores(ctx context.Context, b *bridge.Builder) error`.

- [ ] **Step 1: Failing tagged test.** `TestAWSFamily_RegistersDecoders` —
  fresh `ports.NewRegistry()`, call `registerAWSDecoders`, assert `Kinds()`
  contains `sqs`, `aws.sqs`, `dynamodb`; `TestAWSFamily_ReportsFamily`
  asserts `compiledFamilies` contains `"aws"`. Run
  `go -C cmd/gobridge test -tags gobridge_aws -run AWSFamily -v` → FAIL.
- [ ] **Step 2: Name every family kind in the blank-root test.** Add untagged
  `TestBlankRoot_ExcludesEveryFamilyKind` listing every family kind
  (`mqtt`, `memory`, `sqlite`, `sqs`, `dynamodb`, `servicebus`, `amqp091`,
  `amqp10`, `http`) so a future untagged leak names the kind. It must pass
  from the start; it exists to fail loudly later.
- [ ] **Step 3: Implement** both files + aggregator lines + go.mod.
- [ ] **Step 4:** Both runs pass. Then
  `go -C cmd/gobridge build -o /tmp/g.out . && go version -m /tmp/g.out` →
  no `aws-sdk-go-v2` modules; rebuild with `-tags gobridge_aws` → present.
- [ ] **Step 5:** `make lint` — pluginsym must pass the new pair (R1–R5; the
  seed literal dedupes against the wire literal).
- [ ] **Step 6: Commit** — `feat(gobridge): optional AWS plugin family behind gobridge_aws build tag`

---

### Task 5: Azure family (`gobridge_azure`)

Same pattern as Task 4 (transport-only: no seed function); per-family
specifics:

**Files:** Create `cmd/gobridge/plugins_azure.go` + `plugins_azure_stub.go` +
`plugins_azure_test.go`; modify `plugins.go` (aggregator lines), `go.mod`
(require `adapters/azure/transport/servicebus`).

**Interfaces produced:** `registerAzureDecoders(reg *ports.Registry) error`
(calls `servicebus.Register`); `wireAzureFactories` (shared wire signature)
wiring literals `"servicebus"`, `"azure.servicebus"` to one
`servicebus.NewFactory(logger)` instance (check whether that factory takes a
variadic metrics exporter like SQS — thread `metrics` if so); `init()`
appends `"azure"`.

- [ ] Failing tagged test `TestAzureFamily_RegistersDecoders` (kinds
  `servicebus`, `azure.servicebus`) → implement → tagged + untagged pass →
  `make lint && make test` → commit —
  `feat(gobridge): optional Azure Service Bus family behind gobridge_azure build tag`

---

### Task 6: AMQP families (`gobridge_amqp091`, `gobridge_amqp10`)

Two independent stub pairs, same pattern as Task 5; per-family specifics:

**Files:** Create `plugins_amqp091.go`/`_stub.go`/`_test.go` and
`plugins_amqp10.go`/`_stub.go`/`_test.go`; modify `plugins.go`, `go.mod`
(require `adapters/amqp/transport/amqp091`, `adapters/amqp/transport/amqp10`).

**Interfaces produced:** `registerAMQP091Decoders` / `wireAMQP091Factories`
(literals `"amqp091"`, `"amqp.amqp091"`, factory `amqp091.NewFactory(logger)`);
`registerAMQP10Decoders` / `wireAMQP10Factories` (literals `"amqp10"`,
`"amqp.amqp10"`, factory `amqp10.NewFactory(logger)`); families `"amqp091"`,
`"amqp10"`. Shared wire signature; thread `metrics` if the factories accept
it.

- [ ] Failing tagged tests per family → implement → four combinations pass
  (each tag alone, both, none) → `make lint && make test` → commit —
  `feat(gobridge): optional AMQP 0-9-1 and AMQP 1.0 families behind build tags`

---

### Task 7: HTTP family (`gobridge_http`)

Same pattern; http transport lives in the **root module**
(`adapters/http/transport`, no new go.mod require).

**Files:** Create `plugins_http.go`/`_stub.go`/`_test.go`; modify
`plugins.go`.

**Interfaces produced:** `registerHTTPDecoders(reg) error`
(`httptransport.Register`); `wireHTTPFactories` (shared wire signature)
wiring literal `"http"` to
`httptransport.NewFactory(httptransport.WithFactoryLogger(logger))`, adding
`httptransport.WithFactoryMetrics(metrics)` when `metrics != nil` (mirrors
`deployment/aws-filebased-config/lib/bootstrap/registry.go:106-111`).
Family `"http"`.

- [ ] Failing tagged test (kind `http`) → implement → pass → lint/test →
  commit — `feat(gobridge): optional HTTP transport family behind gobridge_http build tag`

⛳ **Review checkpoint** after Task 7.

---

### Task 8: Build plumbing

**Files:**
- Modify: `Makefile` — `GOBRIDGE_TAGS ?=` (empty → blank root) +
  `build-gobridge` target per DESIGN.md §5; extend the `test` recipe with a
  second `go -C cmd/gobridge test -tags gobridge_all ./...` pass
- Modify: `.golangci.yml` — `build-tags: [gobridge_all]`
- Modify: `.github/workflows/ci.yml` — build + vet passes for `cmd/gobridge`
  both untagged and with `-tags gobridge_all`

(The Dockerfile change landed in Task 1.)

- [ ] **Step 1:** Makefile target; verify `make build-gobridge` produces a
  blank `cmd/gobridge/gobridge.out` (`-version` → `families=[]`) and
  `make build-gobridge GOBRIDGE_TAGS="gobridge_aws gobridge_otel"` lists AWS +
  OTel modules in `go version -m`.
- [ ] **Step 2:** golangci + CI edits; `make lint` locally; push branch,
  confirm CI green on both passes.
- [ ] **Step 3:** `docker build -f deployment/kubernetes/Dockerfile
  -t gobridge-k8s:default .` → `docker run --rm gobridge-k8s:default -version`
  → `families=[mqtt native]`; rebuild with
  `--build-arg GO_BUILD_TAGS=gobridge_all` → all families listed.
- [ ] **Step 4: Commit** — `build: tag-selectable gobridge binary in make, lint and CI`

---

### Task 9: Durable docs + plan retirement

**Files:**
- Modify: `PLUGIN.md` — new section "Binary composition (build tags)": the
  family table from DESIGN.md §1, the blank-root rule, the stub-pair rules,
  the shared wire + seed signatures, the per-file symmetry contract
  (R1–R5 in plain English), and an "adding a family" checklist (pair files +
  pluginsym registrar + aliasMap + go.mod + golangci/CI)
- Modify: `DEVELOPMENT.md` — building with tags (`make build-gobridge`,
  `GOBRIDGE_TAGS`); a plain `go build` is blank by design; note that go.mod
  keeps all adapter requires while the linker trims
- Modify: `TESTS.md` — add the family tags to the build-tag table; the
  rule that family-behaviour tests carry the family constraint; note the
  `gobridge_all` test pass
- Modify: `UBIQUITOUS.md` — terms **Plugin family**, **Family tag**,
  **Blank root** (definitions from DESIGN.md §1)
- Modify: `docs/container-deployment.md`, `docs/deployment-guide.md`,
  `deployment/kubernetes/` docs — wherever the MQTT + native set is
  described as what `go build` gives you, say `-tags gobridge_mqtt,gobridge_native`
  / `GO_BUILD_TAGS`; `docs/release-notes.md` — call the blank default out as
  breaking for local builds (the shipped image is unchanged)
- Delete: `cmd/gobridge/DESIGN.md`, `cmd/gobridge/TASKS.md`

- [ ] **Step 1:** Write the doc updates; every rule in plain English, no
  references to this plan.
- [ ] **Step 2:** `make lint` (planning-refs checker stays green) and
  `make test`.
- [ ] **Step 3:** Delete the two planning files. Commit —
  `docs: document plugin-family build tags and the blank root; retire binary composition plan`

⛳ **Final review** (`superpowers:requesting-code-review` on the whole
branch), then `superpowers:finishing-a-development-branch`.

## Self-review (spec coverage)

- DESIGN §1 families → Task 1 (mqtt, native), Task 3 (otel), Tasks 4-7
  (cloud/amqp/http). §2 stub pairs, wire signature, seed function,
  untagged-files-register-nothing → Task 1 and every family task. §3
  self-describing + `-version` → Task 2. §4 pluginsym R1–R5 → Task 1. §5
  plumbing → Task 8 (Dockerfile default in Task 1). §6 ldflags → Task 2
  (vars) + Task 8 (Makefile stamps). Docs/glossary/release note → Task 9.
- Acceptance bullets in DESIGN.md map to: blank root (Task 1 steps 5, 7),
  today's set via two tags (Task 1 step 7, Task 8 step 3), gobridge_all
  build (Task 8), lint with R5 (Task 1 step 7), tests untagged + all (Task 8
  step 1), yaml kind behaviour (Task 1 step 7, Task 4 step 2).
