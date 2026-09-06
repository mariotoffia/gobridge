# cmd/gobridge — Binary Composition Design (build tags)

> Planning document. Deleted when the work in [TASKS.md](./TASKS.md) is fully
> implemented. Durable content is promoted to `PLUGIN.md`, `DEVELOPMENT.md`,
> `TESTS.md`, and `UBIQUITOUS.md` before deletion. Nothing in code, tests, or
> shipped docs may reference this file.

## Problem

`cmd/gobridge` is the reference composition root. It hardcodes exactly one
plugin set — MQTT (paho) + native stores (memory/sqlite) — as the thing you
get from a plain `go build` (`main.go:1-19`, `main.go:97-104`,
`main.go:211-213`). Every other adapter exists only as commented-out example
wiring (`main.go:215-263`). An operator who wants a different set — SQS,
Service Bus, AMQP, OTel, or *fewer* things than MQTT + SQLite — must fork
`main.go`.

Goal: one canonical binary that is **blank** when built with no tags — a
composition root that links no transport, no store and no exporter — and whose
capabilities are added at compile time with `go build -tags`, without
weakening the two machine gates that guard registration symmetry
(`scripts/pluginsym`) and adapter config shape (`scripts/cfgshape`).

## Current state (verified)

| Fact | Where |
|---|---|
| Decoder registration: one literal block | `cmd/gobridge/main.go:97-104` (`paho.Register`, `nativestore.Register`) |
| Factory wiring: one literal block | `cmd/gobridge/main.go:211-213` (`sup.RegisterTransport("mqtt", …)`, `sup.RegisterStoreFactory("memory"/"sqlite", …)`) |
| **Second, untagged wiring site** | `cmd/gobridge/seed_managed_subscriptions.go:76-78` re-wires `"memory"`/`"sqlite"` on a `bridge.Builder` for the `-seed-managed-subscriptions` one-shot; kept in sync with `main.go` by a comment |
| Adapter contract | Each adapter exports `Register(reg *ports.Registry) error` in a file literally named `register.go` (enforced by `scripts/cfgshape/analyzer.go:424-447`) |
| `pluginsym` parses **one file** | `scripts/pluginsym/main.go:164,222` — default `cmd/gobridge/main.go`; plain `go/parser`, ignores build constraints; wired kinds must be **string literals**; never sees `seed_managed_subscriptions.go` |
| `pluginsym` invokes registrars | Hardcoded `adapterRegistrars` map (`main.go:125-128`, only paho + nativestore); its `go.mod` requires only those |
| Alias collapse | `aliasMap` `aws.sqs→sqs`, `mqtt.paho→mqtt` (`main.go:60-63`) |
| No feature build tags exist | Only test/integration tags: `longrunning`, `integration_local`, `integration_aws`, `!race`, GOOS files. `TESTS.md` §5.2 bans a bare `integration` tag |
| Lint does not see tagged files | `.golangci.yml:28` `build-tags: []` |
| `cmd/gobridge/go.mod` requires 5 gobridge modules | gobridge, paho, native config/file, native credentials/file, native store (+httpapi, testutil/wait) — no AWS/Azure/AMQP/OTel |
| No version stamp in this root | `cmd/gobridge` has no `-version` flag and no `main.version`/`main.gitSHA` variables; only `deployment/aws-filebased-config/lib/cmd/gobridge-filebased/main.go:19` carries the `-X` convention |
| Usage/startup text hardcodes the plugin set | `main.go:63-70` (flag.Usage), `main.go:91-92` (startup log) |
| Kubernetes image builds this binary | `deployment/kubernetes/Dockerfile` (`ARG BINARY_MODULE=cmd/gobridge`), no `-tags` — the image's plugin set is whatever the untagged build links |
| Tests that need MQTT/native register them in-test | `seed_managed_subscriptions_test.go:114,159` call `paho.Register` / `nativestore.Register` directly; `TestSeedManagedSubscriptions_EstablishesBaselineTheSessionWillLoad` (`:106`) goes through `seedManagedSubscriptions()` and therefore through its hardcoded native stores |

## Goals

