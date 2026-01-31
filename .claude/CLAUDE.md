# GoBridge - Claude Code Guidelines

This file provides guidance for Claude Code when working with the gobridge project.

## Project Overview

**GoBridge** is a message bridge framework for connecting different transport technologies. It routes messages between MQTT, AWS SQS, Azure Service Bus, and other transports with middleware support for transformation, filtering, and retry handling.

### Key Characteristics

- **Multi-module Go workspace**: Core module has zero external dependencies; SDK dependencies are in separate modules
- **Go version**: 1.24+
- **Interface-first design**: All core abstractions defined in `bridge/types/`
- **Pluggable architecture**: Easy to add new transports and middlewares

## Project Structure

```
gobridge/
├── bridge/                    # Core abstractions
│   ├── core/                  # Runtime: Bridge, Pipeline, Route
│   ├── credentials/           # Credential resolution and builders
│   ├── logging/               # Logging infrastructure
│   ├── middleware/transport/  # Transport-level middleware (logging, correlation)
│   ├── registry/              # Connection registry
│   └── types/                 # All interfaces and type definitions
├── config/                    # Configuration sources (DynamoDB, file)
├── credentials/               # Credential repositories (AWS PMS, file)
├── middleware/                # Business middleware (filter, transform, retry)
├── metrics/                   # Metrics exporters (CloudWatch, OpenTelemetry)
├── transport/                 # Transport implementations (separate modules)
│   ├── mqtt/                  # MQTT v5 transport (go.mod)
│   ├── aws/                   # AWS SQS transport (go.mod)
│   └── azure/                 # Azure Service Bus transport (go.mod)
└── tests/                     # Integration test utilities
```

## Build & Test Commands

```bash
# Build all modules
make build

# Run unit tests (with race detection)
make test
# Or: go test -v -race ./...

# Run integration tests (requires Docker)
go test -v -tags=integration -timeout=300s ./tests/docker/...

# Lint all modules
make lint

# Tidy dependencies
make tidy

# Update all dependencies
make update

# Full CI check locally
make check

# Docker test containers
make docker-up    # Start Mosquitto, LocalStack
make docker-down  # Stop containers
```

## Coding Conventions

### Go Style

- Follow standard Go idioms and effective Go guidelines
- Use descriptive variable names; avoid single-letter names except for loop indices
- Error handling: Always check errors; wrap with context using `fmt.Errorf("context: %w", err)`
- Use structured errors with `types.BridgeError` for transport operations

### Package Organization

- **Types in `bridge/types/`**: All interfaces and shared types live here
- **Implementations in separate packages**: `core/`, `transport/mqtt/`, etc.
- **Option pattern**: Use functional options (`WithXxx(value)`) for configuration
- **Factory pattern**: Transports use factory registration for pluggability

### Interface Implementation

Verify interface satisfaction at compile time:

```go
var (
    _ types.Source = (*MySource)(nil)
    _ types.Target = (*MyTarget)(nil)
)
```

### Error Handling

Use the two-tier error system:

1. **BridgeError for transports**: All `Target.Send()` and connection operations must return `*types.BridgeError`
2. **Recoverable vs Permanent**: Set `IsRecoverable` appropriately

```go
// Wrap infrastructure errors (recoverable)
return types.ErrConnectionLost.Wrap(err)

// Wrap application errors (permanent)
return types.ErrInvalidPayload.With("topic", topic).Wrap(err)
```

### Logging

Use the structured logging pattern with `types.LogCreator`:

```go
if b.Log != nil {
    b.Log(ctx, types.LogLevelInfo).
        Str("id", id).
        Int("count", count).
        Msg("operation completed")
}
```

## Testing Conventions

### Test File Organization

- Unit tests: `*_test.go` in the same package
- Internal tests: `*_internal_test.go` for whitebox testing
- External tests: `tests/*_test.go` for integration tests
- Test indices: `_test_index.md` files document test coverage

### Test Patterns

```go
// Use testify for assertions
import "github.com/stretchr/testify/assert"
import "github.com/stretchr/testify/require"

// Table-driven tests
func TestSomething(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        expected string
    }{
        {"case1", "input", "output"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := Function(tt.input)
            assert.Equal(t, tt.expected, result)
        })
    }
}
```

### Test Descriptions

Use block comments at the top of test files:

```go
// ═══════════════════════════════════════════════════════════════════════════
// Component Unit Tests
//
// Tests for the component covering:
// - Feature A
// - Feature B
// ═══════════════════════════════════════════════════════════════════════════
```

## Architecture Concepts

### Two Retry Systems

1. **Transport Retry** (`TransportRetryConfig`): For infrastructure failures (DNS, connection)
   - Location: `Target.Send()`, `Connection.Start()`
   - Limit: Message TTL
   - Uses adaptive backoff

2. **Message Retry** (`RetryPolicy`): For application failures (transform, validation)
   - Location: `RetryManager`, middleware
   - Limit: MaxAttempts
   - Uses exponential backoff

### Flow Control

Pipelines implement backpressure:
- `MaxInFlight`: Limits concurrent messages
- `DefaultMessageTTL`: Expiration for messages without explicit TTL

### Shared Connections

MQTT and similar transports support shared connections:
- Multiple sources/targets on one connection
- Use `LifecycleCoordinator` for atomic subscription changes

## Important Files to Know

| File | Purpose |
|------|---------|
| `bridge/types/ARCHITECTURE.md` | System design overview |
| `bridge/types/ARCHITECTURE-MIDDLEWARE.md` | Middleware and retry systems |
| `bridge/types/ARCHITECTURE-TRANSPORTS.md` | Transport implementation guide |
| `bridge/types/MISSING.md` | Roadmap for production readiness |
| `apis/README.md` | HTTP API documentation |

## Multi-Module Workflow

When working with transport modules:

```bash
# Work in specific module
cd transport/mqtt
go build ./...
go test -v ./...

# Or use workspace from root
go build ./...  # Builds all modules via go.work
```

## CI/CD Notes

- CI uses `dorny/paths-filter` to detect which modules changed
- Each module is tested independently
- Integration tests run after core module passes
- Coverage uploaded to Codecov with module-specific flags
