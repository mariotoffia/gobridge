# Scenario 19: RabbitMQ Queue-to-Queue Bridge

Route messages between two RabbitMQ queues on the same broker with automatic topology declaration.

## Use Case

An order service publishes events to an `orders` exchange. A downstream analytics service consumes from a different queue. The bridge subscribes to the order events, and forwards them to an analytics queue -- both on the same RabbitMQ instance.

## Architecture

```mermaid
flowchart LR
    subgraph RabbitMQ
        EX1["Exchange\norders\n(direct)"]
        Q1["Queue\norder-events"]
        EX2["Exchange\nanalytics\n(direct)"]
        Q2["Queue\nanalytics-events"]
        EX1 -->|"routing_key=new"| Q1
        EX2 -->|"routing_key=order"| Q2
    end

    subgraph GoBridge
        R["Receiver\norder-in"]
        Route["Route\nforward"]
        S["Sender\nanalytics-out"]
    end

    Q1 -->|consume| R
    R --> Route
    Route --> S
    S -->|publish| EX2

    style Route fill:#f96,stroke:#333
    style GoBridge fill:#eef,stroke:#333
```

## Configuration

```yaml
bridge:
  id: rabbit-forwarder

sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      broker_url: "amqp://guest:guest@localhost:5672/"
      heartbeat: "10s"

receivers:
  - id: order-in
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "order-events"
        options:
          exchange: "orders"
          exchange_type: "direct"
          routing_key: "new"
          durable: true
    options:
      queue_name: "order-events"
      prefetch_count: 20

senders:
  - id: analytics-out
    transport: amqp091
    session_id: rabbit-conn
    options:
      exchange: "analytics"
      routing_key: "order"

bindings:
  - id: to-analytics
    sender_id: analytics-out
    address: analytics-events

routes:
  - id: forward
    receiver_id: order-in
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-analytics]
```

## Config Walkthrough

### Session

- **`transport: amqp091`** -- Uses the RabbitMQ (AMQP 0-9-1) transport adapter.
- **`broker_url`** -- Standard AMQP URI with credentials. Supports `amqps://` for TLS.
- **`heartbeat: 10s`** -- The connection heartbeat interval. RabbitMQ uses this to detect dead connections.

Both receiver and sender share this session, which means one TCP connection and one reconnect loop for both.

### Topology Declaration

The `topics[].options` block tells the session what to declare during `Reconcile`:

| Field | Value | Purpose |
|-------|-------|---------|
| `exchange` | `orders` | Exchange name to declare |
| `exchange_type` | `direct` | Direct routing by key |
| `routing_key` | `new` | Binding key between exchange and queue |
| `durable` | `true` | Survives broker restart |

On startup, the session declares:
1. Exchange `orders` (direct, durable)
2. Queue `order-events` (durable)
3. Binding from `orders` to `order-events` with routing key `new`

After a reconnect, it redeclares the same topology. This is safe -- RabbitMQ treats repeated declarations as idempotent if parameters match.

### Receiver

- **`queue_name: order-events`** -- Consume from this queue.
- **`prefetch_count: 20`** -- The broker pushes up to 20 unacknowledged messages to the consumer. Higher values increase throughput at the cost of memory.

### Sender

- **`exchange: analytics`** -- Publish to this exchange. The exchange must already exist or be declared by another session topic.
- **`routing_key: order`** -- Used to route the published message.

## Component Relationship

```mermaid
graph TD
    Session["Session: rabbit-conn\n(AMQP connection)"]
    Recv["Receiver: order-in\n(consumes order-events)"]
    Send["Sender: analytics-out\n(publishes to analytics)"]
    Bind["Binding: to-analytics"]
    Route["Route: forward"]

    Session --> Recv
    Session --> Send
    Recv --> Route
    Route --> Bind
    Bind --> Send

    style Route fill:#f96,stroke:#333
    style Session fill:#6bf,stroke:#333
```

## Go Bootstrap

```go
cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto)

rt, _ := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    RegisterTransport("amqp091", amqp091.NewFactory(logger)).
    Build(ctx)

rt.Start(ctx)
```

## Variations

### TLS Connection

```yaml
sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      broker_url: "amqps://rabbit.example.com:5671/"
      tls:
        enable: true
        ca_cert_file: /etc/certs/ca.pem
        cert_file: /etc/certs/client.pem
        key_file: /etc/certs/client.key
```

### Virtual Hosts

Isolate traffic using RabbitMQ virtual hosts:

```yaml
sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      broker_url: "amqp://guest:guest@localhost:5672/"
      vhost: "/production"
```

### Fanout Exchange

Broadcast to all bound queues:

```yaml
receivers:
  - id: broadcast-in
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "broadcast-queue"
        options:
          exchange: "events"
          exchange_type: "fanout"
          durable: true
    options:
      queue_name: "broadcast-queue"
```

With a fanout exchange, the `routing_key` is ignored. Every queue bound to the exchange receives a copy.

### Topic Exchange with Wildcards

```yaml
receivers:
  - id: topic-in
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "order-updates"
        options:
          exchange: "events"
          exchange_type: "topic"
          routing_key: "order.*.updated"
          durable: true
    options:
      queue_name: "order-updates"
```

RabbitMQ topic exchanges support `*` (one word) and `#` (zero or more words) wildcards in routing keys.
