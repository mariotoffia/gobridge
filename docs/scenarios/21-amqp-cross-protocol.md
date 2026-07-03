# Scenario 21: Cross-Protocol AMQP Bridge

Bridge messages between RabbitMQ (AMQP 0-9-1) and an AMQP 1.0 broker. GoBridge normalizes messages into its `Envelope` format, making the protocol boundary transparent.

## Use Case

Your organization runs RabbitMQ for internal microservice communication and Apache Artemis (or AWS MQ) as the external-facing message broker. Orders arrive on a RabbitMQ queue from internal services. The bridge forwards them to an Artemis queue where external partners consume via AMQP 1.0.

A second route handles the reverse: responses from the Artemis broker flow back to a RabbitMQ queue.

## Architecture

```mermaid
flowchart LR
    subgraph RabbitMQ ["RabbitMQ (AMQP 0-9-1)"]
        RQ1["Queue\ninternal-orders"]
        RQ2["Queue\nexternal-responses"]
    end

    subgraph GoBridge
        R1["Receiver\nrabbit-in"]
        Route1["Route\norders-out"]
        S1["Sender\nartemis-out"]
        R2["Receiver\nartemis-in"]
        Route2["Route\nresponses-back"]
        S2["Sender\nrabbit-out"]
    end

    subgraph Artemis ["Artemis (AMQP 1.0)"]
        AQ1["queue://partner-orders"]
        AQ2["queue://partner-responses"]
    end

    RQ1 -->|consume| R1
    R1 --> Route1
    Route1 --> S1
    S1 -->|send| AQ1

    AQ2 -->|"link credit"| R2
    R2 --> Route2
    Route2 --> S2
    S2 -->|publish| RQ2

    style Route1 fill:#f96,stroke:#333
    style Route2 fill:#f96,stroke:#333
    style GoBridge fill:#eef,stroke:#333
```

## Configuration

```yaml
bridge:
  id: cross-protocol-bridge

sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      session:
        broker_url: "amqp://user:pass@rabbitmq.internal:5672/"
        heartbeat: "10s"

  - id: artemis-conn
    transport: amqp10
    options:
      session:
        address: "amqp://artemis.external:5672"
        container_id: "cross-bridge-01"
        username: "bridge-user"
        password: "bridge-pass"

receivers:
  - id: rabbit-in
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "internal-orders"
        options:
          subscription:
            exchange: "orders"
            exchange_type: "direct"
            routing_key: "outbound"
            durable: true
    options:
      receiver:
        queue_name: "internal-orders"
        prefetch_count: 50

  - id: artemis-in
    transport: amqp10
    session_id: artemis-conn
    options:
      receiver:
        address: "queue://partner-responses"
        link_credit: 20

senders:
  - id: artemis-out
    transport: amqp10
    session_id: artemis-conn
    options:
      sender:
        address: "queue://partner-orders"
        timeout: "30s"

  - id: rabbit-out
    transport: amqp091
    session_id: rabbit-conn
    options:
      sender:
        exchange: "responses"
        routing_key: "inbound"

bindings:
  - id: to-artemis
    sender_id: artemis-out
    address: "queue://partner-orders"

  - id: to-rabbit
    sender_id: rabbit-out
    address: inbound

routes:
  - id: orders-out
    receiver_id: rabbit-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-artemis]
    policy:
      max_in_flight: 100

  - id: responses-back
    receiver_id: artemis-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-rabbit]
    policy:
      max_in_flight: 50
```

## Config Walkthrough

### Two Sessions, Two Protocols

The bridge maintains two independent sessions:

| Session | Transport | Protocol | Connection |
|---------|-----------|----------|------------|
| `rabbit-conn` | `amqp091` | AMQP 0-9-1 | Single TCP connection, channels per receiver/sender |
| `artemis-conn` | `amqp10` | AMQP 1.0 | Single TCP connection, links per receiver/sender |

Each session handles its own reconnection independently. A RabbitMQ outage does not affect the Artemis session, and vice versa.

### Header Translation

GoBridge normalizes all transport-specific headers into its `Envelope` model. When a message crosses protocol boundaries:

```mermaid
flowchart LR
    subgraph "RabbitMQ Ingress"
        RH["amqp091.message-id\namqp091.content-type\namqp091.routing-key\n(+ user headers)"]
    end

    subgraph "Envelope"
        EH["ID\nSubject\nPayload\nHeaders map"]
    end

    subgraph "Artemis Egress"
        AH["amqp10.message-id\namqp10.content-type\namqp10.subject\n(+ application properties)"]
    end

    RH -->|"receiver maps"| EH
    EH -->|"sender maps"| AH
```

Transport-specific properties (e.g., `amqp091.delivery-tag`, `amqp091.exchange`) are carried as envelope headers. On egress, the target transport maps relevant headers back to its own properties. Non-matching transport-prefixed headers pass through as application properties or user headers.

Custom headers (without transport prefixes) pass through untouched. Reserved `x-bridge.*` headers are stripped at ingress.

### Bidirectional Routing

Two routes run in parallel:

| Route | Direction | Ingress | Egress |
|-------|-----------|---------|--------|
| `orders-out` | RabbitMQ -> Artemis | AMQP 0-9-1 consumer | AMQP 1.0 sender link |
| `responses-back` | Artemis -> RabbitMQ | AMQP 1.0 receiver link | AMQP 0-9-1 publisher |

Each route operates independently with its own concurrency limit (`max_in_flight`).