1. Default build (`go build`, no tags) produces a **blank root**: no
   transports, no stores, no exporters linked. It still boots (`-start-empty`
   defaults to true), serves the admin/monitor HTTP API, and rejects every
   config that names a transport or store with the existing unknown-kind
   error. Nothing under `adapters/` is linked except the file config source
   and the file credential store (the skeleton, see Non-goals).
2. Additive per-family tags link transports/stores/exporters — MQTT and the
   native stores included; they are families like any other.
3. `make lint` proves decoder↔factory symmetry for **every** tag combination,
   and proves that no unconstrained file registers or wires anything (Goal 1
   as a lint fact, not a convention).
4. Registration stays literal, `init()`-free, and singleton-free (the
   `ports.Registry` contract in `ARCHITECTURE.md` §Typed Plugin Config).
5. The binary reports what it was compiled with (`-version`, startup log,
   usage text) — no drifting hardcoded strings.

## Non-goals

- Runtime plugin loading (`plugin` package, IPC) — rejected, out of scope.
- Tag-selecting the **skeleton**: the file config source
  (`adapters/native/config/file`) and the file credential store
  (`adapters/native/credentials/file`). A root with no config source cannot be
  told what to do; that is not blank, it is dead. Both register no registry
  kinds, so they are invisible to `pluginsym` and stay untagged in `main.go`.
  Making them selectable needs a config-source seam in `run()` and is
  separate work.
- Config-source selection in `cmd/gobridge` (`-config-source dynamodb`).
  DynamoDB-sourced config is the deployment profile's job
  (`deployment/aws-filebased-config/DESIGN.md`); embedders wire
  `ddbconfig.NewLoader` through the `bridge` library API.
- Credential-backend selection (SSM etc.). The deployment profile owns SSM.
- A convenience tag reproducing today's set (`gobridge_base`). The set is two
  literal tags and appears in two places (the Dockerfile default and the
  docs); a third spelling is drift waiting to happen.
- Shrinking `go.mod`. Build tags trim the **linked binary**, never the module
  graph; `go.mod` gains requires for every optionally-linkable adapter.
- Tag-splitting adapter modules themselves. Tags live only in composition
  roots; `cfgshape` requires every adapter to keep its untagged `register.go`.

## Design

### 1. Plugin families and tags

One tag per adapter *family* (module boundary = tag boundary). Names use the
`gobridge_` prefix so they can never collide with third-party or GOOS tags.

| Tag | Links | Registry kinds added |
|---|---|---|
| *(none — blank root)* | file config source, file credential store | *(none)* |
| `gobridge_mqtt` | paho transport | `mqtt`, `mqtt.paho` |
| `gobridge_native` | native store (memory, sqlite) | `memory`, `sqlite` |
| `gobridge_aws` | sqs transport, aws store factory | `sqs`, `aws.sqs`, `dynamodb` |
| `gobridge_azure` | servicebus transport | `servicebus`, `azure.servicebus` |
| `gobridge_amqp091` | amqp091 transport | `amqp091`, `amqp.amqp091` |
| `gobridge_amqp10` | amqp10 transport | `amqp10`, `amqp.amqp10` |
| `gobridge_http` | http transport (root module, stdlib-only) | `http` |
| `gobridge_otel` | otel metrics + tracing exporters | *(none — observability wiring, not registry kinds)* |
| `gobridge_all` | everything above | union |

`gobridge_all` is not a real tag constant anywhere; every family constraint is
written `//go:build gobridge_<family> || gobridge_all`, so `-tags gobridge_all`
turns everything on. Family tags compose freely
(`-tags "gobridge_mqtt,gobridge_native"` is today's binary;
`-tags "gobridge_aws,gobridge_otel"` is an SQS/DynamoDB bridge with OTel and
no MQTT).

New `UBIQUITOUS.md` terms: **Plugin family** (an adapter group toggled by one
tag), **Family tag** (`gobridge_<family>`), **Blank root** (the untagged
`cmd/gobridge` build: skeleton only, zero transports/stores/exporters).

### 2. File layout — stub pairs, literal registrations

`main.go` **loses both literal blocks** (`:97-104`, `:211-213`). Its only
registration sites become two aggregate calls into untagged `plugins.go`,
which lists every family once:

