# Testing Guide

This guide covers the testing patterns, utilities, and workflows used in gobridge.

## Overview

gobridge uses three levels of testing:

1. **Unit tests** -- fast, no external dependencies, run with `make test`
2. **Conformance tests** -- shared test suites that verify store implementations against port contracts
3. **Integration tests** -- require Docker containers, run with `make test-integration`

All tests use the standard `testing` package. `stretchr/testify` (assert/require) is available in the root module but optional -- many packages use plain `t.Fatalf`/`t.Errorf`.

There are **no build tags** for integration tests. Instead, integration tests are gated by `testing.Short()` and the test utilities skip automatically when Docker is unavailable.

## Running Tests

```bash
# Unit tests only (skips all Docker-dependent tests)
make test
# Equivalent to: go test -short -race -timeout 120s ./...

# All tests including integration (requires Docker)
make test-integration
# Equivalent to: AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test go test -race -timeout 600s -v ./...

# With persistent containers (faster repeated runs)
make docker-up
DYNAMODB_ENDPOINT=http://127.0.0.1:8000 \
SQS_ENDPOINT=http://127.0.0.1:9324 \
MQTT_BROKER_URL=tcp://127.0.0.1:1883 \
make test-integration

# CI checks
make check       # build + lint + unit tests
make check-all   # build + lint + all tests

# Cleanup orphaned containers
make docker-clean
```

## Unit Tests

### Conventions

- Use `t.Run("SubtestName", ...)` for table-driven tests
- Prefer hand-rolled fakes over code-generated mocks
- Use `errors.Is` and `errors.As` for error assertions
- Test packages may be internal (`package foo`) or external (`package foo_test`)

### Hand-Rolled Fakes

The runtime package defines comprehensive fakes in `runtime/fakes_test.go`. These implement `ports.Delivery`, `ports.Receiver`, `ports.Sender`, and store interfaces with controllable behavior:

```go
type FakeDelivery struct {
    env     *domain.Envelope
    acked   bool
    retried bool
    ackErr  error
}

func (d *FakeDelivery) Envelope() *domain.Envelope { return d.env }
func (d *FakeDelivery) Ack(ctx context.Context) error {
    d.acked = true
    return d.ackErr
}
// ... etc
```

Use this pattern in your own tests. Do not use gomock or mockery.

### Processor Test Pattern

Processors use a lightweight test helper pattern:

```go
func TestFilter(t *testing.T) {
    // Simple next function that records whether it was called
    var called bool
    nextOK := func(ctx context.Context, env *domain.Envelope) error {
        called = true
        return nil
    }

    env := &domain.Envelope{
        Subject: "test.topic",
        Headers: map[string]any{"type": "alert"},
    }

    proc, err := filter.New(filter.Config{
        Name:       "test-filter",
        Conditions: []filter.Condition{{Field: "subject", Operator: filter.OpEquals, Value: "test.topic"}},
        Action:     filter.ActionPass,
    })
    if err != nil {
        t.Fatal(err)
    }

    if err := proc.Process(context.Background(), env, nextOK); err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !called {
        t.Fatal("expected next to be called")
    }
}
```

### Domain Test Pattern

Domain tests use the external test package and testify:

```go
package domain_test

import (
    "testing"
    "github.com/stretchr/testify/assert"
    "github.com/mariotoffia/gobridge/domain"
)

func TestEnvelope_IsExpired(t *testing.T) {
    t.Run("not expired", func(t *testing.T) {
        env := &domain.Envelope{ExpiresAt: time.Now().Add(time.Hour)}
        assert.False(t, env.IsExpired())
    })
    t.Run("expired", func(t *testing.T) {
        env := &domain.Envelope{ExpiresAt: time.Now().Add(-time.Hour)}
        assert.True(t, env.IsExpired())
    })
}
```

## Conformance Test Suites

The `ports/storetest/` package provides shared test suites that verify store implementations conform to their port contracts.

### Available Suites

```go
import "github.com/mariotoffia/gobridge/ports/storetest"

// DLQ store conformance
storetest.RunDLQStoreTests(t, store)

// Outbox store conformance
storetest.RunOutboxStoreTests(t, store)

// Lease store conformance (with optional timing configuration)
storetest.RunLeaseStoreTests(t, store, &storetest.LeaseTestOptions{
    WaitForExpiry: 2 * time.Second,
    LeaseTTL:      1 * time.Second,
})
```

### Wiring Into Adapter Tests

Each store adapter includes a `_test.go` that constructs the store and runs the conformance suite:

**Memory store example:**

```go
func TestMemoryDLQStore(t *testing.T) {
    store := memorydlq.NewStore()
    storetest.RunDLQStoreTests(t, store)
}
```

