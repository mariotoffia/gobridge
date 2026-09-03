# gobridge

A message-bridge framework for Go. Route messages between MQTT, AWS SQS, Azure Service Bus, RabbitMQ (AMQP 0-9-1), AMQP 1.0 brokers, and other transports with pluggable processors, durable outbox delivery, dead-letter queue management, and observability.

## Capabilities

Every capability is an abstract **port** in the core; the technologies are
**adapters** you opt into. The core itself has no external dependencies — you
import only the adapters you use.

| Capability | What it does | Implementations |
|---|---|---|
| **Transport** | Receive and send messages | MQTT v5 · AWS SQS · Azure Service Bus · RabbitMQ (AMQP 0-9-1) · AMQP 1.0 (Artemis, Solace, Qpid) · HTTP (POST in, SSE out) |
| **Delivery guarantee** | How hard it tries not to lose a message | `DirectHold` (send-then-ack) · `SharedOutbox` (persist-then-ack, durable drainer) |
| **Outbox store** | Durable hand-off so a crash mid-delivery loses nothing | Memory · SQLite · DynamoDB |
| **Dead-letter store** | Quarantines undeliverable messages instead of blocking or dropping them | Memory · SQLite · DynamoDB |
| **Lease store** | Elects one owner per stream so replicas never double-deliver | Memory · DynamoDB |
| **Clustering** | Run several replicas: exactly-once ownership, automatic failover, coordinated config rollout | Lease-backed (DynamoDB for multi-node; memory for single-process) |
| **Rollout store** | Moves the whole cluster to a new config together, or not at all | Memory · DynamoDB |
| **Managed subscriptions** | Tracks which subscriptions a node owns across restarts | SQLite · DynamoDB |
| **Processor chain** | Acts on messages in flight | Filter · Transform · Circuit breaker · Tenant isolation |
| **Credentials** | Resolves secrets and TLS material at run time, with rotation | `file://` · `pms://` (AWS Parameter Store) |
| **TLS / mTLS** | Server-cert validation and client certificates, from files, inline PEM, or a credential store | MQTT · AMQP 0-9-1 · AMQP 1.0 · Service Bus |
| **Config source** | Where the bridge reads its configuration | File · DynamoDB (layered, hot-reloadable) |
| **HTTP APIs** | Operate a running bridge | Admin (lifecycle, route injection, DLQ) · Monitor (health, topology) |
| **Observability** | Tells you what moved and what is stuck | OpenTelemetry metrics + tracing · CloudWatch metrics · correlation-aware `slog` |

Full documentation: **<https://mariotoffia.github.io/gobridge/>**

## Quick Start

### Production (container image / composition root)

The shipped **production** image is **`ghcr.io/mariotoffia/gobridge`**, the
AWS file-based composition root
`deployment/aws-filebased-config/lib/cmd/gobridge-filebased`. Every stable
`cmd/gobridge/vX.Y.Z` release pushes it **by digest** and attaches the verified
digest to that release as `gobridge-image-digest.txt`; the one mutable tag,
`latest`, is promoted from that same scanned digest only when the release is
the highest stable one. Deploy from the digest, never from `latest` — see
[Pin Images by Digest](docs/container-deployment.md#pin-images-by-digest) and
**[RELEASE.md](RELEASE.md#image-publication)**. The image registers the MQTT,
AWS SQS and HTTP transports plus native (memory/SQLite) and DynamoDB stores,
resolves its secrets through SSM, and is the binary the AWS ECS/EFS profile
runs: see the **[Deployment Guide](docs/deployment-guide.md)** and the
**[AWS file-based profile](deployment/aws-filebased-config/README.md)**.

**Kubernetes and other non-AWS platforms** run the maintained
**[Kubernetes profile](deployment/kubernetes/README.md)**: a Dockerfile and one
manifest around the reference binary below (MQTT transport, memory/SQLite
stores, `file://` credentials, HTTP API keys from a Secret), tested end to end
through probes, traffic, reload, SIGTERM and restart. For transports neither
profile bundles (Azure Service Bus, AMQP), build a custom composition root the
same way — the reference binary shows the two wiring sites.

### Reference binary

`cmd/gobridge` is the **reference composition root**: it links MQTT + native
(memory/SQLite) stores + `file://` credentials and nothing else, so a config
naming any other transport or store is rejected at startup. It forwards a
single MQTT topic to another, walked through end to end (YAML config + Go
bootstrap + variations) in
**[Scenario 1: MQTT-to-MQTT Bridge](docs/scenarios/01-mqtt-to-mqtt.md)**.

For richer setups, see the [scenarios index](docs/scenarios/) (durable outbox, clustered exclusive sessions, multi-tenant routing, custom processors, …) or jump straight to the [Configuration Overview](docs/configuration-overview.md).

## Installation

**One version for everything.** GoBridge publishes 31 modules from this
repository, and every one of them carries the *same* version. There is no
compatibility matrix to consult and no per-module changelog to cross-check: pick
a version, use it everywhere, and the pieces are guaranteed to be the set that
was built, tested, and released together.

```bash
go get github.com/mariotoffia/gobridge@v0.3.0
go get github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho@v0.3.0
go get github.com/mariotoffia/gobridge/adapters/aws/transport/sqs@v0.3.0
```

If those three lines look boring, that is the point — mixing `v0.3.0` of the core
with `v0.2.x` of an adapter is not a thing you can accidentally do. Every module
in a release is tagged from one commit train, and the release is only published
if *all* of it passes. See [RELEASE.md](RELEASE.md) for the policy.

The full set:

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
| [Changelog](CHANGELOG.md) | What changed in each release (one entry per version, all modules) |
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
| Memory (DLQ, Outbox) | `adapters/native/store/memory*` | Development and testing |
| Memory (Lease, Rollout) | `adapters/native/memorylease`, `adapters/native/memoryrollout` (root module) | Reference in-memory implementations of the lease/rollout ports; live in the root module because the core's own tests exercise them |
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

To cut a release, see [MODULES.md §3](MODULES.md#3-cut-a-release-make-it-go-get-able):
`make release VERSION=vX.Y.Z` is a dry run, `CONFIRM=1` publishes the whole
train in dependency order.

See [DEVELOPMENT.md](DEVELOPMENT.md) for full setup instructions.

## License

MIT License -- see [LICENSE](LICENSE)