```go
// plugins.go (untagged) — the ONLY untagged file that names a family.
func registerAllDecoders(reg *ports.Registry) error {
    return errors.Join(
        registerMQTTDecoders(reg),
        registerNativeDecoders(reg),
        registerAWSDecoders(reg),
        registerAzureDecoders(reg),
        registerAMQP091Decoders(reg),
        registerAMQP10Decoders(reg),
        registerHTTPDecoders(reg),
    )
}

// metrics comes from the newMetricsExporter hook (see the OTel family
// below); it is nil unless gobridge_otel is compiled in.
func wireAllFactories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
    return errors.Join(
        wireMQTTFactories(ctx, sup, logger, metrics),
        wireNativeFactories(ctx, sup, logger, metrics),
        wireAWSFactories(ctx, sup, logger, metrics),
        wireAzureFactories(ctx, sup, logger, metrics),
        wireAMQP091Factories(ctx, sup, logger, metrics),
        wireAMQP10Factories(ctx, sup, logger, metrics),
        wireHTTPFactories(ctx, sup, logger, metrics),
    )
}

// seedAllStores mirrors wireAllFactories for the -seed-managed-subscriptions
// one-shot, which builds through a bridge.Builder rather than the Supervisor.
// Only store-providing families contribute.
func seedAllStores(ctx context.Context, b *bridge.Builder) error {
    return errors.Join(
        seedNativeStores(ctx, b),
        seedAWSStores(ctx, b),
    )
}
```

`run()` calls `registerAllDecoders(reg)` where the paho/nativestore block
was, `wireAllFactories(...)` where the supervisor block was, and
`seedManagedSubscriptions` calls `seedAllStores` instead of its own
`RegisterStoreFactory` literals. Untagged files then contain **no**
`Register`/`RegisterTransport`/`RegisterStoreFactory` literals at all —
which is what `pluginsym` enforces (§4 R5).

Each family is a **stub pair** in `package main`. AWS is the fullest example
(client construction, metrics threading, a store, and therefore a seed
function):

`plugins_aws.go`

```go
//go:build gobridge_aws || gobridge_all

package main

import (
    "context"
    "errors"
    "log/slog"

    awsconfig "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/dynamodb"
    awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
    sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
    "github.com/mariotoffia/gobridge/bridge"
    "github.com/mariotoffia/gobridge/ports"
)

func init() { compiledFamilies = append(compiledFamilies, "aws") }

func registerAWSDecoders(reg *ports.Registry) error {
    return errors.Join(sqsadapter.Register(reg), awsstore.Register(reg))
}

func wireAWSFactories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
    awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
    if err != nil {
        return err // gobridge_aws was compiled in: AWS must be usable
    }
    var sqsFactory *sqsadapter.Factory
    if metrics != nil {
        sqsFactory = sqsadapter.NewFactory(logger, metrics)
    } else {
        sqsFactory = sqsadapter.NewFactory(logger)
    }
    sup.RegisterTransport("sqs", sqsFactory)
    sup.RegisterTransport("aws.sqs", sqsFactory)
    sup.RegisterStoreFactory("dynamodb",
        awsstore.NewDynamoDBStoreFactory(dynamodb.NewFromConfig(awsCfg)))
    return nil
}

func seedAWSStores(ctx context.Context, b *bridge.Builder) error {
    awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
    if err != nil {
        return err
    }
    b.RegisterStoreFactory("dynamodb",
        awsstore.NewDynamoDBStoreFactory(dynamodb.NewFromConfig(awsCfg)))
    return nil
}
```

`plugins_aws_stub.go`

```go
//go:build !gobridge_aws && !gobridge_all

package main

import (
    "context"
    "log/slog"

    "github.com/mariotoffia/gobridge/bridge"
    "github.com/mariotoffia/gobridge/ports"
)

func registerAWSDecoders(*ports.Registry) error { return nil }

func wireAWSFactories(context.Context, *bridge.Supervisor, *slog.Logger, ports.MetricsExporter) error {
    return nil
}

func seedAWSStores(context.Context, *bridge.Builder) error { return nil }
```