**DynamoDB store example:**

```go
func TestDynamoDBOutboxStore(t *testing.T) {
    client := ddblocal.Client(t)
    store := newTestStore(t, client) // creates table, registers cleanup
    storetest.RunOutboxStoreTests(t, store)
}
```

### What the Suites Test

- **DLQ**: WriteAndList, ListFilterByRouteID, ListFilterByCategory, ListFilterBySince, ListFilterByBefore, ListRespectsLimit, WriteIdempotent, ReplayMarksEntries, PurgeRemovesOld, PurgeSkipsRecent, FullLifecycle
- **Outbox**: PersistAndQuery, ClaimRecords, CompleteRecords, ExpireOldRecords, FencingTokenValidation, QueryPending
- **Lease**: AcquireAndRelease, RenewWithValidToken, RenewWithStaleToken, ConcurrentAcquire, Expiry

## Integration Tests

### Test Utility Pattern

The `testutil/` packages manage Docker containers for local testing. They share a common pattern:

1. **Configure** before first use in `TestMain`
2. **Get endpoint** on first test -- starts container if needed
3. **Shutdown** after all tests complete

```go
func TestMain(m *testing.M) {
    ddblocal.Configure(ddblocal.WithCleanOrphans(true))
    code := m.Run()
    ddblocal.Shutdown()
    os.Exit(code)
}

func TestMyIntegration(t *testing.T) {
    // First call starts the Docker container (or uses env var endpoint)
    client := ddblocal.Client(t)
    // ... use client ...
}
```

### Available Test Utilities

| Package | Docker Image | Environment Variable | What It Provides |
|---------|-------------|---------------------|-----------------|
| `testutil/ddblocal` | `amazon/dynamodb-local` | `DYNAMODB_ENDPOINT` | `Client(t) *dynamodb.Client` |
| `testutil/sqslocal` | `softwaremill/elasticmq-native` | `SQS_ENDPOINT` | `Client(t) *sqs.Client`, `UniqueQueue(t)`, `CreateQueueWithAttrs(t, ...)` |
| `testutil/asblocal` | Azure ASB emulator | `ASB_CONNECTION_STRING` | `ConnectionString(t) string` |
| `testutil/s3local` | `minio/minio` | `S3_ENDPOINT` | `Client(t) *s3.Client` |
| `testutil/tlsgen` | (none -- pure crypto) | -- | `Generate(Options) (*Result, error)`, `MustGenerate(t, Options)` |

### Automatic Skip Behavior

Test utilities automatically skip tests when:
- `testing.Short()` is true (`-short` flag)
- Docker is not available and no environment variable is set

This means `make test` always succeeds without Docker.

### Orphan Cleanup

Test containers use prefixed names (e.g. `gobridge-ddblocal-<uuid>`). The `WithCleanOrphans(true)` option removes any leftover containers from previous runs. `make docker-clean` removes all orphaned gobridge containers.

### End-to-End Tests

The `tests/integration/` directory contains full bridge flow tests:

```go
func TestMain(m *testing.M) {
    ddblocal.Configure(ddblocal.WithCleanOrphans(true))
    sqslocal.Configure(sqslocal.WithCleanOrphans(true))
    // mqttlocal.Configure(...)

    code := m.Run()

    ddblocal.Shutdown()
    sqslocal.Shutdown()
    os.Exit(code)
}

func TestE2E_MQTTToSQS(t *testing.T) {
    // Wire up full bridge: config -> builder -> runtime
    // Send messages via MQTT, verify arrival in SQS
}
```

These tests exercise the complete pipeline: config parsing, builder wiring, runtime startup, message flow, and graceful shutdown.

## Metrics Testing

Use `ports.RecordingExporter` for testing metrics emissions:

```go
func TestMetrics(t *testing.T) {
    rec := &ports.RecordingExporter{}

    // ... run code that emits metrics with rec as the exporter ...

    entries := rec.FindEntries("bridge.delivery.latency")
    if len(entries) == 0 {
        t.Fatal("expected latency metric")
    }
}
```

## Writing New Tests

### Checklist

1. **Unit test**: Does the component work in isolation? Use fakes for dependencies.
2. **Conformance test**: Does the store implementation pass the shared suite?
3. **Error paths**: Are all error conditions tested (transient, permanent, expired)?
4. **Concurrency**: Use `-race` flag (enabled by default in Makefile targets).
5. **Cleanup**: Register cleanup functions with `t.Cleanup()` or defer.

### File Naming

- `foo_test.go` -- unit tests for `foo.go`
- `integration_test.go` -- integration tests requiring Docker
- `fakes_test.go` -- shared test doubles within a package
