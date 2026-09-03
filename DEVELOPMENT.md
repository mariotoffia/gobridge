# Development Guide

This guide covers everything you need to set up a development environment, build, test, and contribute to gobridge.

## Prerequisites

- **Go 1.25+** -- gobridge uses a Go workspace (`go.work`)
- **Docker** -- required for integration tests (Floci, DynamoDB Local, Mosquitto, Azure Service Bus emulator)
- **Make** -- optional, provides convenient commands
- **Dev tools** -- run `make install` to install all required tools

## Repository Structure

gobridge is a multi-module Go workspace. Each adapter and processor is a separate Go module so consumers only import what they need. The workspace root `go.work` ties them together for development.

For the everyday workflow — bootstrapping the workspace, adding a module, and cutting a
`go get`-able release — see [MODULES.md](MODULES.md); it is the simple front door and
links here and to [RELEASE.md](RELEASE.md) for depth.

```
gobridge/
├── go.work                 # Workspace definition
├── go.mod                  # Root module (domain, ports, runtime, bridge, config, ...)
├── Makefile                # Build, test, lint, Docker commands
│
├── domain/                 # Core value types -- innermost hexagonal ring
├── ports/                  # Port interfaces -- driven (secondary) ports
│   └── storetest/          # Conformance test suites for store implementations
├── circuitbreaker/         # Standalone circuit breaker state machine
├── logging/                # Trace and debug log level utilities
├── observability/          # Context helpers, correlation slog handler
├── config/                 # Declarative YAML/JSON config model
├── validate/               # Startup config validation
├── runtime/                # Route execution engine (orchestration)
│   ├── dlq/                # Dead-letter-queue router
│   ├── cluster/            # Route-ownership locator
│   ├── session/            # Lease lifecycle + step-down
│   ├── outbox/             # Shared-outbox Drainer + DepthCache
│   ├── route/              # Per-route ingress pipeline + dispatch
│   └── credentials/        # Pull→Push credential wrapper (used by bridge)
├── bridge/                 # Composition root (Builder)
├── httpapi/                # Admin + monitor HTTP servers
│
├── adapters/               # Port implementations (separate go.mod per adapter)
│   ├── mqtt/transport/paho/
│   ├── aws/transport/sqs/
│   ├── aws/store/                # DynamoDB store factory
│   │   ├── dynamodblease/
│   │   ├── dynamodboutbox/
│   │   └── dynamodbdlq/
│   ├── aws/credentials/ssm/
│   ├── aws/metrics/cloudwatch/
│   ├── aws/config/dynamodb/
│   ├── aws/cluster/ecs/          # ECS cluster resolver
│   ├── azure/transport/servicebus/
│   ├── amqp/transport/amqp091/   # RabbitMQ (AMQP 0-9-1)
│   ├── amqp/transport/amqp10/    # AMQP 1.0 (Artemis, Solace, Qpid)
│   ├── http/transport/            # HTTP POST ingress, SSE egress
│   ├── native/store/             # Memory + SQLite store factory
│   │   ├── memorylease/
│   │   ├── memoryoutbox/
│   │   ├── memorydlq/
│   │   ├── sqliteoutbox/
│   │   └── sqlitedlq/
│   ├── native/credentials/file/
│   ├── native/config/file/       # File config loader/watcher
│   ├── native/cluster/           # Native cluster resolver
│   └── otel/
│       ├── metrics/
│       └── tracing/
│
├── processors/             # ports.Processor implementations (separate go.mod each)
│   ├── filter/
│   ├── transform/
│   ├── circuitbreaker/     # Processor wrapper (uses root circuitbreaker/)
│   └── tenant/
│
├── cmd/gobridge/           # Example binary
├── testutil/               # Docker test helpers
│   ├── flocilocal/         # Floci -- every AWS API except DynamoDB
│   ├── ddblocal/           # DynamoDB Local
│   ├── asblocal/           # Azure Service Bus emulator
│   └── tlsgen/             # TLS certificate generator (pure crypto, no Docker)
└── tests/integration/      # End-to-end integration tests
```