The MQTT and native pairs are the same shape minus the client:
`plugins_mqtt.go` registers `paho.Register` and wires `"mqtt"` to
`paho.NewFactory(logger)`; `plugins_native.go` registers
`nativestore.Register`, wires `"memory"`/`"sqlite"` to
`nativestore.NewMemoryStoreFactory()` / `NewSQLiteStoreFactory()`, and its
`seedNativeStores` wires the same two literals on the Builder. Transport-only
families (mqtt, azure, amqp091, amqp10, http) have no seed function.

Every family's wire function shares that signature. A nil client is a
wiring-test-only posture for `NewDynamoDBStoreFactory` (its preflight is
skipped); production wiring always builds the real client, matching the
intent of the commented example being replaced.

Rules the pattern encodes:

- **Kind names are string literals** in the tagged file — `pluginsym` keeps
  seeing them.
- **Decoder registration, factory wiring and seed wiring for a family live in
  the same file.** This is the invariant that makes symmetry composable (see
  §4): the file's registered kind set must equal the union of everything it
  wires, on the Supervisor or the Builder.
- **Untagged files register nothing.** `plugins.go` only aggregates calls;
  `main.go` and `seed_managed_subscriptions.go` only call the aggregates.
- The only `init()` permitted is the one-line `compiledFamilies` append. It
  registers nothing — the no-`init()` registration rule is about plugin
  decoders/factories, and those stay explicit calls from `run()`.
- Stubs are no-ops, so `run()` calls every family unconditionally; no
  reflection, no map of functions (a map/loop would be invisible to
  `pluginsym` — the reason `make lint` rejects it).

`gobridge_otel` is the one non-registry family. Its pair provides
observability constructors instead (called in `run()` **before**
`bridge.NewSupervisor` and before any wire function):

```go
// plugins_otel.go (tagged) / plugins_otel_stub.go (inverse)
func newMetricsExporter(ctx context.Context, logger *slog.Logger) (ports.MetricsExporter, func(context.Context) error, error)
func newTracer(ctx context.Context, logger *slog.Logger) (ports.Tracer, func(context.Context) error, error)
```

The stub returns `(nil, nil, nil)`; `run()` then adds no supervisor option
and the runtime keeps its Noop defaults. The tagged file constructs
`otelmetrics.New(ctx)` / `oteltracing.New(ctx)` (endpoints from the standard
`OTEL_EXPORTER_OTLP_*` environment variables — no new flags). When non-nil,
`run()` passes `bridge.WithSupervisorMetrics(me)` /
`bridge.WithSupervisorTracer(tr)` to `NewSupervisor`, threads the exporter
into `wireAllFactories` and `httpapi.WithMetrics`, and calls the returned
close functions exactly once at process shutdown — the exporter and tracer
are shared across hot-reloaded runtimes, so the composition root owns
`Close`, never the runtime. The commented example block at `main.go:215-263`
is deleted — the family files replace it as living, compiled documentation.

### 3. Self-describing binary

`plugins.go` (untagged) also carries:

```go
// compiledFamilies lists the plugin families linked into this binary. A
// blank root has none. Family files append to it from init().
var compiledFamilies []string
```

- New for this root (mirroring the aws-filebased root's convention): `var
  version, gitSHA string` stamped by `-ldflags "-X main.version=… -X
  main.gitSHA=…"`, and a `-version` flag printing
  `gobridge <version> (<gitSHA>) families=[mqtt native]` — a blank root
  prints `families=[]`. Unstamped builds print `dev`.
- The startup log line (`main.go:91-92`) and `flag.Usage` (`main.go:63-70`)
  stop hardcoding a plugin set; they print `compiledFamilies` (sorted) and
  the startup log additionally prints `reg.Kinds()` so operators see the
  exact decodable kinds. A blank root logs at WARN: no transports or stores
  are linked, every routed config will be rejected, rebuild with
  `-tags gobridge_<family>`. The usage text names the tag mechanism and
  points at `PLUGIN.md`.

### 4. pluginsym rework — per-file symmetry

Today `pluginsym` parses one file and checks global symmetry. Replace with:

1. **Walk** every non-test `.go` file in the root package dir (`-dir`,
   default `cmd/gobridge`); parse every file, record its constraint
   expression (`go/build/constraint`, both `//go:build` and filename
   suffixes). A file with no family tag in its constraint is **unconstrained**
   (`main.go`, `plugins.go`, `seed_managed_subscriptions.go`, …).
2. Per file, collect (a) adapter `Register` calls resolved through the
   file's own import table (existing `parseRegisteredAdapterPaths` logic,
   made per-file), (b) wired kinds from string-literal first args of
   `RegisterTransport` / `RegisterTransportFactory` / `RegisterStoreFactory`
   (existing `parseWiredKinds` logic, made per-file) — receiver-agnostic, so
   Builder calls in a seed function count exactly like Supervisor calls.
