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
├── runtime/                # Route execution engine
├── bridge/                 # Composition root (Builder)
├── config/                 # Declarative YAML/JSON config model
├── validate/               # Startup config validation
├── httpapi/                # Admin + monitor HTTP servers
├── observability/          # Context helpers, correlation slog handler
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
│   ├── azure/transport/servicebus/
│   ├── native/store/             # Memory + SQLite store factory
│   │   ├── memorylease/
│   │   ├── memoryoutbox/
│   │   ├── memorydlq/
│   │   ├── sqliteoutbox/
│   │   └── sqlitedlq/
│   ├── native/credentials/file/
│   └── otel/
│       ├── metrics/
│       └── tracing/
│
├── processors/             # ports.Processor implementations (separate go.mod each)
│   ├── filter/
│   ├── transform/
│   ├── circuitbreaker/
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

Integration tests require Docker containers. You can either let the test helpers start ephemeral containers automatically, or use persistent containers for faster iteration:

```bash
# Option A: Persistent containers (faster for repeated runs)
make docker-up
DYNAMODB_ENDPOINT=http://127.0.0.1:8000 \
SQS_ENDPOINT=http://127.0.0.1:9324 \
MQTT_BROKER_URL=tcp://127.0.0.1:1883 \
make test-integration

# Option B: Automatic ephemeral containers (slower but zero setup)
make test-integration
```

`make test-integration` runs `go test -race -timeout 600s -v ./...` with dummy AWS credentials set for the SDK.

### Stop and Clean Docker

```bash
make docker-down   # Stop persistent containers
make docker-clean  # Remove ALL orphaned gobridge-* containers and networks
```

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
make lint         # Lint all workspace modules
make lint-fix     # Lint with auto-fix
```

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
make check       # build + lint + unit tests (no Docker)
make check-all   # build + lint + all tests (Docker required)
```

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

- **domain/** must NOT import from any other gobridge package
- **ports/** imports domain/ only
- **runtime/** imports domain/, ports/, observability/
- **adapters/** import ports/ and domain/ -- never runtime/ or bridge/
- **processors/** import ports/ and domain/ -- never runtime/ or bridge/
- **bridge/** imports config/, ports/, runtime/ -- it is the composition root

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