> **Binding addresses and the nested `options:` shape.** Transport options now
> nest under role blocks (`session:` / `receiver:` / `sender:`); the flat form
> no longer decodes. A binding needs no `options:` block -- it carries only its
> `address`, which the runtime propagates as `OutboundMessage.Address`. For AMQP
> 1.0 the binding `address` **must equal** the sender's `sender.address` exactly
> (or be empty) -- the sender link is address-bound and rejects a mismatched
> per-dispatch address. For AMQP 0-9-1 the binding `address` is used as the
> publish **routing key** (it overrides `sender.routing_key`).

### Settlement Differences

The two protocols settle messages differently, but the bridge abstracts this behind the `Delivery` interface:

| Operation | RabbitMQ (0-9-1) | Artemis (1.0) |
|-----------|-----------------|---------------|
| Success | `Ack` -> `delivery.Ack(false)` | `Ack` -> `AcceptMessage` |
| Retry | `Retry` -> `Nack(false, true)` (requeue) | `Retry(0)` -> `ReleaseMessage` |
| Retry with delay | `Retry` -> `Nack(false, true)` (delay logged, not enforced) | `Retry(>0)` -> `ModifyMessage(delivery-failed=true)` |

RabbitMQ requeues immediately on retry regardless of the `after` parameter. Artemis can schedule delayed retry when the `delivery-failed` flag is set, depending on broker configuration.

## Go Bootstrap

```go
cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto)

rt, _ := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    RegisterTransport("amqp091", amqp091.NewFactory(logger)).
    RegisterTransport("amqp10", amqp10.NewFactory(logger)).
    Build(ctx)

rt.Start(ctx)
```

Both transport factories must be registered since the config references both protocols.

## Variations

### AWS MQ as the AMQP 1.0 Endpoint

Replace Artemis with AWS MQ for ActiveMQ. The bridge config changes only the session:

```yaml
sessions:
  - id: aws-mq-conn
    transport: amqp10
    options:
      session:
        address: "amqps://b-xxxx-xxxx.mq.eu-west-1.amazonaws.com:5671"
        container_id: "cross-bridge-aws"
        username: "admin"
        password: "aws-mq-password"
        tls:
          enable: true
```

All receivers and senders referencing `artemis-conn` switch to `aws-mq-conn`. Everything else stays the same.

### Adding a Transform Processor

Adapt message format between internal and external schemas:

```yaml
routes:
  - id: orders-out
    receiver_id: rabbit-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-artemis]
    processors: [format-external]
    policy:
      max_in_flight: 100
```

`transform.New` takes a single `Config` and returns `(*transform.Processor, error)` -- there is no name argument and no functional options. Every rewrite is a declarative JSONPath mapping in `Config.Mappings`; a `Target` prefixed with `header.` writes to an envelope header instead of the payload body.

```go
transformProc, _ := transform.New(transform.Config{
    Name: "format-external",
    Mappings: []transform.FieldMapping{
        // Rename internal payload fields to the external partner schema.
        transform.SimpleMapping("$.order.id", "orderId"),
        transform.SimpleMapping("$.order.customerRef", "partner.customer"),
        // Promote a payload field into a header the partner consumer reads.
        transform.SimpleMapping("$.order.region", "header.partner-region"),
        // Stamp partner-version=2. Mappings have no dedicated "constant"
        // field, so point Source at a path the payload never carries and let
        // DefaultValue supply the value: the resolver falls back to
        // DefaultValue whenever the Source yields no match.
        {Source: "$.__partner_version__", Target: "header.partner-version", DefaultValue: "2"},
    },
})
```

Register it under the name the route references so `processors: [format-external]` resolves:

```go
rt, _ := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    // ... amqp091 / amqp10 transport factories as in the Go Bootstrap section ...
    RegisterProcessor("format-external", transformProc).
    Build(ctx)
```

> **Constant headers are not a first-class transform capability.** The processor is mapping-based: each rule copies a JSONPath source from the payload to a payload or `header.` target. There is no "set literal header" option, so the `partner-version` rule above leans on `DefaultValue`, which applies only when the `Source` is absent -- hence the synthetic source path the payload never contains. Header writes also happen only for a non-empty, valid-JSON payload; with the default `FailOnError: false` and no required mappings, an empty or non-JSON body passes through untouched.

### Durable Outbox for Guaranteed Cross-Protocol Delivery

For critical messages, use `shared_outbox` delivery mode with a persistent store. The bridge persists the message before acknowledging the source, preventing data loss if the target broker is temporarily unavailable.

```yaml
stores:
  outbox:
    backend: sqlite
    options:
      dsn: "file:bridge.db?_journal=WAL"

routes:
  - id: orders-out
    receiver_id: rabbit-in
    delivery_mode: shared_outbox
    dispatch_mode: single
    bindings: [to-artemis]
    policy:
      max_in_flight: 100
```

### Three-Way Bridge: RabbitMQ + Artemis + SQS

Fan out from RabbitMQ to both Artemis and SQS:

```yaml
senders:
  - id: artemis-out
    transport: amqp10
    session_id: artemis-conn
    options:
      sender:
        address: "queue://partner-orders"

  - id: sqs-archive
    transport: sqs
    options:
      queue_name: "order-archive"
      region: "eu-west-1"
      batch_size: 10

bindings:
  - id: to-artemis
    sender_id: artemis-out
    address: "queue://partner-orders"
  - id: to-archive
    sender_id: sqs-archive
    address: order-archive

routes:
  - id: orders-fanout
    receiver_id: rabbit-in
    delivery_mode: direct_hold
    dispatch_mode: all
    bindings: [to-artemis, to-archive]
    policy:
      max_in_flight: 100
```

With `dispatch_mode: all`, the bridge sends to both bindings. It acknowledges the source message only after both sends succeed.
