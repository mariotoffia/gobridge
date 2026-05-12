# Development Guide

This guide covers everything you need to set up a development environment, build, test, and contribute to gobridge.

## Prerequisites

- **Go 1.25+** -- gobridge uses a Go workspace (`go.work`)
- **Docker** -- required for integration tests (DynamoDB Local, ElasticMQ, Mosquitto, Azure Service Bus emulator)
- **Make** -- optional, provides convenient commands
- **Dev tools** -- run `make install` to install all required tools

## Repository Structure

gobridge is a multi-module Go workspace. Each adapter and processor is a separate Go module so consumers only import what they need. The workspace root `go.work` ties them together for development.

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
│   ├── ddblocal/           # DynamoDB Local
│   ├── sqslocal/           # ElasticMQ (SQS-compatible)
│   ├── asblocal/           # Azure Service Bus emulator
│   ├── s3local/            # MinIO (S3-compatible)
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
| `SQS_ENDPOINT` | (auto-start ElasticMQ) | `testutil/sqslocal` |
| `MQTT_BROKER_URL` | (auto-start Mosquitto) | MQTT integration tests |
| `ASB_CONNECTION_STRING` | (auto-start ASB emulator) | `testutil/asblocal` |
| `S3_ENDPOINT` | (auto-start MinIO) | `testutil/s3local` |

When an environment variable is set, the test utility uses the existing service instead of starting a container.

## Linting

```bash
make lint              # Lint all workspace modules (gofmt + go vet + golangci-lint + go-arch-lint)
make lint-fix          # Lint with auto-fix
make lint-go           # golangci-lint pass only (uses .golangci.yml at the repo root)
make lint-arch         # Strict architecture dependency lint (blocking)
make lint-arch-report  # Same checks, non-blocking; writes reports/go-arch-lint.log
```

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

### Architecture-quality reports (advisory)

```bash
make arch-quality      # Run all advisory reports (writes to reports/)
make arch-graph        # Module dep graph as text (`go mod graph` output)
make dupl-report       # Find duplicate code blocks (advisory)
make goconst-report    # Find repeated literals (advisory)
make lint-acl          # ACL boundary check on adapters (advisory)
```

These are **review aids, not gates** — false positives are common
(test fixtures, repeated HTTP method strings, similar boilerplate).
Forcing them to pass would push contributors toward over-abstraction.
Treat them as prompts for human review:

- `dupl.log` — when the same logic appears in two packages, ask
  whether a missing aggregate root or domain service is hiding.
- `goconst.log` — when the same literal appears 4+ times, ask whether
  it deserves a domain-meaningful constant (named from the ubiquitous
  language, not just a generic helper).
- `arch-graph.txt` — raw `go mod graph` edges, one per line. Grep
  for `^github.com/mariotoffia/gobridge ` to see direct deps; diff
  the file across PRs to spot unintended new module edges (e.g., a
  transport adapter starting to depend on a store implementation).
  Plain text so any tool, including LLM agents, can inspect it.
- `aclcheck.log` — flags adapter files that import a vendor SDK but
  are not named `acl_*.go` (or under `acl/`). The DDD intent is to
  concentrate the SDK boundary in named files. Existing adapters are
  not refactored to satisfy it; treat the report as a prompt for new
  adapters and gradual cleanup.

Run them at release cuts, before adding a new transport/store
adapter, or when investigating a smell that lint cannot pinpoint.

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

## CI Workflow

```bash
make check       # build + golangci-lint + lint-arch-check + unit tests (no Docker)
make check-all   # build + golangci-lint + lint-arch-check + all tests (Docker required)
```

The `lint-arch-check` step runs both `make lint-arch` (strict
architecture component-import lint) and `make lint-arch-mapping-test`
(regression test that verifies `.go-arch-lint.yml` still maps every
sentinel package to its expected role-based component). Both have to
succeed for the build to pass.

`.github/workflows/ci.yml` runs `lint-arch` as a separate job so
architectural failures are easy to spot in PR status checks.

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

These rules are enforced by `make lint-arch`. The full ruleset lives in
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
