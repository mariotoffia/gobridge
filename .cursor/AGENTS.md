# GoBridge - Cursor Agent Guidelines

This file provides guidance for Cursor AI agents when working with the gobridge project.

## Project Overview

**GoBridge** is a message bridge framework for Go that routes messages between transport technologies (MQTT, AWS SQS, Azure Service Bus) with middleware support for transformation, filtering, and retry handling.

### Quick Facts

- **Language**: Go 1.24+
- **Architecture**: Multi-module workspace with zero-dependency core
- **Testing**: `testify` for assertions, table-driven tests
- **Linting**: `golangci-lint`
- **CI**: GitHub Actions with per-module detection

## Essential Commands

```bash
# Build
make build                    # Build all modules
go build ./...                # Using workspace

# Test
make test                     # Unit tests with race detection
go test -v -race ./...        # Manual run

# Lint
make lint                     # Lint all modules

# Tidy
make tidy                     # Tidy all module dependencies
```

## Project Layout

```
bridge/types/     # All interfaces - READ FIRST when understanding the codebase
bridge/core/      # Runtime implementation (Bridge, Pipeline, Route)
transport/*/      # Transport modules (mqtt/, aws/, azure/) - separate go.mod files
middleware/*/     # Business middleware (filter/, transform/, retry/)
```

## Code Patterns

### Creating New Types

All shared types go in `bridge/types/`. Implementation packages import from there.

### Functional Options

Use the options pattern for configuration:

```go
func NewThing(id string, opts ...ThingOption) *Thing {
    t := &Thing{id: id}
    for _, opt := range opts {
        opt(t)
    }
    return t
}

func WithTimeout(d time.Duration) ThingOption {
    return func(t *Thing) { t.timeout = d }
}
```

### Interface Verification

Always verify interface implementation at compile time:

```go
var _ types.Source = (*MySource)(nil)
```

### Error Handling

For transport operations, use `types.BridgeError`:

```go
// Recoverable (will be retried)
return types.ErrConnectionLost.Wrap(err)

// Permanent (goes to DLQ)
return types.ErrInvalidPayload.Wrap(err)
```

### Structured Logging

```go
if log != nil {
    log(ctx, types.LogLevelInfo).
        Str("key", value).
        Msg("message")
}
```

## Testing Patterns

### Standard Test Structure

```go
func TestFeature(t *testing.T) {
    tests := []struct {
        name     string
        input    InputType
        expected OutputType
        wantErr  bool
    }{
        {"valid input", validInput, expectedOutput, false},
        {"invalid input", invalidInput, nil, true},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := Function(tt.input)
            if tt.wantErr {
                require.Error(t, err)
                return
            }
            require.NoError(t, err)
            assert.Equal(t, tt.expected, result)
        })
    }
}
```

### Test File Naming

- `*_test.go` - Standard unit tests
- `*_internal_test.go` - Whitebox tests (same package)
- `tests/docker/*_test.go` - Integration tests (require Docker)

## Architecture Notes

### Two Retry Systems

1. **Transport Retry**: Infrastructure failures, TTL-limited, in `Target.Send()`
2. **Message Retry**: Application failures, attempt-limited, via `RetryManager`

### Key Interfaces (in `bridge/types/`)

- `Source` - Message source (subscriber)
- `Target` - Message destination (publisher)
- `Pipeline` - Source → Middleware → Target flow
- `Connection` - Shared transport connection
- `Middleware` - Message processing chain

## Documentation References

| Document | Location |
|----------|----------|
| Architecture Overview | `bridge/types/ARCHITECTURE.md` |
| Middleware Guide | `bridge/types/ARCHITECTURE-MIDDLEWARE.md` |
| Transport Guide | `bridge/types/ARCHITECTURE-TRANSPORTS.md` |
| HTTP APIs | `apis/README.md` |

## Multi-Module Tips

```bash
# Work in a specific module
cd transport/mqtt && go test ./...

# Update a specific module's deps
cd transport/aws && go get -u ./... && go mod tidy

# Sync the workspace
go work sync
```

## Common Tasks

### Adding a New Middleware

1. Create package in `middleware/newmiddleware/`
2. Implement `types.Middleware` interface
3. Register factory in `MiddlewareRegistry`
4. Add tests following existing patterns

### Adding a New Transport

1. Create module in `transport/newtransport/` with own `go.mod`
2. Implement `Source`, `Target`, and optionally `Connection`
3. Create factory implementing `types.SourceFactory`/`types.TargetFactory`
4. Follow error patterns using `types.BridgeError`
5. Add integration tests in `tests/` subdirectory

### Modifying Core Types

1. Update interface in `bridge/types/`
2. Update implementations in `bridge/core/`
3. Run `make test` to verify all modules compile
4. Update architecture docs if needed