## Getting Started

### Clone and sync

```bash
git clone https://github.com/mariotoffia/gobridge.git
cd gobridge
go work sync
make tidy
make hooks-install
```

### Build

```bash
# Build all workspace modules
make build

# Or directly
go build ./...
```

### Run Unit Tests

```bash
# Fast: unit tests only, integration tests skipped via -short
make test
```

This runs `go test -short -race -timeout 120s ./...`. The `-short` flag causes all Docker-dependent tests to skip automatically.

### Run Integration Tests

Integration tests require Docker. Each `testutil/*local` helper starts an
ephemeral container the first time it is called from a `TestMain`, reuses
it for the rest of the run, and shuts it down on teardown. Set the
matching environment variable (see the table below) to point the helpers
at an already-running endpoint instead of letting them start their own
container.

```bash
make test-integration
```

`make test-integration` runs `go test -race -timeout 600s -v ./...` with dummy AWS credentials set for the SDK.

## Environment Variables

The test utilities check these environment variables before starting Docker containers:

| Variable | Default | Used By |
|----------|---------|---------|
| `DYNAMODB_ENDPOINT` | (auto-start DynamoDB Local) | `testutil/ddblocal` |
| `FLOCI_ENDPOINT` | (auto-start Floci) | `testutil/flocilocal` |
| `MQTT_BROKER_URL` | (auto-start Mosquitto) | MQTT integration tests |
| `ASB_CONNECTION_STRING` | (auto-start ASB emulator) | `testutil/asblocal` |

When an environment variable is set, the test utility uses the existing service instead of starting a container.

### Local deployment proof

`make test-local-deploy` deploys the `aws-filebased-config` CDK profile against
local emulation and drives the running system. It needs Docker and Node, no AWS
account and no credentials: it builds the runtime image, installs the CDK CLI
and its local wrapper under `.tools/`, stands the emulators on one Docker
network, and reclaims everything — including the containers the emulator
launched — when it finishes.

| Variable | Effect |
|----------|--------|
| `GOBRIDGE_INT_LOCAL=1` | Take the local branch instead of skipping on missing `GOBRIDGE_INT_*`. Set by the Make target. |
| `GOBRIDGE_INT_KEEP=1` | Leave the stack, the containers and the shared config directory in place for a post-mortem. |
| `GOBRIDGE_LOCAL_IMAGE` | Runtime image the slots deploy (default `gobridge-filebased:local`, built by `make docker-build`). |

Two local runs cannot share a machine: the emulator starts an image registry of
its own on a fixed host port.

The suite deploys one stack per topology — the SQS data plane, the MQTT bridge,
the control/worker cluster, the static-slot cohort, and the shape, redeploy,
destroy and dead-letter proofs. What each one proves, what the emulator cannot
back, and the measured reason behind every matrix entry that has no local test
is [docs/aws-deployment/local-deployment-suite.md](docs/aws-deployment/local-deployment-suite.md).

## Linting

```bash
make lint     # Run every static check; writes one report per checker under reports/
make lint-fix # Auto-format all tracked Go files with gofmt (escape hatch)
```

`make lint` is the single entry point. For the full checker order, report inventory, and failure → fix recipes, see [LINT.md](LINT.md).

Lint is fail-fast: the first failing checker stops the build. Read the latest `reports/<tool>.log` when something is red; older logs are from the previous successful run.

Architecture lint enforces three layers of rules from `.go-arch-lint.yml`:

1. **Component imports** — direct package-to-package dependencies must
   match each component's `mayDependOn` list.
2. **Vendor imports** — each component must explicitly opt into the
   external dependencies it uses (`canUse:` block). The inner ring
   (`domain`, `ports`, `runtime`, `bridge`, `logging`, `observability`)
   is stdlib-only; `config` is the only inner-ring package allowed a
   single vendor (`gopkg.in/yaml.v3`).
3. **Method calls and dependency injections** (deep scan) — catches
   structural-typing leaks where a type from one component flows into
   another even without a direct import.

