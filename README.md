# gobridge

A message-bridge framework for Go. Route messages between MQTT, AWS SQS, Azure Service Bus, RabbitMQ (AMQP 0-9-1), AMQP 1.0 brokers, and other transports with pluggable processors, durable outbox delivery, dead-letter queue management, and observability.

## Features

- **Multi-transport routing**: MQTT v5, AWS SQS, Azure Service Bus, RabbitMQ (AMQP 0-9-1), AMQP 1.0 with a clean port/adapter model
- **Delivery guarantees**: DirectHold (send-then-ack) and SharedOutbox (persist-then-ack with durable outbox drainer)
- **Processor chain**: Middleware for filtering, transformation, circuit breaking, and tenant isolation
- **Pluggable stores**: LeaseStore, OutboxStore, DLQStore with Memory and DynamoDB implementations; SQLite for OutboxStore and DLQStore
- **Credential management**: URI-based resolution (file://, pms://) with scheme dispatch and caching
- **HTTP APIs**: Admin server for bridge lifecycle, route injection, and DLQ management; Monitor server for health probes and topology
- **Observability**: OpenTelemetry metrics and tracing, CloudWatch metrics, correlation-aware structured logging via slog
- **Zero-dependency core**: The root module has no external dependencies -- only import the adapters you need

## Quick Start

### Production (container image / composition root)

The shipped **production** binary is the file-based composition root
`deployment/aws-filebased-config/lib/cmd/gobridge-filebased`, published as the
container image **`ghcr.io/mariotoffia/gobridge`**. It registers the MQTT, AWS
SQS and HTTP transports plus native (memory/SQLite) and DynamoDB stores, and is
the binary the AWS ECS/EFS deployment profile runs. Start here for any real
deployment: see the **[Deployment Guide](docs/deployment-guide.md)** and the
**[AWS file-based profile](deployment/aws-filebased-config/README.md)**. For
transports it does not bundle (Azure Service Bus, AMQP), build a custom
composition root the same way — the demo binary below shows the two wiring
sites.

### Local / demo

`cmd/gobridge` is a **DEMO / reference** binary — it links **only** MQTT +
native (memory/SQLite) stores and is **not** for production (a config using any
other transport/store is rejected at startup). It forwards a single MQTT topic
to another, walked through end to end (YAML config + Go bootstrap + variations)
in **[Scenario 1: MQTT-to-MQTT Bridge](docs/scenarios/01-mqtt-to-mqtt.md)**.

For richer setups, see the [scenarios index](docs/scenarios/) (durable outbox, clustered exclusive sessions, multi-tenant routing, custom processors, …) or jump straight to the [Configuration Overview](docs/configuration-overview.md).

## Installation

> **⚠️ External `go get` requires released, path-prefixed submodule tags — a
> prerequisite release step that is not yet performed.** This repository is a
> Go **multi-module workspace**: the submodules below depend on sibling modules
> via in-repo `replace` directives and `v0.0.0` requirements that only resolve
> inside this checkout (`go.work`). Go **ignores `replace` directives in
> dependencies**, so a clean external `go get` of a submodule currently fails to
> resolve those siblings (`unknown revision … v0.0.0`). Until every submodule is
> published with a **path-prefixed semver tag** and its internal `v0.0.0`/zero
> pseudo-version requirements are replaced with the released versions, consume
> these modules from an **in-repo workspace** (clone this repo and work within
> `go.work`), not via `go get`. See the release checklist in
> **[RELEASE.md](RELEASE.md)** (published module set, path-prefixed tag policy,
> and the first-release migration that removes `replace` directives).

Once the submodules are published (see the release checklist), the modules are consumed with:

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
| [Domain Model (DDD)](DDD.md) | Bounded contexts, aggregates, invariants, context map |
| [Ubiquitous Language](UBIQUITOUS.md) | Authoritative glossary of terms used in code, config, logs, and docs |
| [Development](DEVELOPMENT.md) | Prerequisites, workspace setup, building, testing, CI |
| [Plugins](PLUGIN.md) | How to write transport, store, credential, and processor plugins |
| [Testing](TESTS.md) | Rules for writing non-flaky, architecturally-correct unit, integration, and long-running tests |

### Guides

| Guide | Description |
|-------|-------------|
| [Configuration Overview](docs/configuration-overview.md) | Configuration lifecycle, sources, layered config, dynamic reconfiguration |
| [Configuration Reference](docs/configuration-reference.md) | Field-by-field reference for `BridgeConfig` |
| [Transport Configuration](docs/transport-configuration.md) | MQTT, SQS, Azure Service Bus, RabbitMQ, AMQP 1.0, HTTP transport options |
| [Processors and Stores](docs/processors-and-stores.md) | Processor chain (filter, transform, circuit breaker, tenant) and store backends |
| [Credentials and HTTP API](docs/credentials-and-http-api.md) | URI-based credential resolution and Admin/Monitor HTTP API |
| [Scenarios](docs/scenarios/) | Progressive walkthroughs from basic MQTT forwarding to cross-protocol AMQP bridging |

### Machine-readable specs

| Spec | Description |
|------|-------------|
| [`spec/httpapi/http-api.yaml`](spec/httpapi/http-api.yaml) | OpenAPI 3.x contract for the Admin and Monitor HTTP servers |
| [`spec/httpapi/components.yaml`](spec/httpapi/components.yaml) | Shared OpenAPI component schemas for the bridge HTTP surface |
| [`spec/httpapi/config-components.yaml`](spec/httpapi/config-components.yaml) | OpenAPI schema for the `BridgeConfig` blueprint exposed via the Admin API |

## Project Structure

```
gobridge/
├── domain/           Core value types (Envelope, RoutePolicy, errors)
├── ports/            Port interfaces (Receiver, Sender, stores, Processor)
├── runtime/          Route execution engine (Runtime, RouteRunner)
├── bridge/           Composition root (Builder wires config to runtime)
├── config/           Inner-ring shared kernel: validate, merge, Manager (stdlib-only)
│   └── parser/       Wire-format adapter: YAML/JSON parser, FileStore (yaml.v3, mapstructure)
├── validate/         Cross-cutting blueprint validation (used by config + admin API)
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
| SQLite | `adapters/native/store/sqlite*` | Single-process deployments (OutboxStore, DLQStore only) |
| DynamoDB | `adapters/aws/store/dynamodb*` | Production, clustered deployments |

## Development

```bash
make install          # Install all dev tools (golangci-lint, govulncheck, gomajor, etc.)
make build            # Build all modules
make test             # Unit tests (no Docker)
make test-integration # All tests (Docker required)
make lint             # Lint all modules
make tidy             # Sync workspace + tidy all module deps
make update           # Upgrade deps to latest minor/patch
make update-major     # Show available major version upgrades
make vulncheck        # Scan for known vulnerabilities
make check            # Build + lint + unit tests
make verify-release-preparation # Source-safe release DAG/tooling preflight
```

The release-strict `make verify-published-modules RELEASE_VERSION=vX.Y.Z` gate
is intentionally expected to fail until the staged first-release migration and
matching path-prefixed tags described in [RELEASE.md](RELEASE.md) exist.

See [DEVELOPMENT.md](DEVELOPMENT.md) for full setup instructions.

## License

MIT License -- see [LICENSE](LICENSE)