3. **Per-file symmetry (R1)**: within each file, after alias collapse, the
   canonical kind set produced by (a) must equal the canonical kind set wired
   by (b). Each `plugins_<family>.go` carries its family; stubs and
   unconstrained files carry the empty set (∅ = ∅ passes).
4. **Because every tag either includes or excludes a whole file, per-file
   symmetry implies symmetry for every tag combination.** No combinatorial
   builds, no constraint evaluation beyond "which file declares what".
5. Guard rails: (R2) error if the same adapter import path registers in two
   different files; (R3) error if a stub file (constraint contains a `!` on a
   family tag) contains any Register/wire call; (R4) error if a family file's
   constraint is not exactly `gobridge_<family> || gobridge_all`;
   **(R5) error if an unconstrained file contains any Register/wire call.**
   R5 is what makes Goal 1 a lint fact: the only way to link an adapter is a
   family file, so an untagged build cannot carry one. R5 lands in the same
   task that empties `main.go` (a gate that is red until a later task is not
   a gate).
6. `adapterRegistrars` (`scripts/pluginsym/main.go:125-128`) gains one entry
   per newly linkable adapter; `scripts/pluginsym/go.mod` gains the matching
   requires; `aliasMap` gains `azure.servicebus→servicebus`,
   `amqp.amqp091→amqp091`, `amqp.amqp10→amqp10`.
7. The `registrychk` tool is untouched (it points at the CDK synth registry,
   a different root).

### 5. Build plumbing

| Piece | Change |
|---|---|
| `cmd/gobridge/go.mod` | Add requires: `adapters/aws/transport/sqs`, `adapters/aws/store` (+ its leaf modules as indirect), `adapters/azure/transport/servicebus`, `adapters/amqp/transport/amqp091`, `adapters/amqp/transport/amqp10`, `adapters/otel/metrics`, `adapters/otel/tracing` — each at the last published tag (RELEASE.md policy; no replaces, `cmd/gobridge` is a published module). `adapters/http/transport` is in the root module — no new require. paho and native store are already required. |
| Root `Makefile` | New variable `GOBRIDGE_TAGS ?=` (empty → blank root) and target `build-gobridge`: `go -C cmd/gobridge build -tags "$(GOBRIDGE_TAGS)" -trimpath -ldflags "-s -w -X main.version=$(IMAGE_TAG) -X main.gitSHA=$(GIT_SHA)" -o gobridge.out .` (`.out` postfix keeps the artifact gitignored). |
| `make test` | `go -C cmd/gobridge test ./...` runs twice: untagged (the blank root must compile and its blank-root tests pass) and `-tags gobridge_all` (family tests). Tests that pin a family's behaviour carry that family's constraint (e.g. the seed test needs `gobridge_native`). |
| `.golangci.yml` | `build-tags: [gobridge_all]` so lint analyses every family file in one pass (stubs are then excluded — acceptable: stubs are no-op one-liners; `go vet ./...` without tags still covers them). |
| CI (`.github/workflows/ci.yml`) | The build job runs `go -C cmd/gobridge build ./...` and `vet` **both** untagged and with `-tags gobridge_all`, mirroring how `longrunning` gets its extra vet pass. |
| `deployment/kubernetes/Dockerfile` | `ARG GO_BUILD_TAGS="gobridge_mqtt,gobridge_native"` threaded into the `go build` line (`-tags "$GO_BUILD_TAGS"`). The image keeps today's plugin set; a blank default would ship an image that can route nothing. Lands in the same task as the `main.go` split. |
| `make lint` | `bin/pluginsym` invocation gains `-dir cmd/gobridge` (replacing `-main`); everything else unchanged. |

