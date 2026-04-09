# gobridge

A message-bridge framework for Go. Route messages between MQTT, AWS SQS, Azure Service Bus, RabbitMQ (AMQP 0-9-1), AMQP 1.0 brokers, and other transports with pluggable processors, durable outbox delivery, dead-letter queue management, and observability.

## Features

- **Multi-transport routing**: MQTT v5, AWS SQS, Azure Service Bus, RabbitMQ (AMQP 0-9-1), AMQP 1.0 with a clean port/adapter model
- **Delivery guarantees**: DirectHold (send-then-ack) and SharedOutbox (persist-then-ack with durable outbox drainer)
- **Processor chain**: Middleware for filtering, transformation, circuit breaking, and tenant isolation
- **Pluggable stores**: LeaseStore, OutboxStore, DLQStore with Memory, SQLite, and DynamoDB implementations
- **Credential management**: URI-based resolution (file://, pms://) with scheme dispatch and caching
- **HTTP APIs**: Admin server for bridge lifecycle, route injection, and DLQ management; Monitor server for health probes and topology
- **Observability**: OpenTelemetry metrics and tracing, CloudWatch metrics, correlation-aware structured logging via slog
- **Zero-dependency core**: The root module has no external dependencies -- only import the adapters you need

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

# RabbitMQ (AMQP 0-9-1) transport adapter
go get github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091

# AMQP 1.0 transport adapter (Artemis, Solace, Qpid, etc.)
go get github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10

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

### Guides

| Guide | Description |
|-------|-------------|
| [Configuration Overview](docs/configuration-overview.md) | Configuration lifecycle, sources, layered config, dynamic reconfiguration |
| [Configuration Reference](docs/configuration-reference.md) | Field-by-field reference for `BridgeConfig` |
| [Transport Configuration](docs/transport-configuration.md) | MQTT, SQS, Azure Service Bus, RabbitMQ, AMQP 1.0, HTTP transport options |
| [Processors and Stores](docs/processors-and-stores.md) | Processor chain (filter, transform, circuit breaker, tenant) and store backends |
| [Credentials and HTTP API](docs/credentials-and-http-api.md) | URI-based credential resolution and Admin/Monitor HTTP API |
| [Scenarios](docs/scenarios/) | Progressive walkthroughs from basic MQTT forwarding to cross-protocol AMQP bridging |

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
│   ├── aws/          SQS, DynamoDB stores, SSM credentials, CloudWatch, ECS cluster
│   ├── amqp/         RabbitMQ (AMQP 0-9-1) and AMQP 1.0 (Artemis, Solace, Qpid)
│   ├── azure/        Azure Service Bus
│   ├── http/         HTTP POST ingress, SSE egress
│   ├── native/       Memory and SQLite stores, file credentials, file config
│   └── otel/         OpenTelemetry metrics and tracing
├── processors/       Filter, transform, circuit breaker, tenant
├── cmd/gobridge/     Example binary
└── testutil/         Docker test helpers (DynamoDB, SQS, ASB, RabbitMQ, Artemis, S3)
```

## Transports

| Transport | Module | Features |
|-----------|--------|----------|
| MQTT v5 | `adapters/mqtt/transport/paho` | Shared sessions, QoS 0/1/2, topic wildcards, autopaho reconnect |
| AWS SQS | `adapters/aws/transport/sqs` | Long polling, batch send, visibility extension, FIFO support |
| Azure Service Bus | `adapters/azure/transport/servicebus` | Queues, topics/subscriptions, batch send, auto-extend lock |
| RabbitMQ (AMQP 0-9-1) | `adapters/amqp/transport/amqp091` | Exchanges, queues, bindings, publisher confirms, prefetch, reconnect |
| AMQP 1.0 | `adapters/amqp/transport/amqp10` | Artemis/Solace/Qpid, link credit flow, reconnect, settlement mapping |
| HTTP | `adapters/http/transport` (root module) | POST ingress, SSE egress, path-based routing |

## Stores

| Store | Module | Use Case |
|-------|--------|----------|
| Memory | `adapters/native/store/memory*` | Development and testing |
| SQLite | `adapters/native/store/sqlite*` | Single-process deployments |
| DynamoDB | `adapters/aws/store/dynamodb*` | Production, clustered deployments |

## Development

```bash
make install          # Install all dev tools (golangci-lint, govulncheck, gomajor, etc.)
make build            # Build all modules
make test             # Unit tests (no Docker)
make test-integration # All tests (Docker required)
make docker-up        # Start persistent test containers
make lint             # Lint all modules
make tidy             # Sync workspace + tidy all module deps
make update           # Upgrade deps to latest minor/patch
make update-major     # Show available major version upgrades
make vulncheck        # Scan for known vulnerabilities
make check            # Build + lint + unit tests
```

See [DEVELOPMENT.md](DEVELOPMENT.md) for full setup instructions.

## License

MIT License -- see [LICENSE](LICENSE)
