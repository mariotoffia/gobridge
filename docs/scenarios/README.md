# Scenarios Index

This directory collects worked examples that show GoBridge end-to-end: each
scenario pairs a YAML configuration with the matching Go composition root and
describes the observable behavior on the wire. The index exists so operators
picking a starting point and contributors hunting for an example near a feature
they are touching can find the right scenario without scanning twenty-six files.

There are two families:

- **Transport scenarios** (`01-..21-*.md`) — runtime, configuration, processors,
  routing, and adapter behavior. Run locally against brokers in `tests/`.
- **CDK deployment scenarios** (`cdk/01..05-*.md`) — packaging and operating
  GoBridge on AWS ECS Fargate using the L3 constructs in
  `deployment/aws-filebased-config/cdk`.

## How to read a scenario

Every scenario is self-contained. Expect a use-case statement, a Mermaid
architecture diagram, the full YAML config, the Go composition root snippet,
and a short "what you should observe" section covering logs, metrics, and
admin-API output. Scenarios do not depend on each other; you can jump
directly to the one closest to your problem.

If you are new to the project, start with [Scenario 1: MQTT-to-MQTT
Bridge](01-mqtt-to-mqtt.md) for the runtime model and [CDK Scenario 1:
Quickstart with Default VPC](cdk/01-quickstart-default-vpc.md) for the
deployment model.

## By concept

A scenario may legitimately appear in more than one group when it demonstrates
multiple capabilities of equal weight.

### Transports — getting started

Single-protocol bridges that establish the baseline `Receiver → Route → Sender`
flow per transport family.

- **1. MQTT-to-MQTT Bridge** ([01-mqtt-to-mqtt.md](01-mqtt-to-mqtt.md)) — Forwards messages between two topics on the same MQTT broker.
- **2. SQS-to-SQS Queue Bridge** ([02-sqs-to-sqs.md](02-sqs-to-sqs.md)) — Moves messages between two AWS SQS queues with at-least-once delivery.
- **19. RabbitMQ Queue-to-Queue Bridge** ([19-rabbitmq-to-rabbitmq.md](19-rabbitmq-to-rabbitmq.md)) — Bridges two RabbitMQ queues over AMQP 0.9.1 with publisher confirms.
- **20. AMQP 1.0 Queue Bridge (Artemis / AWS MQ)** ([20-amqp10-artemis-bridge.md](20-amqp10-artemis-bridge.md)) — Bridges two AMQP 1.0 queues against ActiveMQ Artemis or Amazon MQ.

### Cross-protocol bridging

Scenarios where the receiver and sender speak different protocols and the
runtime owns the translation.

- **3. MQTT-to-SQS Cross-Transport Bridge** ([03-mqtt-to-sqs.md](03-mqtt-to-sqs.md)) — Subscribes to an MQTT topic and forwards each message to an SQS queue.
- **21. Cross-Protocol AMQP Bridge** ([21-amqp-cross-protocol.md](21-amqp-cross-protocol.md)) — Bridges AMQP 0.9.1 and AMQP 1.0 endpoints with header and property mapping.

### Routing & filtering

Demonstrates the routing engine: predicates, fan-out, content-based dispatch,
and dynamic destination selection.

- **4. Fan-Out with Filtering** ([04-fanout-with-filtering.md](04-fanout-with-filtering.md)) — Splits one inbound stream into multiple senders using header predicates.
- **12. Dynamic Destination Routing** ([12-dynamic-destination-routing.md](12-dynamic-destination-routing.md)) — Resolves the outbound `Address` per message from envelope metadata.
- **13. Content-Based Routing to SSE Streams** ([13-content-based-sse-routing.md](13-content-based-sse-routing.md)) — Routes messages to per-tenant Server-Sent Events streams based on payload content.
- **14. Multi-Tenant Priority Routing** ([14-multi-tenant-priority-routing.md](14-multi-tenant-priority-routing.md)) — Assigns priority lanes per tenant and enforces them through the route table.

### Processors & pipelines

Shows the processor pipeline (transform, enrich, circuit break) and how to
plug a custom processor in.

- **6. Transform + Circuit Breaker Pipeline** ([06-transform-circuit-breaker.md](06-transform-circuit-breaker.md)) — Chains a payload transformer with a circuit breaker around a flaky downstream sender.
- **17. Custom Processor Implementation** ([17-custom-processor.md](17-custom-processor.md)) — Implements, registers, and configures a user-defined processor following `PLUGIN.md`.

