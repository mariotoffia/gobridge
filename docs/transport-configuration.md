# Transport Configuration Reference

GoBridge uses a factory-based architecture where each transport is registered
by name and creates sessions, receivers, and senders from declarative YAML
configuration. The YAML `options:` block under each session, receiver, and
sender is decoded **once** at config-parse time into the transport's typed
`ports.PluginConfig` (e.g. `paho.Config`, `sqs.SenderConfig`) by the decoder
the adapter registered on `*ports.Registry` from its `register.go`. The
typed value is then carried on `ports.SessionSpec.Config` /
`ports.ReceiverSpec.Config` / `ports.SenderSpec.Config` to the factory — the
runtime never hands `map[string]any` to plugin code. See
[`docs/typed-plugin-config.adoc`](typed-plugin-config.adoc) for the full
contract and [`PLUGIN.md`](../PLUGIN.md#typed-plugin-config) for the
adapter-author summary.

## Factory Architecture

```mermaid
flowchart TD
    YAML["bridge.yaml"] --> Parse["config.ParseFile()"]
    Parse --> CFG["config.BridgeConfig"]
    CFG --> Builder["bridge.Builder"]

    Builder -->|"RegisterTransport(name, factory)"| TF["TransportFactory"]

    TF -->|"NewSession(ctx, SessionDef)"| Session["ports.Session"]
    TF -->|"NewReceiver(ctx, ReceiverDef, session)"| Receiver["ports.Receiver"]
    TF -->|"NewSender(ctx, SenderDef, session)"| Sender["ports.Sender"]

    Session -.->|"bound to"| Receiver
    Session -.->|"bound to"| Sender

    subgraph Stateful ["Stateful Transports (MQTT, AMQP 0-9-1, AMQP 1.0)"]
        Session
    end

    subgraph Stateless ["Stateless Transports (SQS, Service Bus, HTTP)"]
        direction LR
        NilSession["session = nil"]
    end
```

Stateful transports (MQTT, AMQP 0-9-1, AMQP 1.0) create a real `Session`
that receivers and senders share. Stateless transports (SQS, Azure Service
Bus, HTTP) return `nil` from `NewSession` -- receivers and senders manage
their own connections.

## Subject vs. Address

Two concepts are kept strictly separate across every transport:

- **`Envelope.Subject`** — the logical event subject. It is set by the producer (or by the bridge's ingress mapping) and is never mutated by the runtime to inject a destination.
- **`ports.OutboundMessage.Address`** — the transport destination chosen by the route's dispatch plan (`DispatchPlan.Address`). The runtime passes it to `Sender.Send(ctx, OutboundMessage)` separately from the envelope.

Each transport carries the logical subject over the wire in a transport-native field where one exists; for transports that do not have a dedicated subject slot (MQTT, AMQP 0-9-1) the bridge uses an explicit non-reserved carrier named **`gobridge.subject`**.

| Transport | Outbound destination from `OutboundMessage.Address` | Subject carrier on the wire | Ingress: what sets `Envelope.Subject` |
|-----------|------------------------------------------------------|-----------------------------|---------------------------------------|
| MQTT | Publish topic (or `default_topic` when address is empty). Never derived from `Envelope.Subject`. | MQTT user property `gobridge.subject` | The `gobridge.subject` user property. The publish topic the broker delivered on is exposed in `Headers["mqtt.topic"]`. |
| AMQP 0-9-1 | Routing key (when sender `routing_key` is empty). Never derived from `Envelope.Subject`. | AMQP header `gobridge.subject` | The `gobridge.subject` AMQP header. The broker's `Delivery.RoutingKey` is preserved in `Headers["amqp091.routing-key"]`. |
| AMQP 1.0 | Validated against the configured sender link address (empty allowed; mismatch fails fast). Per-address dynamic links are deferred. | `Message.Properties.Subject` | `Message.Properties.Subject`. There is no fallback from the link address. |
| SQS | Reserved for future dynamic queue selection. Today the queue is sender-state. | `Subject` message attribute | The `Subject` message attribute. There is no fallback from the queue URL/name. When the producer supplies no explicit `MessageDeduplicationId`, FIFO dedup is derived from an md5 hash of the payload, subject and id (or creation time when id is empty) — not the subject alone. |
| Azure Service Bus | Reserved for future dynamic entity selection. Today the entity is sender-state. | `Message.Subject` | `Message.Subject`. There is no fallback from the queue/topic name. |
| HTTP / SSE | Path is sender-state. | JSON field `subject` | JSON field `subject`. |

Reserved `x-bridge.*` headers are not used as the cross-transport subject carrier; `gobridge.subject` is the explicit, application-visible name where no native field exists.

---

## MQTT (Paho)

**Transport name:** `mqtt`
**Factory:** `paho.NewFactory(logger)`
**Capabilities:** `stateful_session`, `exclusive_identity`

MQTT requires a session. Multiple receivers and senders can share one session
(one TCP connection). Session mode controls lifecycle and ownership semantics.

### Session Modes

| Mode | `session_mode` | `clean_start` | Behavior |
|------|---------------|---------------|----------|
| Ephemeral | `ephemeral` (default) | `true` | No state survives disconnect |
| Persistent | `persistent` | `false` | Broker retains subscriptions and queued messages |
| Exclusive | `exclusive` | `false` | Lease-based single holder; requires a lease store |

### YAML Example

```yaml
sessions:
  - id: mqtt-session-1
    transport: mqtt
    session_mode: persistent
    options:
      broker_url: "tcp://broker.example.com:1883"
      client_id: "bridge-node-01"
      keep_alive: 30
      connect_timeout: "30s"
      reconnect_timeout: "30s"
      clean_start: false
      session_expiry_interval: 3600
      username: "bridge"
      password: "secret"
      tls:
        enable: true
        ca_cert_file: "/etc/certs/ca.pem"
        cert_file: "/etc/certs/client.pem"
        key_file: "/etc/certs/client-key.pem"
        insecure_skip_verify: false

receivers:
  - id: sensor-receiver
    transport: mqtt
    session_id: mqtt-session-1
    topics:
      - topic: "sensors/+/temperature"
        qos: 1
      - topic: "sensors/+/humidity"
        qos: 1

senders:
  - id: command-sender
    transport: mqtt
    session_id: mqtt-session-1
    options:
      default_topic: "devices/commands"
      qos: 1
      retain: false
      timeout: "30s"
```

### Session Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- | Single broker URL (e.g. `tcp://host:1883`) |
| `broker_urls` | []string | -- | Multiple broker URLs for failover |
| `client_id` | string | **required** | MQTT client identifier |
| `keep_alive` | int | 30 | Keep-alive interval in seconds |
| `connect_timeout` | duration | `30s` | TCP + MQTT CONNECT timeout |
| `reconnect_timeout` | duration | `30s` | Delay before reconnect attempt |
| `clean_start` | bool | `true` | MQTT 5 clean start flag |
| `session_expiry_interval` | int | 0 | Session expiry in seconds (MQTT 5) |
| `username` | string | -- | Authentication username |
| `password` | string | -- | Authentication password |
| `tls.enable` | bool | `false` | Enable TLS |
| `tls.ca_cert_file` | string | -- | CA certificate file path |
| `tls.cert_file` | string | -- | Client certificate file path |
| `tls.key_file` | string | -- | Client private key file path |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `default_topic` | string | -- | Fallback publish topic used when `OutboundMessage.Address` is empty. The publish topic is never read from `Envelope.Subject`. |
| `qos` | int | 1 | MQTT QoS level (0, 1, or 2) |
| `retain` | bool | `false` | MQTT retain flag |
| `timeout` | duration | `30s` | Per-publish timeout |

### Resilience Behavior

- **Publish timeout fallback.** When `timeout` is set to `0` (or omitted and no
  context deadline is present), the sender applies a 60-second safety-net
  timeout to prevent indefinite hangs on stalled broker connections.
- **Case-insensitive error classification.** MQTT error messages from brokers
  are matched case-insensitively. `"Connection Refused"`, `"CONNECTION REFUSED"`,
  and `"connection refused"` are all correctly classified as `ErrConnectionLost`,
  enabling proper retry behavior regardless of broker formatting.
- **Properties deep-copy for shared sessions.** When multiple receivers share an
  MQTT session, each handler goroutine receives an independent deep-copy of the
  MQTT Properties (User properties, CorrelationData, ContentType, etc.),
  preventing data races under concurrent dispatch.

### Receiver Options

MQTT receivers have no transport-specific options. Subscriptions are declared
in the `topics[]` array on the `ReceiverDef`, not in the `options` map.

---

## AWS SQS

**Transport name:** `sqs`
**Factory:** `sqs.NewFactory(logger)`
**Capabilities:** `visibility_extension`, `source_redelivery`

SQS is stateless -- no sessions are needed. Each receiver and sender opens
its own AWS SDK client. Receivers support long polling and automatic
visibility extension. Senders support batching and FIFO queues.

### YAML Example

```yaml
receivers:
  - id: order-events
    transport: sqs
    options:
      queue_url: "https://sqs.eu-west-1.amazonaws.com/123456789012/orders"
      region: "eu-west-1"
      max_messages: 10
      wait_time_seconds: 20
      visibility_timeout: 60
      auto_extend: true
      sns_unwrap: false

senders:
  - id: notification-sender
    transport: sqs
    options:
      queue_name: "notifications"
      region: "eu-west-1"
      delay_seconds: 0
      batch_size: 10
      timeout: "30s"

  - id: fifo-sender
    transport: sqs
    options:
      queue_url: "https://sqs.eu-west-1.amazonaws.com/123456789012/orders.fifo"
      region: "eu-west-1"
      fifo: true
      message_group_id: "order-processing"
      batch_size: 10
```

### Receiver Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_url` | string | -- | Fully qualified SQS queue URL |
| `queue_name` | string | -- | Logical queue name (resolved at startup) |
| `region` | string | SDK default | AWS region |
| `endpoint` | string | -- | Override endpoint (for LocalStack) |
| `profile` | string | -- | AWS shared-config profile name |
| `max_messages` | int | 10 | Messages per ReceiveMessage call (1--10) |
| `wait_time_seconds` | int | 20 | Long-poll duration in seconds (0--20) |
| `visibility_timeout` | int | 30 | Visibility timeout in seconds |
| `auto_extend` | bool | `true` | Renew visibility at 50% of timeout |
| `sns_unwrap` | bool | `false` | Extract inner message from SNS wrapper |

Either `queue_url` or `queue_name` must be provided.

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_url` | string | -- | Fully qualified SQS queue URL |
| `queue_name` | string | -- | Logical queue name (resolved at startup) |
| `region` | string | SDK default | AWS region |
| `endpoint` | string | -- | Override endpoint (for LocalStack) |
| `profile` | string | -- | AWS shared-config profile name |
| `delay_seconds` | int | 0 | Delivery delay in seconds (0--900) |
| `batch_size` | int | 10 | Messages per SendMessageBatch (1--10) |
| `timeout` | duration | `30s` | Per-call send timeout |
| `message_group_id` | string | -- | Default FIFO message group ID |
| `fifo` | bool | `false` | Mark queue as FIFO |

Either `queue_url` or `queue_name` must be provided.

### Resilience Behavior

- **Long-poll default.** `wait_time_seconds` defaults to `20` (maximum SQS
  long-poll duration) when not explicitly configured, preventing accidental
  short-polling which causes excessive API costs.
- **Receiver initialization timeout.** SQS receiver startup (client creation,
  queue URL resolution) is bounded by a 30-second timeout, preventing
  indefinite hangs when AWS credentials or endpoints are unavailable.
- **Per-poll timeout.** Each `ReceiveMessage` call has an explicit timeout of
  `WaitTimeSeconds + 10` seconds, protecting against network-level stalls
  beyond the SQS long-poll window.
- **Batch error classification.** SQS batch send failures distinguish between
  server faults (transient, retriable) and sender faults (permanent, not
  retriable). Messages with malformed payloads are classified as
  `ErrorRejected` and routed to DLQ instead of being retried indefinitely.
- **Adaptive auto-extend ticker.** When `Extend()` changes the SQS visibility
  timeout, the auto-extend ticker interval updates accordingly, preventing
  excessive or insufficient extend calls.

> **Tip:** Set the SQS native DLQ `maxReceiveCount` to at least
> `(bridge max retries + 3)` to prevent SQS from moving messages to the DLQ
> before the bridge has finished its own retry handling.

---

## Azure Service Bus

**Transport name:** `servicebus`
**Factory:** `servicebus.NewFactory(logger)`
**Capabilities:** `visibility_extension`

Azure Service Bus is stateless in GoBridge (no bridge-level sessions).
Connection credentials are nested directly in receiver/sender options.
The transport supports both queues and topic/subscription patterns, plus
Azure SB sessions for ordered processing.

### YAML Example

```yaml
receivers:
  - id: asb-queue-receiver
    transport: servicebus
    options:
      connection_string: "Endpoint=sb://myns.servicebus.windows.net/;SharedAccessKeyName=listen;SharedAccessKey=..."
      queue_name: "orders"
      max_messages: 10
      max_wait_time: "30s"
      prefetch: 50
      receive_mode: "PeekLock"
      lock_duration: "30s"
      auto_extend: true

  - id: asb-topic-receiver
    transport: servicebus
    options:
      namespace: "myns.servicebus.windows.net"
      use_managed_identity: true
      topic_name: "events"
      subscription_name: "bridge-sub"
      receive_mode: "PeekLock"
      session_id: "partition-1"

senders:
  - id: asb-queue-sender
    transport: servicebus
    options:
      connection_string: "Endpoint=sb://myns.servicebus.windows.net/;SharedAccessKeyName=send;SharedAccessKey=..."
      queue_name: "commands"
      batch_size: 10
      timeout: "30s"
      default_session_id: "partition-1"

  - id: asb-topic-sender
    transport: servicebus
    options:
      namespace: "myns.servicebus.windows.net"
      tenant_id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
      client_id: "ffffffff-0000-1111-2222-333333333333"
      client_secret: "my-secret"
      topic_name: "notifications"
```

### Receiver Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Service Bus queue name |
| `topic_name` | string | -- | Service Bus topic name |
| `subscription_name` | string | -- | Subscription on the topic |
| `session_id` | string | -- | Lock to a specific ASB session |
| `max_messages` | int | 10 | Messages per receive call (1--100) |
| `max_wait_time` | duration | `30s` | Maximum wait for messages |
| `prefetch` | int | 0 | AMQP link prefetch count |
| `receive_mode` | string | `PeekLock` | `PeekLock` or `ReceiveAndDelete` |
| `sub_queue` | string | -- | `""`, `"deadletter"`, or `"transferdeadletter"` |
| `lock_duration` | duration | `30s` | Expected lock duration (for auto-extend) |
| `auto_extend` | bool | `true` | Renew lock at 50% of duration |

Either `queue_name` or both `topic_name` + `subscription_name` are required.

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Service Bus queue name |
| `topic_name` | string | -- | Service Bus topic name |
| `default_session_id` | string | -- | Default ASB session for messages |
| `batch_size` | int | 10 | Upper bound on messages per batch |
| `timeout` | duration | `30s` | Per-call send timeout |

Either `queue_name` or `topic_name` is required.

### Connection Options (nested in receiver/sender)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `connection_string` | string | -- | Full Service Bus connection string |
| `namespace` | string | -- | Namespace FQDN (token-based auth) |
| `use_managed_identity` | bool | `false` | Use Azure Managed Identity |
| `tenant_id` | string | -- | Azure AD tenant (app auth) |
| `client_id` | string | -- | Azure AD app client ID |
| `client_secret` | string | -- | Azure AD app client secret |
| `ca_pem` | string | -- | Custom CA certificate (PEM string) |
| `insecure_skip_verify` | bool | `false` | Skip TLS server verification |

Either `connection_string` or `namespace` is required.

---

## HTTP Transport

**Transport name:** `http`
**Factory:** `httptransport.NewFactory(opts...)`
**Capabilities:** `http_endpoint`
**OpenAPI spec:** `spec/http-adapter/http-api.yaml`

The HTTP transport exposes receivers as POST endpoints and senders as
Server-Sent Events (SSE) GET endpoints. All endpoints are mounted on an
internal `http.ServeMux` accessible via `factory.Handler()`.

```mermaid
flowchart LR
    Client1["HTTP Client"] -->|"POST /receivers/{id}/messages"| Recv["Receiver Handler"]
    Recv --> Bridge["Bridge Pipeline"]
    Bridge --> SSE["SSE Sender"]
    SSE -->|"GET /senders/{id}/events"| Client2["SSE Client"]
```

### Authentication

Both receivers and senders support optional per-endpoint API key
authentication. When `api_key` is configured, requests must include the key
via `X-API-Key` header or `Authorization: Bearer` token. Keys are compared
using SHA-256 constant-time comparison to prevent timing and length-based
information leaks.

### YAML Example

```yaml
receivers:
  - id: webhook-receiver
    transport: http
    options:
      path: "/transport/http/receivers/webhook-receiver/messages"
      api_key: "recv-secret-min-16ch"
      max_body_size: 1048576

senders:
  - id: event-stream
    transport: http
    options:
      mode: "sse"
      path: "/transport/http/senders/event-stream/events"
      heartbeat_interval: "30s"
      api_key: "sse-secret-min-16ch"
      max_clients: 100
```

### Receiver Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `path` | string | `/transport/http/receivers/{id}/messages` | POST endpoint path |
| `api_key` | string | -- | Per-receiver API key (SHA-256 constant-time comparison) |
| `max_body_size` | int | 1048576 (1 MiB) | Maximum request body in bytes |

### Receiver Request Format

The receiver accepts JSON POST with the following fields:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `subject` | string | **yes** | Logical event subject. Mapped 1:1 to `Envelope.Subject`. Not a topic or routing key. |
| `payload` | any JSON | no | Message content (stored as raw bytes) |
| `id` | string | no | Caller-provided message ID |
| `headers` | object | no | Custom metadata (`X-Bridge-*` keys stripped) |
| `expires_at` | RFC 3339 | no | Message TTL (drives `on_expired` policy) |

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `mode` | string | `sse` | Sender mode (only `sse` supported) |
| `path` | string | `/transport/http/senders/{id}/events` | GET endpoint path |
| `heartbeat_interval` | duration | `30s` | SSE keep-alive heartbeat interval |
| `api_key` | string | -- | Per-sender API key (SHA-256 constant-time comparison) |
| `max_clients` | int | 10000 | Maximum concurrent SSE connections |

### Resilience Behavior

- **Content-Type validation.** HTTP ingress validates
  `Content-Type: application/json` when the header is present, returning
  `415 Unsupported Media Type` for non-JSON requests. Requests without a
  `Content-Type` header are accepted (assumed JSON).
- **Automatic envelope ID.** HTTP ingress generates a unique UUID envelope ID
  when the request omits the `id` field, ensuring all messages have traceable
  identifiers through the pipeline.
- **Forward error classification.** HTTP forwarder responses are classified by
  status code family: **5xx** responses map to `ErrUnavailable` (transient,
  retriable) and **4xx** responses map to `ErrForwardFailed` (permanent, not
  retriable), enabling correct DLQ routing for upstream HTTP failures.

### Cluster-Aware Routing

When a `RouteLocator` is configured, endpoints become cluster-aware:

- **Receivers**: If the target route is owned by a remote node, the message is
  transparently forwarded to the peer. The `X-Bridge-Forwarded: true` header
  prevents infinite loops.
- **Senders**: If the target route is remote, the client receives an HTTP 307
  redirect to the peer's SSE endpoint.

### Factory Options

The HTTP factory accepts functional options at registration time:

| Option | Description |
|--------|-------------|
| `WithPathPrefix(prefix)` | Override URL prefix (default `/transport/http`) |
| `WithRouteLocator(l)` | Set cluster-aware route locator |
| `WithMessageForwarder(fw)` | Set cluster message forwarder |
| `WithFactoryMetrics(m)` | Set metrics exporter |
| `WithFactoryLogger(l)` | Set structured logger |

---

## RabbitMQ (AMQP 0-9-1)

**Transport name:** `amqp091`
**Factory:** `amqp091.NewFactory(logger)`
**Capabilities:** `stateful_session`, `source_redelivery`

RabbitMQ uses a stateful session. A `Session` owns a single AMQP connection
with automatic reconnection. Receivers and senders each open their own
channel on that connection. `Reconcile` declares exchanges, queues, and
bindings from the `SessionPlan`.

### YAML Example

```yaml
sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      broker_url: "amqp://guest:guest@localhost:5672/"
      heartbeat: "10s"
      connect_timeout: "30s"
      reconnect_delay: "1s"
      tls:
        enable: false

receivers:
  - id: order-consumer
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "order-queue"
        options:
          exchange: "orders"
          exchange_type: "direct"
          routing_key: "new-order"
          durable: true
    options:
      queue_name: "order-queue"
      prefetch_count: 20
      auto_ack: false

senders:
  - id: notification-publisher
    transport: amqp091
    session_id: rabbit-conn
    options:
      exchange: "notifications"
      routing_key: "order.confirmed"
      timeout: "30s"
```

### Session Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- (required) | AMQP URI, e.g. `amqp://user:pass@host:5672/vhost` |
| `username` | string | -- | Injected into broker URL if no userinfo present |
| `password` | string | -- | Injected into broker URL if no userinfo present |
| `vhost` | string | -- | AMQP virtual host |
| `heartbeat` | duration | `10s` | Connection heartbeat interval |
| `connect_timeout` | duration | `30s` | Dial timeout per attempt |
| `reconnect_delay` | duration | `1s` | Initial delay before reconnect (grows to 30s) |
| `tls.enable` | bool | `false` | Enable TLS (`amqp091.DialTLS`) |
| `tls.ca_cert_file` | string | -- | CA certificate PEM file path |
| `tls.cert_file` | string | -- | Client certificate PEM file path |
| `tls.key_file` | string | -- | Client private key PEM file path |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

### Receiver Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Queue to consume from |
| `consumer_tag` | string | auto-generated | Consumer tag identifier |
| `auto_ack` | bool | `false` | Automatic acknowledgement (disables manual settlement) |
| `exclusive` | bool | `false` | Exclusive consumer |
| `prefetch_count` | int | `10` | QoS prefetch count |
| `prefetch_size` | int | `0` | QoS prefetch size in bytes |

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `exchange` | string | `""` | Target exchange name |
| `routing_key` | string | `""` | Routing key. When empty the sender uses `OutboundMessage.Address` (resolved from the dispatch plan). The routing key is **never** taken from `Envelope.Subject`. |
| `mandatory` | bool | `false` | Return unroutable messages |
| `immediate` | bool | `false` | Return messages when no consumer is ready |
| `timeout` | duration | `30s` | Per-publish timeout (applied when context has no deadline) |

### Topology Declaration (Reconcile)

AMQP 0-9-1 sessions support automatic topology declaration via `Reconcile`.
When a receiver has `topics[]` entries with `options`, the session declares
the exchange, queue, and binding during startup and after each reconnect.

```yaml
topics:
  - topic: "my-queue"
    options:
      exchange: "my-exchange"
      exchange_type: "direct"
      routing_key: "events"
      durable: true
```

This declares:

1. Exchange `my-exchange` of type `direct` (durable)
2. Queue `my-queue` (durable)
3. Binding from `my-exchange` to `my-queue` with routing key `events`

### Settlement Mapping

| `ports.Delivery` method | AMQP 0-9-1 operation | Notes |
|---|---|---|
| `Ack(ctx)` | `delivery.Ack(false)` | Single-message acknowledgement |
| `Retry(ctx, after, err)` | `delivery.Nack(false, true)` | Requeue; `after` is logged but not enforced |
| `Extend(ctx, deadline)` | -- | Returns `ErrNotSupported` |

### Resilience Behavior

- **Automatic reconnection.** On connection loss, the session retries with
  exponential backoff (1s initial, 30s cap) plus 25% jitter. After
  reconnecting, it re-runs `Reconcile` to redeclare topology.
- **Publisher confirms.** The sender opens its channel in confirm mode.
  Each `Send` waits for the broker to confirm receipt before returning.
- **Channel isolation.** Each receiver and sender opens its own channel.
  A channel-level error (e.g., queue deleted) does not affect other
  receivers and senders on the same connection.

---

## AMQP 1.0

**Transport name:** `amqp10`
**Factory:** `amqp10.NewFactory(logger)`
**Capabilities:** `stateful_session`

The AMQP 1.0 adapter works with any broker that speaks the AMQP 1.0 wire
protocol: Apache ActiveMQ Artemis, Solace PubSub+, Apache Qpid, Azure
Event Hubs (direct AMQP), and AWS MQ for ActiveMQ. Built on
[github.com/Azure/go-amqp](https://github.com/Azure/go-amqp).

A `Session` owns the TCP connection and AMQP session. Receivers and senders
create links on that session. Topology (queues, topics) is broker-managed --
AMQP 1.0 does not declare exchanges or queues.

### YAML Example

```yaml
sessions:
  - id: artemis-conn
    transport: amqp10
    options:
      address: "amqp://localhost:5672"
      container_id: "bridge-node-01"
      connect_timeout: "30s"
      reconnect_delay: "1s"
      idle_timeout: "2m"
      max_frame_size: 65536
      username: "admin"
      password: "admin"

receivers:
  - id: order-receiver
    transport: amqp10
    session_id: artemis-conn
    options:
      address: "queue://orders"
      link_credit: 20
      durability_mode: 0

senders:
  - id: event-publisher
    transport: amqp10
    session_id: artemis-conn
    options:
      address: "topic://events"
      timeout: "30s"
      durability_mode: 0
```

### Session Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Broker URL, e.g. `amqp://host:5672` or `amqps://host:5671` |
| `container_id` | string | -- | AMQP container ID; identifies this client to the broker |
| `connect_timeout` | duration | `30s` | Dial timeout per connection attempt |
| `reconnect_delay` | duration | `1s` | Initial delay before reconnect (grows to 30s) |
| `idle_timeout` | duration | `2m` | Connection idle timeout sent to broker |
| `max_frame_size` | int | `65536` | Maximum AMQP frame size in bytes |
| `username` | string | -- | SASL PLAIN username |
| `password` | string | -- | SASL PLAIN password |
| `tls.enable` | bool | `false` | Enable TLS |
| `tls.ca_cert_file` | string | -- | CA certificate PEM file path |
| `tls.cert_file` | string | -- | Client certificate PEM file path |
| `tls.key_file` | string | -- | Client private key PEM file path |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

### Receiver Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Queue or topic address to consume from |
| `link_credit` | int | `10` | Prefetch credit; how many messages the broker sends ahead |
| `durability_mode` | int | `0` | AMQP durability level for the receiver link |

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Target address to publish to |
| `timeout` | duration | `30s` | Send timeout per message |
| `durability_mode` | int | `0` | AMQP durability level for the sender link |

### Settlement Mapping

| `ports.Delivery` method | AMQP 1.0 disposition | Notes |
|---|---|---|
| `Ack(ctx)` | `AcceptMessage` (accepted) | Message removed from queue |
| `Retry(ctx, 0, err)` | `ReleaseMessage` (released) | Immediate redelivery to any consumer |
| `Retry(ctx, >0, err)` | `ModifyMessage` (delivery-failed=true) | Signals broker to schedule retry |
| `Extend(ctx, deadline)` | -- | Returns `ErrNotSupported` |

Settlement is idempotent. Only the first call on a `Delivery` performs the
disposition; subsequent calls are no-ops.

### AMQP 1.0 vs AMQP 0-9-1

| Aspect | AMQP 1.0 | AMQP 0-9-1 (RabbitMQ) |
|--------|----------|----------------------|
| Topology | Broker-managed (no declare) | Client declares exchanges, queues, bindings |
| Retry with delay | `ModifyMessage` (broker-dependent) | Nack+requeue only (no native delay) |
| Flow control | Link credit (prefetch) | Channel-level QoS prefetch |
| Batch send | Individual messages over link | Individual messages with publisher confirms |
| Session model | Connection + AMQP session + links | Connection + channels |

### Resilience Behavior

- **Automatic reconnection.** On connection loss or link detach, the
  session reconnects with exponential backoff (1s initial, 30s cap, 25%
  jitter). Links re-create themselves on the next operation.
- **Link lifecycle.** Receivers and senders detect link errors and notify
  the session, which triggers reconnection. After reconnect, `Reconcile`
  runs again if a plan exists.
- **Idempotent settlement.** A `Delivery` can only be settled once. Repeat
  calls to `Ack` or `Retry` are safe no-ops.

### AWS MQ (Managed AMQP 1.0)

AWS MQ for ActiveMQ supports the AMQP 1.0 protocol. Connect using the
broker's AMQP endpoint with TLS:

```yaml
sessions:
  - id: aws-mq-conn
    transport: amqp10
    options:
      address: "amqps://b-xxxx-xxxx.mq.eu-west-1.amazonaws.com:5671"
      container_id: "bridge-aws-mq"
      username: "admin"
      password: "your-password"
      tls:
        enable: true
```

AWS MQ manages broker topology (queues, topics) through the ActiveMQ admin
console or API. The bridge connects as a standard AMQP 1.0 client.

---

## Transport Capabilities Matrix

Each transport declares its capabilities at registration time. The runtime
uses these to validate routes and enable transport-specific features.

| Capability | MQTT | SQS | Azure SB | AMQP 0-9-1 | AMQP 1.0 | HTTP | Description |
|------------|:----:|:---:|:--------:|:----------:|:--------:|:----:|-------------|
| `stateful_session` | Yes | -- | -- | Yes | Yes | -- | Persistent session across reconnects |
| `exclusive_identity` | Yes | -- | -- | -- | -- | -- | Lease-based single-holder session |
| `visibility_extension` | -- | Yes | Yes | -- | -- | -- | Auto-renew message lock / visibility |
| `source_redelivery` | -- | Yes | -- | Yes | -- | -- | Broker redelivers unacknowledged messages |
| `http_endpoint` | -- | -- | -- | -- | -- | Yes | Exposes HTTP endpoints |

Additional capabilities defined in `ports.Capability` but not currently
declared by any built-in transport: `delayed_send`, `shared_consumer`.

---

## Programmatic Registration

Register transport factories on the `Builder` (one-shot) or `Supervisor`
(survives config reloads):

```go
// Builder (one-shot)
rt, err := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
    RegisterTransport("mqtt", paho.NewFactory(logger)).
    RegisterTransport("sqs", sqs.NewFactory(logger)).
    RegisterTransport("servicebus", servicebus.NewFactory(logger)).
    RegisterTransport("amqp091", amqp091.NewFactory(logger)).
    RegisterTransport("amqp10", amqp10.NewFactory(logger)).
    RegisterTransport("http", httptransport.NewFactory(
        httptransport.WithFactoryLogger(logger),
    )).
    RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
    Build(ctx)

// Supervisor (survives hot-reload)
sup := bridge.NewSupervisor()
sup.RegisterTransport("mqtt", paho.NewFactory(logger))
sup.RegisterTransport("sqs", sqs.NewFactory(logger))
sup.RegisterTransport("servicebus", servicebus.NewFactory(logger))
sup.RegisterTransport("amqp091", amqp091.NewFactory(logger))
sup.RegisterTransport("amqp10", amqp10.NewFactory(logger))
sup.RegisterTransport("http", httptransport.NewFactory(
    httptransport.WithFactoryLogger(logger),
))
```

All factories implement the `TransportFactory` interface:

```go
type TransportFactory interface {
    NewSession(ctx context.Context, def config.SessionDef) (ports.Session, error)
    NewReceiver(ctx context.Context, def config.ReceiverDef, session ports.Session) (ports.Receiver, error)
    NewSender(ctx context.Context, def config.SenderDef, session ports.Session) (ports.Sender, error)
    Capabilities() []ports.Capability
}
```

---

## Multi-Transport Example

MQTT sensors to SQS archive with SSE dashboard streaming:

```yaml
bridge:
  id: multi-transport-bridge
http:
  admin_addr: ":8080"
  admin_api_key: "change-me"
sessions:
  - id: mqtt-primary
    transport: mqtt
    session_mode: persistent
    options: { broker_url: "tcp://mqtt.example.com:1883", client_id: "bridge-01" }
receivers:
  - id: mqtt-sensors
    transport: mqtt
    session_id: mqtt-primary
    topics: [{ topic: "sensors/#", qos: 1 }]
senders:
  - id: sqs-archive
    transport: sqs
    options: { queue_name: "sensor-archive", region: "eu-west-1", batch_size: 10 }
  - id: sse-dashboard
    transport: http
    options: { mode: "sse", heartbeat_interval: "15s", max_clients: 50 }
bindings:
  - { id: to-archive, sender_id: sqs-archive, address: "sensor-archive" }
  - { id: to-dashboard, sender_id: sse-dashboard, address: "dashboard-events" }
routes:
  - id: sensor-to-archive
    receiver_id: mqtt-sensors
    bindings: ["to-archive", "to-dashboard"]
    policy: { max_in_flight: 100 }
```
