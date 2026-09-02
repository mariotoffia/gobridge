# GoBridge documentation

GoBridge routes messages between systems that do not speak the same protocol —
MQTT, AWS SQS, Azure Service Bus, RabbitMQ (AMQP 0-9-1), AMQP 1.0 and HTTP —
with durable delivery, dead-letter handling, clustering and observability.

New here? Read [Scenario 1: MQTT-to-MQTT](scenarios/01-mqtt-to-mqtt.md) first: it
walks a working bridge end to end. Then skim the
[Configuration Overview](configuration-overview.md).

---

## Start here

| Page | What it covers |
|---|---|
| [Scenario 1: MQTT-to-MQTT](scenarios/01-mqtt-to-mqtt.md) | A complete working bridge, start to finish |
| [Configuration Overview](configuration-overview.md) | How configuration is layered, sourced and reloaded |
| [Deployment Guide](deployment-guide.md) | Running GoBridge for real |
| [Health Checks and Graceful Shutdown](health-and-shutdown.md) | Probes, shutdown sequence and budgets, exit codes |
| [Container and Orchestrator Deployment](container-deployment.md) | Image pinning, probe wiring, building your own image |
| [Troubleshooting](troubleshooting.md) | Symptoms, causes and fixes |

## Configuration

| Page | What it covers |
|---|---|
| [Configuration Overview](configuration-overview.md) | Lifecycle, sources, layering, dynamic reconfiguration |
| [Configuration Reference](configuration-reference.md) | Field-by-field `BridgeConfig` reference |
| [Routes and Runtime Reference](routes-and-runtime-reference.md) | Route shape and runtime behaviour |
| [Programmatic API](programmatic-api.md) | Delivery hooks, the builder, runtime lifecycle |
| [Transport Configuration](transport-configuration.md) | Options common to every transport |
| [Processors and Stores](processors-and-stores.md) | Filter, transform, circuit breaker, tenant; store backends |
| [Config Stores](config-stores.md) | Where configuration is read from and written to |

## Transports

| Transport | Everyday description |
|---|---|
| [MQTT](transports/mqtt.md) | The lightweight protocol devices and sensors use — [options](transports/mqtt-options.md) · [behaviour](transports/mqtt-behavior.md) · [settlement recovery](transports/mqtt-settlement-recovery.md) |
| [AWS SQS](transports/sqs.md) | Amazon's managed queue |
| [Azure Service Bus](transports/servicebus.md) | Microsoft's managed queue and topics |
| [RabbitMQ / AMQP 0-9-1](transports/amqp091.md) | The widely-used in-house message broker |
| [AMQP 1.0](transports/amqp10.md) | Artemis, Solace, Qpid |
| [HTTP](transports/http.md) | POST ingress, server-sent-events egress |

## Security and credentials

| Page | What it covers |
|---|---|
| [Credentials and HTTP API](credentials-and-http-api.md) | Credential URIs, admin and monitor APIs |
| [Credential Rotation](credentials-rotation.md) | Rotating secrets without downtime |
| [HTTP API](http-api.md) · [Examples](http-api-examples.md) | Administering a running bridge |
| [Monitor API Endpoints](http-api-monitor.md) | Health, liveness, readiness, topology and deep health |

## Clustering

| Page | What it covers |
|---|---|
| [Cluster overview](cluster/README.md) | Running several replicas without duplicate delivery |
| [Operating a cluster](cluster/operating.md) | Day-to-day operation |
| [Cost of ownership](cluster/tco.md) | What a cluster costs to run |

## Scenarios

Thirty progressive walkthroughs, from a single MQTT topic to cross-protocol
bridging. See the [scenarios index](scenarios/) — a few starting points:

- [MQTT to MQTT](scenarios/01-mqtt-to-mqtt.md) — the basics
- [MQTT to SQS](scenarios/03-mqtt-to-sqs.md) — crossing protocols
- [Fan-out with filtering](scenarios/04-fanout-with-filtering.md)
- [Durable shared outbox](scenarios/05-durable-shared-outbox.md) — surviving a crash mid-delivery
- [DLQ with the HTTP API](scenarios/07-dlq-with-http-api.md) — inspecting undeliverable messages
- [Clustered exclusive sessions](scenarios/08-clustered-exclusive-sessions.md)

## Operations

| Area | Pages |
|---|---|
| [Runbooks](runbooks/) | 18 incident procedures — broker outages, rollbacks, rotation failures, migrations |
| [AWS deployment](aws-deployment/) | Running on ECS/EFS with DynamoDB |
| [Timing audit](timing-audit.md) | How timing correctness is enforced |
| [Release notes](release-notes.md) | Per-version notes |

## Design decisions

[Architecture decision records](adr/) — 15 records covering the choices behind
the delivery model, clustering, storage and configuration.

---

Design and contributor documentation (architecture, domain model, glossary,
plugin authoring, testing rules) lives in the repository root and is not
published here. See the
[repository on GitHub](https://github.com/mariotoffia/gobridge).
