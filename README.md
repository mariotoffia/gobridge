# gobridge

A hexagonal message-bridge framework for Go. Route messages between MQTT, AWS SQS, Azure Service Bus, and other transports with pluggable processors, durable outbox delivery, dead-letter queue management, and production-grade observability.

## Features

- **Multi-transport routing**: MQTT v5, AWS SQS, Azure Service Bus with a clean port/adapter model
- **Delivery guarantees**: DirectHold (send-then-ack) and SharedOutbox (persist-then-ack with durable outbox drainer)
- **Processor chain**: Onion-model middleware for filtering, transformation, circuit breaking, and tenant isolation
- **Pluggable stores**: LeaseStore, OutboxStore, DLQStore with Memory, SQLite, and DynamoDB implementations
- **Credential management**: URI-based resolution (file://, pms://) with scheme dispatch and caching
- **HTTP APIs**: Admin server for bridge lifecycle, route injection, and DLQ management; Monitor server for health probes and topology
- **Observability**: OpenTelemetry metrics and tracing, CloudWatch metrics, correlation-aware structured logging via slog
- **Zero-dependency core**: The root module has no external dependencies -- only import the adapters you need
- **Multi-module workspace**: Each adapter is a separate Go module; consumers cherry-pick dependencies

## Quick Start

```go
package main

import (
    "context"
    "log/slog"
    "os"

    "github.com/mariotoffia/gobridge/bridge"
    "github.com/mariotoffia/gobridge/config"
    "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
    nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
)

func main() {
    cfg, err := config.ParseFile("bridge.yaml", config.FormatAuto)
    if err != nil {
        slog.Error("config error", "error", err)
        os.Exit(1)
    }

    ctx := context.Background()
    logger := slog.Default()

    rt, err := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
        RegisterTransport("mqtt", paho.NewBridgeFactory(logger)).
        RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
        Build(ctx)
    if err != nil {
        slog.Error("build error", "error", err)
        os.Exit(1)
    }

    if err := rt.Start(ctx); err != nil {
        slog.Error("start error", "error", err)
        os.Exit(1)
    }
    defer rt.Stop(context.Background())

    slog.Info("bridge running", "instance_id", rt.InstanceID())
    // ... wait for shutdown signal ...
}
```

## Installation

```bash
# Core module (domain, ports, runtime, config, bridge) -- zero external deps
go get github.com/mariotoffia/gobridge

# MQTT transport adapter
go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho

# AWS SQS transport adapter
go get github.com/mariotoffia/gobridge/adapters/aws/transport/sqs

# Azure Service Bus transport adapter
go get github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus

# Native stores (memory, SQLite)
go get github.com/mariotoffia/gobridge/adapters/native/store

# DynamoDB stores
go get github.com/mariotoffia/gobridge/adapters/aws/store
```

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](ARCHITECTURE.md) | System design, hexagonal layers, core concepts, message flow |
| [Development](DEVELOPMENT.md) | Prerequisites, workspace setup, building, testing, CI |
| [Plugins](PLUGIN.md) | How to write transport, store, credential, and processor plugins |
| [Testing](TESTS.md) | Unit tests, conformance suites, integration tests, test utilities |

## Project Structure

```
gobridge/
├── domain/           Core value types (Envelope, RoutePolicy, errors)
├── ports/            Port interfaces (Receiver, Sender, stores, Processor)
├── runtime/          Route execution engine (Runtime, RouteRunner)
├── bridge/           Composition root (Builder wires config to runtime)
├── config/           Declarative YAML/JSON configuration model
├── httpapi/          Admin and monitor HTTP servers
├── observability/    Context helpers and correlation slog handler
├── adapters/
│   ├── mqtt/         MQTT v5 via Paho
│   ├── aws/          SQS, DynamoDB stores, SSM credentials, CloudWatch
│   ├── azure/        Azure Service Bus
│   ├── native/       Memory and SQLite stores, file credentials
│   └── otel/         OpenTelemetry metrics and tracing
├── processors/       Filter, transform, circuit breaker, tenant
├── cmd/gobridge/     Example binary
└── testutil/         Docker test helpers (DynamoDB, SQS, ASB, S3)
```

## Transports

| Transport | Module | Features |
|-----------|--------|----------|
| MQTT v5 | `adapters/mqtt/transport/paho` | Shared sessions, QoS 0/1/2, topic wildcards, autopaho reconnect |
| AWS SQS | `adapters/aws/transport/sqs` | Long polling, batch send, visibility extension, FIFO support |
| Azure Service Bus | `adapters/azure/transport/servicebus` | Queues, topics/subscriptions, batch send, auto-extend lock |

## Stores

| Store | Module | Use Case |
|-------|--------|----------|
| Memory | `adapters/native/store/memory*` | Development and testing |
| SQLite | `adapters/native/store/sqlite*` | Single-process deployments |
| DynamoDB | `adapters/aws/store/dynamodb*` | Production, clustered deployments |

## Building and Testing

```bash
make build            # Build all modules
make test             # Unit tests (no Docker)
make test-integration # All tests (Docker required)
make docker-up        # Start persistent test containers
make lint             # Lint all modules
make check            # Build + lint + unit tests
```

See [DEVELOPMENT.md](DEVELOPMENT.md) for full setup instructions.

## License

MIT License -- see [LICENSE](LICENSE)