### Durability & operations

Outbox-backed delivery, dead-letter handling, and adapter-level resilience —
the day-2 operability story.

- **5. Durable Delivery with SharedOutbox** ([05-durable-shared-outbox.md](05-durable-shared-outbox.md)) — Persists envelopes to a shared outbox so delivery survives sender restarts.
- **7. DLQ with HTTP API Management** ([07-dlq-with-http-api.md](07-dlq-with-http-api.md)) — Captures `Permanent`/`Rejected` deliveries into a DLQ and manages them through the admin API.
- **16. Adapter Resilience Patterns** ([16-adapter-resilience-patterns.md](16-adapter-resilience-patterns.md)) — Shows reconnection, backoff, and `Transient`-error handling across adapters.

### Clustering & multi-tenancy

Multi-instance deployments, exclusive sessions, tenant isolation, and
tenant-scoped routing.

- **8. Clustered MQTT with Exclusive Sessions** ([08-clustered-exclusive-sessions.md](08-clustered-exclusive-sessions.md)) — Coordinates multiple bridge instances so only one holds a given MQTT session.
- **11. Multi-Tenant Azure Service Bus** ([11-multi-tenant-azure-servicebus.md](11-multi-tenant-azure-servicebus.md)) — Isolates tenants across Azure Service Bus namespaces or queues with per-tenant credentials.
- **14. Multi-Tenant Priority Routing** ([14-multi-tenant-priority-routing.md](14-multi-tenant-priority-routing.md)) — Combines tenant scoping with priority lanes on the route table.
- **23. Coordinated Cluster Config Rollout** ([23-coordinated-cluster-rollout.md](23-coordinated-cluster-rollout.md)) — Rolls a live-safe config change across a whole cohort behind an all-member commit barrier, with optional auto-revert.

### Configuration management

Layered configuration sources and live reconfiguration without restart.

- **9. Layered Configuration with DynamoDB Overlay** ([09-layered-dynamodb-config.md](09-layered-dynamodb-config.md)) — Layers a DynamoDB overlay on top of the file-based config source.
- **10. Dynamic Reconfiguration** ([10-dynamic-reconfiguration.md](10-dynamic-reconfiguration.md)) — Applies a new config at runtime and shows which components reload in place.
- **23. Coordinated Cluster Config Rollout** ([23-coordinated-cluster-rollout.md](23-coordinated-cluster-rollout.md)) — Changes a whole cohort's config at once, with no downtime, behind an all-member commit barrier (plus an optional confirm-window auto-revert).

### Security & credentials

Credential providers, rotation, and TLS termination.

- **15. HTTP Ingress with Credential-Based TLS Egress** ([15-http-ingress-with-credentials.md](15-http-ingress-with-credentials.md)) — Accepts HTTP ingress and forwards over TLS using a credential provider for client certs.
- **22. Kubernetes Secret-Mount Credentials** ([22-k8s-secret-mount-credentials.md](22-k8s-secret-mount-credentials.md)) — Backs `file://` credentials with a read-only Kubernetes Secret volume and rotates on Secret update.

### Observability

The full `slog` + OpenTelemetry traces + metrics story, including correlation
propagation through the pipeline.

- **18. Full-Stack Observability** ([18-observability.md](18-observability.md)) — Wires logs, traces, and metrics end-to-end and shows correlation IDs flowing across hops.

### AMQP family

The AMQP scenarios are grouped together because the protocol matrix matters:
GoBridge ships separate adapters for **AMQP 0.9.1** (RabbitMQ-style) and
**AMQP 1.0** (Artemis / Amazon MQ / Service Bus AMQP). Read scenario 21 to see
the boundary between the two on the wire.

- **19. RabbitMQ Queue-to-Queue Bridge** ([19-rabbitmq-to-rabbitmq.md](19-rabbitmq-to-rabbitmq.md)) — AMQP 0.9.1, classic queues, publisher confirms.
- **20. AMQP 1.0 Queue Bridge (Artemis / AWS MQ)** ([20-amqp10-artemis-bridge.md](20-amqp10-artemis-bridge.md)) — AMQP 1.0, link-based flow control.
- **21. Cross-Protocol AMQP Bridge** ([21-amqp-cross-protocol.md](21-amqp-cross-protocol.md)) — Bridges 0.9.1 and 1.0, including header and property mapping.

### CDK deployment