Adapters are split into role-specific components (one per transport
technology, one per store backend, one per config-source backend, etc.)
so cross-adapter coupling fails lint as soon as it is introduced.

### Advisory report sub-targets

`make lint` ends with three advisory stages (module graph, duplicate scan, repeated literals) that never fail the build. The same three reports are also produced by these standalone targets for ad-hoc invocation:

```bash
make arch-graph     # Workspace module dep graph (`go mod graph` → reports/arch-graph.txt)
make dupl-report    # Find duplicate code blocks (reports/dupl.log)
make goconst-report # Find repeated literals (reports/goconst.log)
```

See [LINT.md](LINT.md) for what each report prompts (typically a missing aggregate, domain service, or ubiquitous-language constant).

## Dependency Management

```bash
make tidy         # Sync workspace versions + tidy all modules (removes unused, adds missing)
make update       # Upgrade all deps to latest minor/patch versions, then tidy
make update-major # Show available major version upgrades (review before applying)
make outdated     # Pretty-print table of outdated direct dependencies
make vulncheck    # Scan all modules for known vulnerabilities
```

`make update-major` uses [gomajor](https://github.com/icholy/gomajor) to detect dependencies with newer major versions available. Major version upgrades require import path changes in Go, so review the output and apply selectively with `gomajor get <module>`.

`make outdated` uses [go-mod-outdated](https://github.com/psampaz/go-mod-outdated) to display a formatted table of direct dependencies that have newer versions.

`make vulncheck` uses [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) (by the Go team) to check all modules against the Go vulnerability database.

## Module versioning & references

The repo is a multi-module `go.work` workspace. The rules below keep it consumable by external `go get` / `go install`; the release-side procedure (tag order, tag names, CI gate) is in [RELEASE.md](RELEASE.md).

- **`go.work` is the only local override.** Inside the repo, builds and tests resolve sibling modules from disk via the workspace `use` directives. Never add a `replace` directive to a published module's go.mod: `go get` ignores replaces in dependency modules, and `go install pkg@version` refuses modules that contain them.
- **Working against a local clone from another project:** use *your* project's `go.work` (`go work use ../gobridge/...`) or a `replace` in *your* go.mod — main-module replaces always apply and stay on your machine.
- **Inter-module `require`s always name the latest published tag** (during development that is the previous release — the workspace gives you HEAD behavior locally). This keeps `make tidy`, `make update`, `make outdated`, and `make vulncheck` working: those loops run `go mod tidy` / `go list -m` per module, which ignore the workspace and resolve from the module proxy.
- **The workspace can lie:** using a new sibling API at HEAD without bumping the require compiles locally but breaks consumers. `GOWORK=off go build ./...` in the module is the check; CI runs it per published module (see RELEASE.md).
- **Internal-only modules** (`tests/`, `testutil/`, `scripts/`, `deployment/`) are never tagged or published and may keep local `replace` directives.

> **Current state:** published modules still carry `replace` directives and `v0.0.0` requires; the migration steps are in [RELEASE.md — Release procedure](RELEASE.md#release-procedure).

## Base image digests

The root `Dockerfile` pins both base images to a **top-level multi-platform OCI
index digest** (the index, not a per-architecture manifest), so a rebuild pulls
the exact reviewed bytes:

| Stage | Image | Pinned index digest |
|-------|-------|---------------------|
| build | `golang:1.25-bookworm` | `sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58` |
| runtime | `gcr.io/distroless/static-debian12:nonroot` | `sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b` |

Both indexes were verified to include `linux/amd64` and `linux/arm64`.

Refresh and verify a digest before committing it. This needs `docker buildx`
(a documented prerequisite; tested with v0.34.1); `crane` v0.21.7 works too. It
never installs a tool:

```bash
IMG=golang:1.25-bookworm   # or gcr.io/distroless/static-debian12:nonroot

# Top-level index digest to pin:
docker buildx imagetools inspect "$IMG" --format '{{.Manifest.Digest}}'

# Confirm it is a multi-platform index covering amd64 AND arm64:
docker buildx imagetools inspect "$IMG" --raw | \
  jq -r '.manifests[] | select(.platform != null) |
         "\(.platform.os)/\(.platform.architecture)"' | sort -u
```

Update the `FROM image:tag@sha256:<digest>` lines only through a reviewed change,
keeping the human-readable tag alongside the digest. A source rebuild is
reproducible only to the extent these pinned bases, the locked per-module
`go.sum`, and the Go toolchain are fixed. The build claims no bit-for-bit
reproducibility beyond those facts.

The seeder base image (`public.ecr.aws/aws-cli/aws-cli`, pinned to a concrete
`2.x.y` tag) uses the same discipline. `make -C deployment/aws-filebased-config
update-seeder-image` discovers the highest concrete `2.x.y` tag (the upstream
image publishes no floating `2` tag), resolves and verifies its top-level index
(amd64 + arm64), computes the digest from the verified bytes, and rewrites both
`image.txt` and the seeder `Dockerfile`, failing closed on a missing tag, digest,
or platform. It never installs a tool; the tested resolver versions are crane
v0.21.7 or docker buildx v0.34.1 (exact, not floors). Its shell checks (both the
crane and docker paths) run under `make -C deployment/aws-filebased-config test`
(see [TESTS.md](TESTS.md), Deployment shell tests).

## CI Workflow

```bash
make check     # build + lint + test (no Docker)
make check-all # build + lint + test-integration (Docker required)
```

`make lint` runs every static check (architecture + mapping regression
+ gofmt + go vet + golangci-lint + aggcheck + aclcheck + cfgshape +
registrychk + pluginsym) and writes one log per checker under
`reports/`. `make test` runs unit tests plus timing audits
(`audit-timings` for production code, `audit-test-timings` for test
code).

`.github/workflows/ci.yml` runs two jobs: `test` (`make test`) and
`lint` (`make lint`). The lint job uploads `reports/` as an artifact
on every run so architectural and analyzer failures are inspectable
without re-running locally.

## Adding a New Module

When adding a new adapter or processor module:

1. Create the directory and `go.mod`:
   ```bash
   mkdir -p adapters/mycloud/transport/myqueue
   cd adapters/mycloud/transport/myqueue
   go mod init github.com/mariotoffia/gobridge/adapters/mycloud/transport/myqueue
   ```

2. Add to `go.work`:
   ```bash
   # From project root
   go work use ./adapters/mycloud/transport/myqueue
   go work sync
   ```

3. Create a `doc.go` with a package-level comment explaining the adapter's purpose.

4. Run `make tidy` to resolve all module dependencies.

## Code Conventions

### Hexagonal Layer Rules

These rules are enforced by `make lint`. The full ruleset lives in
`.go-arch-lint.yml`. The bounded-context decomposition of `domain/`
and the cross-context dependencies are described in
[DDD.md](DDD.md); ubiquitous-language terms are defined in
[UBIQUITOUS.md](UBIQUITOUS.md).

- **domain/shared, domain/messaging, domain/clock** — stdlib only. No project
  dependencies.
- **domain/persistence** — stdlib + `domain/shared` + `domain/messaging`
  (`OutboxRecord` embeds `messaging.Envelope`).
- **domain/routing** — stdlib + `domain/shared` + `domain/messaging`.
- **domain/connectivity** — stdlib + `domain/shared`.
- **ports/** — every `domain/*` context. No external vendors.
- **config/** — `ports`, every `domain/*` context, plus `gopkg.in/yaml.v3`
  and `github.com/go-viper/mapstructure/v2` (the only inner-ring
  vendors). Wire-format marshalling lives here, not in `ports/`.
- **observability/** — stdlib only.
- **logging/** — stdlib only.
- **circuitbreaker/** — `ports` + every `domain/*` context (the breaker
  satisfies `ports.CircuitBreaker`; adapters depend on the port, not on
  this package).
- **validate/** — `ports` + every `domain/*` context.
- **runtime/** — every `domain/*` context except `domain/events`, plus
  `ports`, `observability`, `logging`, and the six runtime leaves
  (`runtime/dlq`, `runtime/cluster`, `runtime/session`, `runtime/outbox`,
  `runtime/route`; **not** `runtime/credentials`, which is consumed only
  by `bridge`).
- **runtime/dlq/** — `domain/clock`, `domain/shared`, `domain/messaging`,
  `domain/persistence`, `domain/routing`, `ports`.
- **runtime/cluster/** — `domain/clock`, `domain/persistence`, `ports`.
- **runtime/session/** — `domain/clock`, `domain/shared`,
  `domain/persistence`, `domain/routing`, `domain/connectivity`, `ports`,
  `logging`.
- **runtime/outbox/** — `domain/clock`, `domain/shared`,
  `domain/messaging`, `domain/persistence`, `domain/routing`, `ports`,
  `logging`, plus the sibling `runtime/dlq` leaf.
- **runtime/route/** — `domain/clock`, `domain/shared`,
  `domain/messaging`, `domain/persistence`, `domain/routing`, `ports`,
  `logging`, `observability`, plus the sibling `runtime/dlq` and
  `runtime/outbox` leaves.
- **runtime/credentials/** — `domain/clock`, `domain/shared`,
  `domain/connectivity`, `ports` (consumed by `bridge`'s composition
  root only; parent `runtime` never imports it).

  Sub-packages MUST NOT import their parent `runtime/` nor unrelated
  siblings; the parent composes leaves through the inward dependency
  rule. Enforced by `.go-arch-lint.yml` components `runtime_dlq`,
  `runtime_cluster`, `runtime_session`, `runtime_outbox`, `runtime_route`,
  `runtime_credentials`.
- **bridge/** — `ports`, `runtime`, every `domain/*` context, `logging`.
  Never depends on `config` directly (the composition root injects a
  `ports.BlueprintValidator`).
- **transport adapters** — `ports`, every `domain/*` context, `logging`,
  vendor SDK. Never `bridge/`, `config/`, sibling adapters, or
  `circuitbreaker/` (consume `ports.CircuitBreaker` instead).
- **store implementation adapters** — `ports`, every `domain/*` context,
  `logging`, vendor SDK. Never aggregators, never sibling stores.
- **store factory aggregators** (`adapters/native/store`, `adapters/aws/store`)
  — `ports`, every `domain/*` context, `logging`, only their own store
  impls. Never `bridge/` or `config/`.
- **config source adapters** (`adapters/native/config/file`,
  `adapters/aws/config/dynamodb`) — `config/`, every `domain/*` context,
  `ports`, `logging`, vendor SDK. The single adapter category allowed
  to import `config`.
- **credential / observability / cluster adapters** — `ports`, every
  `domain/*` context, `logging`, vendor SDK.
- **processors/** — one component per role; each may depend on `ports`
  and the `domain/*` contexts it actually uses. Never sibling
  processors, never `runtime/`, never `bridge/`. Only
  `processors/circuitbreaker` may import the `circuitbreaker` package
  (because circuit breaking IS its job).
- **httpapi/** — `runtime`, `ports`, every `domain/*` context,
  `observability`, `logging`. The composition root injects a
  `ports.ConfigStore` for admin operations; `httpapi` does not depend
  on the `config` parser package directly.
- **cmd/, deployment/** — composition roots; any project package and
  any vendor.

### Package Documentation

Every package must have a `doc.go` with a `// Package <name> ...` comment. This is the first thing a new developer reads.

### Compile-Time Interface Checks

Every adapter must include compile-time interface verification:

```go
var _ ports.Receiver = (*Receiver)(nil)
var _ ports.Sender = (*Sender)(nil)
```

### Error Types

Use `domain.BridgeError` with appropriate ErrorClass (Transient, Permanent, Expired, Rejected). Map transport-specific errors in adapter `errors.go` files.

### Options Pattern

Use functional options for constructors:

```go
func New(cfg Config, opts ...Option) *MyAdapter
```

### Testing

See [TESTS.md](TESTS.md) for complete testing guidelines.