### 6. Compile-time settings (ldflags)

`-X main.version` / `-X main.gitSHA` are the only ldflags settings (new for
this root, see §3). Plugin selection is tags-only; defaults (paths, ports,
intervals) stay runtime configuration. No other `-X` variables — a setting
that varies per deployment belongs in config, not in the linker.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| Keep MQTT + native as the untagged base | Privileges one plugin set forever: it can never be removed, the SQLite driver rides along in every build, and the seed-path duplication stays an untagged blind spot. A blank root plus R5 makes "nothing is linked unless a family file says so" machine-checked. |
| `gobridge_base` convenience tag for today's set | A third spelling of two literal tags; see Non-goals. |
| Code generator (xcaddy/telegraf style): manifest → generated `main.go` | A second build system to maintain; generated-vs-committed drift; `pluginsym` would parse generated output. Stub pairs get the same result with `go build` alone. |
| Per-plugin tags (`gobridge_sqs`, `gobridge_dynamodbstore`, …) | Tag matrix explodes; families already match module boundaries and grant/IAM boundaries. A family is the unit users reason in. |
| Subtractive tags (`gobridge_no_mqtt`) | Inverts the safety default — forgetting a tag would silently ship *more* SDKs, not fewer. Additive keeps the blank-root guarantee. |
| `init()`-appended registration funcs (one file per family, no stubs) | Halves the file count but moves registration into `init()` ordering, which the registration contract deliberately avoids; stubs keep call sites visible in `run()`. |
| Seed path derives its stores from the Supervisor (`Supervisor.StoreFactories()`) | Removes the seed duplication properly but is a `bridge` library change; the per-family seed function keeps this work inside the composition root. Revisit if a third Builder-based one-shot appears. |
| Runtime `plugin` package | CGO, platform limits, defeats static distroless images. |

## Risks

- **pluginsym rewrite is the load-bearing change.** If it lands after the file
  split, the gate is vacuous in between — so it lands *first*, proven against
  fixture files for: tagged file, stub file, asymmetric file, duplicated
  adapter, malformed constraint. R5 and its fixture land with the split.
- **Behaviour change for anyone running `go build ./cmd/gobridge` from the
  docs.** `docs/container-deployment.md`, `docs/deployment-guide.md` and the
  Kubernetes profile describe the MQTT + native set; they must say
  `-tags gobridge_mqtt,gobridge_native` (or `make build-gobridge
  GOBRIDGE_TAGS=…`) and `docs/release-notes.md` must call the blank default
  out as breaking for local builds. The shipped image is unaffected (§5).
- `golangci-lint` with `build-tags: [gobridge_all]` no longer lints stub
  files. Mitigation: stubs contain no logic; `go vet` runs both tagged and
  untagged in CI.
- go.mod growth makes `go mod tidy` in `cmd/gobridge` depend on all adapter
  tags — `go mod tidy` already considers all build configurations, so this is
  correct by construction, just heavier.

## Acceptance

- `go -C cmd/gobridge build ./...` (no tags) → blank root: `go version -m
  gobridge.out` lists **no** `adapters/` module other than
  `adapters/native/config/file` and `adapters/native/credentials/file`; no
  paho, no sqlite, no aws/azure/amqp/otel. `-version` prints `families=[]`;
  the startup log warns that nothing is linked.
- `go -C cmd/gobridge build -tags gobridge_mqtt,gobridge_native ./...` →
  today's plugin set; `go version -m` shows paho + native store and nothing
  else from `adapters/` beyond the skeleton.
- `go -C cmd/gobridge build -tags gobridge_all ./...` → all kinds decodable;
  startup log lists them.
- `make lint` green, including reworked `pluginsym` on the split files with
  R5 active.
- `make test` green untagged and with `gobridge_all`; new tests cover
  per-family registration, the blank root (no kinds, `families=[]`), and the
  `-version` families output.
- A yaml naming `type: mqtt` fails with the existing unknown-kind error on
  the blank root and builds on a `gobridge_mqtt` binary; likewise `type: sqs`
  on `gobridge_aws` (pinned by tests).