Distinct from the runtime scenarios above: these focus on packaging GoBridge
as a container and operating it on AWS ECS Fargate using the L3 constructs in
`deployment/aws-filebased-config/cdk`. They progress from a single-task
quickstart to a multi-bridge cluster.

- **CDK 1. Quickstart with Default VPC** ([cdk/01-quickstart-default-vpc.md](cdk/01-quickstart-default-vpc.md)) — One-command stack with a fresh VPC, EFS, and a single Fargate task.
- **CDK 2. Custom VPC & Existing Infrastructure** ([cdk/02-custom-vpc.md](cdk/02-custom-vpc.md)) — Reuses an existing VPC, subnets, and security groups via L2 constructs.
- **CDK 3. HTTP Transport Behind API Gateway** ([cdk/03-api-gateway.md](cdk/03-api-gateway.md)) — Fronts the HTTP transport with API Gateway for managed ingress.
- **CDK 4. Production-Ready Stack with Monitoring** ([cdk/04-production-stack.md](cdk/04-production-stack.md)) — Adds CloudWatch dashboards, alarms, autoscaling, and least-privilege IAM.
- **CDK 5. Multi-Bridge Cluster with Shared EFS** ([cdk/05-multi-bridge-cluster.md](cdk/05-multi-bridge-cluster.md)) — Runs multiple bridge tasks behind a shared EFS configuration mount.

## By transport / adapter

Reverse index for readers who already know which broker they need to bridge.

- **MQTT** — [1](01-mqtt-to-mqtt.md), [3](03-mqtt-to-sqs.md), [4](04-fanout-with-filtering.md), [8](08-clustered-exclusive-sessions.md), [23](23-coordinated-cluster-rollout.md)
- **AWS SQS** — [2](02-sqs-to-sqs.md), [3](03-mqtt-to-sqs.md), [12](12-dynamic-destination-routing.md)
- **Azure Service Bus** — [11](11-multi-tenant-azure-servicebus.md)
- **RabbitMQ / AMQP 0.9.1** — [19](19-rabbitmq-to-rabbitmq.md), [21](21-amqp-cross-protocol.md)
- **AMQP 1.0 (Artemis / Amazon MQ)** — [20](20-amqp10-artemis-bridge.md), [21](21-amqp-cross-protocol.md)
- **HTTP ingress / SSE egress** — [13](13-content-based-sse-routing.md), [15](15-http-ingress-with-credentials.md)
- **AWS ECS Fargate (deployment)** — [CDK 1](cdk/01-quickstart-default-vpc.md), [CDK 2](cdk/02-custom-vpc.md), [CDK 3](cdk/03-api-gateway.md), [CDK 4](cdk/04-production-stack.md), [CDK 5](cdk/05-multi-bridge-cluster.md)

## By feature / capability

Reverse index for readers who know which capability they need to evaluate.

- **SharedOutbox (durable delivery)** — [5](05-durable-shared-outbox.md)
- **DLQ + HTTP admin API** — [7](07-dlq-with-http-api.md)
- **Circuit breaker** — [6](06-transform-circuit-breaker.md), [16](16-adapter-resilience-patterns.md)
- **Custom processor** — [17](17-custom-processor.md)
- **Dynamic reconfiguration** — [10](10-dynamic-reconfiguration.md)
- **Layered configuration** — [9](09-layered-dynamodb-config.md)
- **Credentials & rotation** — [15](15-http-ingress-with-credentials.md), [22](22-k8s-secret-mount-credentials.md)
- **Clustered exclusive sessions** — [8](08-clustered-exclusive-sessions.md)
- **Coordinated cluster rollout / confirm window** — [23](23-coordinated-cluster-rollout.md)
- **Priority routing** — [14](14-multi-tenant-priority-routing.md)
- **Content-based routing** — [4](04-fanout-with-filtering.md), [13](13-content-based-sse-routing.md)
- **Observability (logs / traces / metrics)** — [18](18-observability.md)

## Authoring a new scenario

A dedicated `docs/scenarios/_template.md` is tracked under backlog item L-13
and will land in a later pass. Until then, copy the closest existing scenario
and adapt it: [Scenario 1](01-mqtt-to-mqtt.md) for a transport scenario, or
[CDK Scenario 1](cdk/01-quickstart-default-vpc.md) for a deployment scenario.
Keep section ordering (Use Case → Architecture → Configuration → Composition
Root → Observable Behavior) and add the new entry to every relevant group in
this index.
