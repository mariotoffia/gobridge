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
**Capabilities:** `stateful_session`, `exclusive_identity`, `plan_driven_subscriptions`

MQTT requires a session. Multiple receivers and senders can share one session
(one TCP connection). Session mode controls lifecycle and ownership semantics.

The `options:` block is decoded into the transport's nested typed config: session
connection settings live under an `options.session` sub-block and sender settings
under an `options.sender` sub-block. A flat `options:` block (keys directly under
`options:`) is rejected by the strict decoder.

Because MQTT advertises `plan_driven_subscriptions`, every MQTT receiver
subscribes only when the session manager reconciles the session plan. The bridge
builder therefore fails the build if an MQTT receiver is bound to a session that
never gets a manager (it would otherwise be silently inert, subscribing to
nothing).

### Session Modes

| Mode | `session_mode` | Effective clean-start on the wire | Behavior |
|------|---------------|-----------------------------------|----------|
| Ephemeral | `ephemeral` (default) | always `true` (the `clean_start` option is ignored) | No state survives disconnect |
| Persistent | `persistent` | honours `clean_start` (default `false`) | Broker retains subscriptions and queued messages |
| Exclusive | `exclusive` | honours `clean_start` (default `false`) | Lease-based single holder; requires a lease store |

The `clean_start` option defaults to **`false`** and is consulted only for
Persistent and Exclusive sessions — the modes that exist to *resume* broker
session state. Ephemeral sessions always connect with clean-start regardless of
the option.

### YAML Example

```yaml
sessions:
  - id: mqtt-session-1
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: "tcp://broker.example.com:1883"
        client_id: "bridge-node-01"
        keep_alive: 30
        connect_timeout: "30s"
        reconnect_timeout: "10s"
        reconnect_delay: "5s"
        clean_start: false
        session_expiry_interval: 86400
        receive_maximum: 100
        username: "bridge"
        password: "secret"
        will:
          topic: "bridge/status/node-01"
          payload: "offline"
          qos: 1
          retain: true
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
      sender:
        default_topic: "devices/commands"
        qos: 1
        retain: false
        timeout: "30s"
        throttle_retry_after: "500ms"
```

### Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- | Single broker URL (e.g. `tcp://host:1883`). Folded into `broker_urls` when the list form is absent. |
| `broker_urls` | []string | -- | Multiple broker URLs for failover |
| `client_id` | string | -- (see note) | MQTT client identifier. At least one of `session.client_id`, `session.broker_urls`, or `sender.default_topic` must be set. |
| `keep_alive` | int | `30` | Keep-alive interval in seconds. Explicit `0` disables the pinger. |
| `connect_timeout` | duration | `30s` | Bounds the **initial** Start connection await |
| `reconnect_timeout` | duration | `30s` | Bounds each individual (re)connect attempt (TCP dial + TLS + CONNECT/CONNACK). Maps to autopaho `ConnectTimeout`; `0` → autopaho default (10s). |
| `reconnect_delay` | duration | autopaho default (10s) | Constant delay between failed reconnect attempts after the first immediate retry |
| `clean_start` | bool | `false` | MQTT 5 clean-start flag; consulted only for Persistent/Exclusive sessions |
| `session_expiry_interval` | int | `0` | MQTT 5 session expiry in seconds. For Persistent/Exclusive sessions a `0` is replaced at Start with `86400` (24h) — a literal `0` would give zero offline retention. Ephemeral always uses `0`. |
| `receive_maximum` | int | `0` (paho default 65535) | MQTT 5 Receive Maximum: max in-flight QoS 1/2 messages the broker may send before PUBACKs |
| `username` | string | -- | Authentication username |
| `password` | string | -- | Authentication password (redacted on marshal) |
| `will.topic` | string | -- | Last Will and Testament topic (required when `will` is set; no wildcards) |
| `will.payload` | string | -- | Will message payload |
| `will.qos` | int | `0` | Will QoS (0, 1, or 2) |
| `will.retain` | bool | `false` | Will retain flag |
| `tls.enable` | bool | `false` | Enable TLS |
| `tls.ca_cert_file` | string | -- | CA certificate file path |
| `tls.cert_file` | string | -- | Client certificate file path |
| `tls.key_file` | string | -- | Client private key file path |
| `tls.ca_cert_pem` | string | -- | CA certificate PEM material (in-memory; wins over `ca_cert_file`). Typically supplied by credential rotation. |
| `tls.cert_pem` | string | -- | Client certificate PEM material (requires `key_pem`; wins over `cert_file`) |
| `tls.key_pem` | string | -- | Client private key PEM material (redacted on marshal; requires `cert_pem`) |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

### Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `default_topic` | string | -- | Fallback publish topic used when `OutboundMessage.Address` is empty. The publish topic is never read from `Envelope.Subject`. |
| `qos` | int | `1` | MQTT QoS level (0, 1, or 2) |
| `retain` | bool | `false` | MQTT retain flag |
| `timeout` | duration | `30s` | Per-publish timeout |
| `throttle_retry_after` | duration | `500ms` | Retry-after hint surfaced when the broker signals QoS-0 back-pressure |

### Settlement Semantics

MQTT deliveries are acknowledged **after** the bridge settles them, not on
receipt. The adapter connects with manual acknowledgement and holds the PUBACK
until the downstream send/persist succeeds, so an in-flight message survives a
crash and is redelivered by the broker for Persistent/Exclusive sessions.

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
**Capabilities:** `visibility_extension`, `source_redelivery`, `delayed_send`

SQS is stateless -- no sessions are needed. The `options:` block is flat (keys
directly under `options:`). Each receiver and sender opens its own AWS SDK
client. Receivers support long polling and automatic visibility extension.
Senders support batching, delayed delivery, and FIFO queues.

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
| `max_messages` | int | 10 | Messages per ReceiveMessage call (1--10). **Forced to 1 for FIFO queues** (`.fifo` suffix) so per-`MessageGroupId` order is preserved under the concurrent route runner. |
| `wait_time_seconds` | int | 20 | Long-poll duration in seconds (0--20) |
| `visibility_timeout` | int | 30 | Visibility timeout in seconds (0--43200) |
| `auto_extend` | bool | `true` | Renew visibility at 50% of timeout |
| `sns_unwrap` | bool | `false` | Extract inner message from an SNS-to-SQS wrapper. Only bodies whose JSON `Type` is `Notification` are unwrapped; a raw body is passed through unchanged. |
| `init_timeout` | duration | `30s` | Bounds receiver startup (client creation + queue-URL resolution) |
| `poll_backoff_initial` | duration | `1s` | Starting delay after a failed `ReceiveMessage` call |
| `poll_backoff_max` | duration | `30s` | Maximum delay between poll retries (must be >= `poll_backoff_initial`) |
| `poll_backoff_multiplier` | float | `2.0` | Exponential growth factor for the poll backoff (must be >= 1.0) |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |

Either `queue_url` or `queue_name` must be provided.

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_url` | string | -- | Fully qualified SQS queue URL |
| `queue_name` | string | -- | Logical queue name (resolved at startup) |
| `region` | string | SDK default | AWS region |
| `endpoint` | string | -- | Override endpoint (for LocalStack) |
| `profile` | string | -- | AWS shared-config profile name |
| `delay_seconds` | int | 0 | Delivery delay in seconds (0--900). Backs the `delayed_send` capability. |
| `batch_size` | int | 10 | Messages per SendMessageBatch (1--10) |
| `timeout` | duration | `30s` | Per-call send timeout |
| `message_group_id` | string | -- | Default FIFO message group ID |
| `fifo` | bool | `false` | Opt into per-envelope FIFO groups via the `x-bridge.ordering-key` header |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |

Either `queue_url` or `queue_name` must be provided.

**FIFO build-time rule.** A `.fifo` queue send requires either
`message_group_id` (a default group) or `fifo: true` (per-envelope groups via
the `x-bridge.ordering-key` header). Configuring neither fails the build rather
than letting SQS reject every send at runtime with `MissingParameter`. When
`fifo: true` is set without a default group, a message missing the ordering-key
header is rejected per-message before the SDK call.

### Resilience Behavior

- **Long-poll default.** `wait_time_seconds` defaults to `20` (maximum SQS
  long-poll duration) when not explicitly configured, preventing accidental
  short-polling which causes excessive API costs.
- **Receiver initialization timeout.** SQS receiver startup (client creation,
  queue URL resolution) is bounded by `init_timeout` (default 30s), preventing
  indefinite hangs when AWS credentials or endpoints are unavailable. The
  receiver's `Started()` channel is always closed even when initialization
  fails, so a supervisor awaiting readiness never hangs — the failure surfaces
  as `Run`'s error.
- **Per-poll timeout.** Each `ReceiveMessage` call has an explicit timeout of
  `WaitTimeSeconds + 10` seconds, protecting against network-level stalls
  beyond the SQS long-poll window. Failed polls back off with
  `poll_backoff_initial` → `poll_backoff_max`, growing by
  `poll_backoff_multiplier`.
- **Receive-latency semantics.** `SQSReceiveLatency` measures the *work* portion
  of a long poll — from the message's broker `SentTimestamp` (or poll start) to
  the poll return — so the metric reflects real receive latency rather than
  echoing `wait_time_seconds` on a quiet queue.
- **Message age from broker `SentTimestamp`.** The envelope `CreatedAt` is taken
  from the broker's `SentTimestamp` (exposed as the `sqs.SentTimestamp` header)
  when present, so TTL/expiry policies measure the message's true age including
  queue time, instead of restarting the clock at each hop.
- **Egress attribute priority.** SQS caps a message at 10 attributes and 256 KiB
  (body + attributes). When headers exceed the cap they are dropped
  deterministically by rank: rank 0 essential propagation
  (`traceparent`/`tracestate` and `x-bridge.idempotency-key`) first, rank 1
  application headers next, rank 2 remaining bridge-to-bridge headers
  (`correlation-id`, `causation-id`, `tenant-id`, `forwarded-*`) sacrificed
  first. FIFO ordering/dedup ride the native `MessageGroupId` /
  `MessageDeduplicationId` fields and never consume an attribute slot.
- **Batch error classification.** SQS batch send failures distinguish between
  server faults (transient, retriable) and sender faults (permanent, not
  retriable). Messages with malformed payloads are classified as
  `ErrorRejected` and routed to DLQ instead of being retried indefinitely.
- **Adaptive auto-extend ticker.** When `Extend()` changes the SQS visibility
  timeout, the auto-extend ticker interval updates accordingly, preventing
  excessive or insufficient extend calls.
- **Send timeout vs. visibility window.** With `auto_extend` disabled, the
  builder rejects a route whose policy `send_timeout` is at least half the
  effective `visibility_timeout`: a send that outruns half the window lets SQS
  redeliver the in-flight message before the send finishes, producing
  duplicates. With `auto_extend` on (the default) the check is skipped --
  background renewal holds the message invisible for the whole send, so a short
  window paired with auto-extend is a valid config and is not rejected. The
  check reads the route's own `visibility_timeout` (default 30s), not a
  transport-wide constant. An effective window below 2 seconds runs a fixed,
  non-renewed visibility even under `auto_extend: true`, so the check still
  applies there.

> **Tip:** Set the SQS native DLQ `maxReceiveCount` to at least
> `(bridge max retries + 3)` to prevent SQS from moving messages to the DLQ
> before the bridge has finished its own retry handling.

> **Migration.** The send-timeout check reads the route's own
> `visibility_timeout` (SQS) or `lock_duration` (Service Bus) rather than a
> fixed 30s default. A route that ran `auto_extend: false` with a short window
> and a long `send_timeout` may now fail at build where it passed before. Keep
> `auto_extend` on (the default), raise the window, or lower `send_timeout`.
> For Service Bus, also set `lock_duration` to the broker entity LockDuration
> -- see Azure Service Bus below.

---

## Azure Service Bus

**Transport name:** `servicebus`
**Factory:** `servicebus.NewFactory(logger)`
**Capabilities:** `visibility_extension`

Azure Service Bus is stateless in GoBridge (no bridge-level sessions).
The `options:` block is decoded into a nested typed config: receiver settings
under `options.receiver`, sender settings under `options.sender`, and shared
connection/credential settings under `options.connection`. The transport
supports both queues and topic/subscription patterns, plus Azure SB sessions
for ordered processing.

### YAML Example

```yaml
receivers:
  - id: asb-queue-receiver
    transport: servicebus
    options:
      connection:
        connection_string: "Endpoint=sb://myns.servicebus.windows.net/;SharedAccessKeyName=listen;SharedAccessKey=..."
      receiver:
        queue_name: "orders"
        max_messages: 10
        max_wait_time: "30s"
        receive_mode: "PeekLock"
        lock_duration: "30s"
        auto_extend: true
        max_lock_renewal_duration: "5m"

  - id: asb-topic-receiver
    transport: servicebus
    options:
      connection:
        namespace: "myns.servicebus.windows.net"
        use_managed_identity: true
      receiver:
        topic_name: "events"
        subscription_name: "bridge-sub"
        receive_mode: "PeekLock"
        session_id: "partition-1"

senders:
  - id: asb-queue-sender
    transport: servicebus
    options:
      connection:
        connection_string: "Endpoint=sb://myns.servicebus.windows.net/;SharedAccessKeyName=send;SharedAccessKey=..."
      sender:
        queue_name: "commands"
        batch_size: 10
        timeout: "30s"
        default_session_id: "partition-1"

  - id: asb-topic-sender
    transport: servicebus
    options:
      connection:
        namespace: "myns.servicebus.windows.net"
        tenant_id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
        client_id: "ffffffff-0000-1111-2222-333333333333"
        client_secret: "my-secret"
      sender:
        topic_name: "notifications"
```

### Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Service Bus queue name |
| `topic_name` | string | -- | Service Bus topic name |
| `subscription_name` | string | -- | Subscription on the topic |
| `session_id` | string | -- | Lock to a specific ASB session (cannot combine with `sub_queue`) |
| `max_messages` | int | 10 | Messages per receive call (1--100). Forced to 1 in `ReceiveAndDelete` mode. |
| `max_wait_time` | duration | `30s` | Maximum wait for messages (>= 1s; a bare int decodes as nanoseconds and is rejected) |
| `receive_mode` | string | `PeekLock` | `PeekLock` or `ReceiveAndDelete` (case-insensitive) |
| `sub_queue` | string | -- | `""`, `"deadletter"`, or `"transferdeadletter"` (case-insensitive) |
| `lock_duration` | duration | `30s` | Expected lock duration (for auto-extend). Accepted range 5s--5m; `0` → 30s default. |
| `auto_extend` | bool | `true` | Renew lock at 50% of duration |
| `max_lock_renewal_duration` | duration | `5m` | Caps total wall-clock time a single delivery's lock is auto-renewed. When the cap is hit the delivery's context is cancelled and renewal stops, so a hung pipeline cannot hold a message locked forever. Counted by `ASBLockRenewalCapExceeded`. |

Either `queue_name` or both `topic_name` + `subscription_name` are required.

> **Removed:** the flat `prefetch` key no longer exists. The receiver runs at
> `max_messages` credit with no separate prefetch knob.

### Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Service Bus queue name |
| `topic_name` | string | -- | Service Bus topic name |
| `default_session_id` | string | -- | Default ASB session for messages |
| `batch_size` | int | 10 | Upper bound on messages per batch |
| `timeout` | duration | `30s` | Per-call send timeout (applied per chunk in `SendBatch`) |

Either `queue_name` or `topic_name` is required.

### Connection Options (`options.connection.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `connection_string` | string | -- | Full Service Bus connection string (redacted on marshal) |
| `namespace` | string | -- | Namespace FQDN (token-based auth) |
| `use_managed_identity` | bool | `false` | Use Azure Managed Identity |
| `tenant_id` | string | -- | Azure AD tenant (app auth) |
| `client_id` | string | -- | Azure AD app client ID |
| `client_secret` | string | -- | Azure AD app client secret (redacted on marshal) |
| `ca_pem` | string | -- | Custom CA certificate (PEM string) |
| `client_cert_pem` | string | -- | Client certificate PEM for mutual TLS (requires `client_key_pem`) |
| `client_key_pem` | string | -- | Client private key PEM for mutual TLS (redacted; requires `client_cert_pem`) |
| `insecure_skip_verify` | bool | `false` | Skip TLS server verification |

A top-level `options.credentials_uri` resolves connection material from the
bridge credential store at build time. Either `connection_string` or `namespace`
is required.

### Retry Wire Semantics

Service Bus has no native delayed-redelivery for a scheduled retry that resets
the broker `DeliveryCount`, so a delayed `Retry` schedules a fresh copy of the
message and stamps two reserved application properties on that copy:

- `x-bridge.retry-attempt` — the accumulated 1-based receive count at schedule
  time. Ingress adds it to the broker `DeliveryCount` so the runtime's
  `MaxReplayAttempts` gate and the broker's `MaxDeliveryCount` still fire; a
  poison message cannot ping-pong forever.
- `x-bridge.original-message-id` — the first delivery's `MessageID`, restored as
  the envelope ID on ingress so end-to-end dedup still sees one logical message.

The scheduled copy's own `MessageID` is **salted** with the attempt number so
broker duplicate detection never silently discards a scheduled retry. Both
reserved properties are stripped at ingress before headers reach the envelope,
so an external producer cannot inject them.

### Credential Rotation

When credentials rotate, the receiver/sender swaps to a freshly built client
atomically — in-flight operations finish against the old client and new
operations use the new one; there is no window where the transport has no
client.

### Resilience Behavior

- **`lock_duration` is a client-side mirror.** It does not configure the
  broker; the queue or subscription entity carries the authoritative
  LockDuration. The receiver uses `lock_duration` only to seed the auto-extend
  renewal cadence (half the value); once a message arrives its broker
  `LockedUntil` deadline governs renewal. The accepted range is 5s--5m, and `0`
  resolves to the 30s default.
- **Send timeout vs. lock window.** The builder applies the same send-timeout
  check described under AWS SQS, using `lock_duration` as the window. With
  `auto_extend` on (the default) the check is skipped. With `auto_extend: false`
  the builder judges `send_timeout` against the *declared* `lock_duration`, not
  the broker's real lock -- a declared-short `lock_duration` is rejected at
  build even when the broker entity permits a longer lock. Set `lock_duration`
  to match the broker entity LockDuration so the declared window reflects what
  the broker enforces.

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
information leaks. A `401` always carries an RFC 7235
`WWW-Authenticate: Bearer` challenge.

**Minimum key length (breaking).** An inline `api_key` shorter than 16
characters is rejected at decode time (`minAPIKeyLength`). A short key an
earlier build accepted must be lengthened to >= 16 characters.
Credential-resolved keys (via `credentials_uri`) are validated at the
credential layer and not re-checked here.

> **The `api_key` and the cluster forward token MUST be distinct secrets.**
> Every client presents `api_key` on each request; reusing it as the forward
> token would let any authenticated caller spoof `X-Bridge-Forwarded`. Provision
> two independent secrets.

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
| `path` | string | `/transport/http/receivers/{id}/messages` | POST endpoint path (literal mount point; no ServeMux `{}` metacharacters) |
| `api_key` | string | -- | Per-receiver API key (constant-time comparison; inline keys >= 16 chars) |
| `max_body_size` | int | 1048576 (1 MiB) | Maximum request body in bytes; a breach returns `413` |
| `dedup_window` | int | 4096 | Size of the node-local ingress idempotency LRU (remembered `Idempotency-Key` / `X-Dedup-Id` values) |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time (populates `api_key` when empty) |

### Receiver Request Format

The receiver accepts a single JSON POST value (trailing tokens are rejected with
`400`) with the following fields:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `subject` | string | **yes** | Logical event subject. Mapped 1:1 to `Envelope.Subject`. Not a topic or routing key. |
| `payload` | any JSON | no | Message content (stored as raw bytes) |
| `id` | string | no | Caller-provided message ID (auto-assigned a UUID when omitted) |
| `headers` | object | no | Custom metadata (reserved `x-bridge.*` keys stripped at ingress) |
| `expires_at` | RFC 3339 | no | Message TTL (drives `on_expired` policy) |

**First-class propagation headers.** The idempotency, dedup, and ordering keys
are accepted *only* through their dedicated non-reserved HTTP request headers and
re-stamped on the trusted side; a client cannot inject them via the reserved
`x-bridge.*` namespace (stripped at ingress):

| Header | Purpose |
|--------|---------|
| `Idempotency-Key` | Cross-hop identity/dedup key; feeds the ingress idempotency window and rides forwards |
| `X-Dedup-Id` | Alternative dedup key remembered by the ingress window |
| `X-Ordering-Key` | Propagated as envelope metadata for ordered *targets* (FIFO queues); HTTP ingress itself never orders |

### Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `mode` | string | `sse` | Sender mode (only `sse` supported) |
| `path` | string | `/transport/http/senders/{id}/events` | GET endpoint path |
| `heartbeat_interval` | duration | `30s` | SSE keep-alive heartbeat interval |
| `write_timeout` | duration | `15s` | Per-frame SSE write deadline (re-armed before every frame; overrides a fronting server's global `WriteTimeout` and evicts a stalled subscriber) |
| `api_key` | string | -- | Per-sender API key (constant-time comparison; inline keys >= 16 chars) |
| `max_clients` | int | 10000 | Maximum concurrent SSE connections (no uncapped mode) |
| `redirect_endpoint` | string | -- (disabled) | `PeerInfo.Endpoints` key used to build a `307` redirect for a remote-owned route. Empty **disables** redirect (remote route → `503`) so an internal peer endpoint is never leaked to an SSE client. |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |

### Resilience & Delivery Semantics

- **Content-Type validation.** HTTP ingress validates
  `Content-Type: application/json` when the header is present, returning
  `415 Unsupported Media Type` for non-JSON requests. Requests without a
  `Content-Type` header are accepted (assumed JSON).
- **Body limit → 413.** A body exceeding `max_body_size` returns
  `413 Request Entity Too Large`, distinct from the `400` used for malformed
  JSON or trailing tokens.
- **Automatic envelope ID.** HTTP ingress generates a unique UUID envelope ID
  when the request omits `id`, ensuring every message has a traceable
  identifier through the pipeline.
- **Ingress is at-least-once with a best-effort dedup window.** Each receiver
  keeps a bounded, node-local LRU of `Idempotency-Key` / `X-Dedup-Id` values of
  *successfully* processed requests (`dedup_window`, default 4096). A request
  presenting a remembered key is acknowledged `200` without re-emitting and
  counted on `HTTPIngressDuplicates`. Keys are recorded only after success, so a
  retry after a `5xx` is re-processed. The window is per-node/per-process and
  not persisted -- it bounds but does not eliminate duplicates.
- **Emit concurrency (deviation).** Unlike the default sequential emit contract,
  each in-flight request emits from its own handler goroutine. The downstream
  pipeline observes concurrent emits and **no ordering guarantee exists -- not
  even for requests sharing an `X-Ordering-Key`**. The ordering key is metadata
  for ordered targets only.
- **Dispatch decoupled from the client connection.** Once the body is converted
  to an envelope, the delivery is emitted on a `context.WithoutCancel` copy of
  the request context: a client disconnect cannot abort the pipeline
  mid-dispatch. The HTTP response still honours the client context (`504` on
  timeout/disconnect) -- a `504` means "outcome unknown", not "not processed".
  Producers that retry on `504` should supply `Idempotency-Key`.
- **Cluster forwarding is at-least-once.** A forward timeout after the peer
  received the body is retried as a fresh POST. Every forward carries an
  `Idempotency-Key` (the envelope's own or derived from its ID) so the peer's
  idempotency window absorbs the replay. Forward classification: `5xx`, `429`
  (`ErrThrottled`) and `408` (`ErrTimeout`) are transient and honour a bounded
  `Retry-After` hint (clamped to 30s); other `4xx` are permanent. An optional
  circuit breaker (`ForwarderConfig.Breaker`) makes a dead peer fail fast and
  emits `HTTPForwardBreakerOpen`.
- **Loop prevention.** A request carrying `X-Bridge-Forwarded: true` is trusted
  as an already-forwarded peer message (and processed locally) **only** when it
  also proves the shared `X-Bridge-Forward-Token` (constant-time compared). With
  no token configured the marker is never trusted, so a client cannot force
  local processing on a non-owner node. An already-forwarded request for a route
  this node does not own is refused with `508 Loop Detected`
  (`HTTPForwardLoopRefused`) -- neither processed nor re-forwarded -- so even an
  untokened cluster fails closed instead of entering an A->B->A loop.
- **SSE egress is at-most-once.** `Send` reports success even when zero
  subscribers are connected or every subscriber's buffer is full -- the route
  runner acks the source either way. Both zero-delivery cases are counted
  (`SSENoSubscribers` / `SSEAllDropped`) and logged. Stronger delivery requires
  fronting SSE egress with an outbox/DLQ route policy.
- **SSE frames carry no `id:` field.** Emitting one would make `EventSource`
  clients send `Last-Event-ID` on reconnect and expect a replay window that does
  not exist. The envelope ID remains in the JSON payload.
- **SSE egress header hygiene.** Internal-only reserved headers (`route-id`,
  `route-override`, `source-id`, `content-type`) are stripped before an envelope
  is serialised to a subscriber. Bridge-to-bridge propagated headers
  (correlation/causation/idempotency/tenant/trace/forwarded-*) and application
  headers pass through -- **if any SSE endpoint is publicly reachable, strip the
  `x-bridge.*` namespace plus `traceparent`/`tracestate` at the edge** or keep it
  internal, or that metadata leaks to external clients.
- **SSE cross-cluster redirect is opt-in.** A remote-owned route is refused with
  `503` by default; a `307` is emitted only when `redirect_endpoint` names a
  `PeerInfo.Endpoints` key, so the internal forwarder endpoint is never leaked in
  a `Location` header.
- **Forward error classification (route policy).** Forwarder responses map by
  status family: `5xx` → `ErrUnavailable` (transient, retriable), `4xx` →
  `ErrForwardFailed` (permanent, not retriable), enabling correct DLQ routing.
- **Graceful shutdown order.** `Factory.Close` fans out to every SSE sender,
  unblocking connected client handlers so `http.Server.Shutdown` can complete.
  **The composition root must call `Factory.Close` before server shutdown**;
  otherwise `Shutdown` blocks until clients disconnect on their own.

### Forwarder Configuration (`ForwarderConfig`)

Cluster forwarding is configured by the composition root, not the YAML
`options:` block. Defaults (`DefaultForwarderConfig`):

| Field | Default | Description |
|-------|---------|-------------|
| `Timeout` | `30s` | Per-forward request timeout |
| `IdleConnTimeout` | `90s` | Transport idle connection timeout |
| `MaxRetries` | `2` | Forward retry attempts |
| `RetryInitialDelay` | `100ms` | First retry backoff |
| `RetryMaxDelay` | `200ms` | Retry backoff ceiling |
| `MaxIdleConnsPerHost` | `32` | Transport idle conns per peer |
| `MaxConnsPerHost` | `64` | Transport max conns per peer |
| `ForwardToken` | -- | Shared secret sent as `X-Bridge-Forward-Token` (must match receiver `WithForwardToken`) |
| `ReceiverAPIKeys` | -- | Per-receiver-ID API keys used when forwarding to protected peers |
| `Breaker` | -- | Optional `ports.CircuitBreaker` gating each forward |
| `Metrics` | no-op | Receives `HTTPForwardBreakerOpen` |

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
**Capabilities:** `stateful_session`, `source_redelivery`, `plan_driven_subscriptions`

RabbitMQ uses a stateful session. A `Session` owns a single AMQP connection
with automatic reconnection. Receivers and senders each open their own
channel on that connection. `Reconcile` declares exchanges, queues, and
bindings from the `SessionPlan`. The `options:` block is nested: session
settings under `options.session`, receiver settings under `options.receiver`,
sender settings under `options.sender`, per-topic declarations under
`options.subscription`, and publisher topology under `options.publisher`.

> **Plan-driven subscriptions.** amqp091 receivers subscribe (queue-declare +
> bind + consume) *only* when the session manager reconciles the `SessionPlan`.
> A receiver on an unmanaged session is silently inert, so the builder requires
> a session manager for amqp091 receivers.

### YAML Example

```yaml
sessions:
  - id: rabbit-conn
    transport: amqp091
    options:
      session:
        broker_url: "amqp://guest:guest@localhost:5672/"
        heartbeat: "10s"
        connect_timeout: "30s"
        reconnect_delay: "1s"
        reconnect_max_delay: "30s"
        reconnect_multiplier: 2.0
        tls:
          enable: false

receivers:
  - id: order-consumer
    transport: amqp091
    session_id: rabbit-conn
    topics:
      - topic: "order-queue"
        options:
          subscription:
            exchange: "orders"
            exchange_type: "direct"
            routing_key: "new-order"
            durable: true
    options:
      receiver:
        queue_name: "order-queue"
        prefetch_count: 20

senders:
  - id: notification-publisher
    transport: amqp091
    session_id: rabbit-conn
    options:
      sender:
        exchange: "notifications"
        routing_key: "order.confirmed"
        delivery_mode: "persistent"
        timeout: "30s"
```

### Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `broker_url` | string | -- (required) | AMQP URI, e.g. `amqp://user:pass@host:5672/vhost` |
| `username` | string | -- | Explicit credential -- **overrides** any userinfo in `broker_url` (precedence note below) |
| `password` | string | -- | Explicit credential -- overrides `broker_url` userinfo |
| `vhost` | string | -- | AMQP virtual host |
| `heartbeat` | duration | `10s` | Connection heartbeat interval |
| `connect_timeout` | duration | `30s` | Dial timeout per attempt |
| `reconnect_delay` | duration | `1s` | Initial delay before reconnect |
| `reconnect_max_delay` | duration | `30s` | Reconnect backoff ceiling |
| `reconnect_multiplier` | float | `2.0` | Reconnect backoff growth factor |
| `tls.enable` | bool | `false` | Enable TLS (`amqp091.DialTLS`) |
| `tls.ca_cert_file` | string | -- | CA certificate PEM file path |
| `tls.cert_file` | string | -- | Client certificate PEM file path |
| `tls.key_file` | string | -- | Client private key PEM file path |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

> **Credential precedence.** An explicitly configured `username`/`password`
> (or credential-store material) **overrides** userinfo embedded in
> `broker_url`. A URL carrying stale userinfo paired with an explicit
> `username` connects as the explicit credential, so a rotated secret is not
> silently defeated by stale URL userinfo.

### Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Queue to consume from |
| `consumer_tag` | string | auto-generated | Consumer tag identifier |
| `exclusive` | bool | `false` | Exclusive consumer |
| `prefetch_count` | int | `10` | QoS prefetch count. `0` maps to the bounded default (10), **not** unlimited -- an unbounded window lets the broker hand the whole queue to one consumer. Set an explicit positive value for a larger window. |
| `prefetch_size` | int | `0` | QoS prefetch size in bytes |

> **Removed:** `auto_ack` is rejected (`auto_ack: true` fails validation --
> broker auto-ack acks on delivery and silently drops messages when a
> downstream step fails; the bridge settles only after the send/persist
> succeeds).

### Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `exchange` | string | `""` | Target exchange name |
| `routing_key` | string | `""` | Routing key. When empty the sender uses `OutboundMessage.Address` (resolved from the dispatch plan). The routing key is **never** taken from `Envelope.Subject`. |
| `delivery_mode` | string | `persistent` | `persistent` (AMQP delivery-mode 2, survives a broker restart on a durable queue) or `transient` (delivery-mode 1, lost on restart). A per-message `amqp091.delivery-mode` header overrides it; quorum queues persist regardless. Empty/invalid resolves to `persistent`. |
| `mandatory` | bool | `false` | Return unroutable messages |
| `timeout` | duration | `30s` | Per-publish timeout (applied when context has no deadline) |

> **Removed:** `immediate` is rejected (`immediate: true` fails validation --
> RabbitMQ removed `basic.publish` `immediate` in 3.0 and closes the channel
> when it is set).

### Topology Declaration (Reconcile)

AMQP 0-9-1 sessions support automatic topology declaration via `Reconcile`.
When a receiver has `topics[]` entries with `options`, the session declares
the exchange, queue, and binding during startup and after each reconnect.

```yaml
topics:
  - topic: "my-queue"
    options:
      subscription:
        exchange: "my-exchange"
        exchange_type: "direct"
        routing_key: "events"
        durable: true
```

This declares:

1. Exchange `my-exchange` of type `direct` (durable)
2. Queue `my-queue` (durable)
3. Binding from `my-exchange` to `my-queue` with routing key `events`

#### Publisher-side exchange auto-declare (best-effort)

A sender's target exchange is also pre-declared during `Reconcile`, so a route
that publishes to an exchange the bridge owns works without a separate
provisioning step:

```yaml
senders:
  - id: notification-publisher
    transport: amqp091
    session_id: rabbit-conn
    options:
      sender:
        exchange: "notifications"      # names the exchange to declare + publish to
        routing_key: "order.confirmed"
      publisher:                       # optional: topology for that exchange
        exchange_type: "topic"
        durable: true
```

Two rules govern the declaration:

- **`sender.exchange` names the exchange; the `publisher.*` block only
  decorates it.** The declared name is always `sender.exchange` (the exact
  exchange `Send` publishes to). `exchange_type`, `durable`, `auto_delete`, and
  `exchange_arguments` come from the separate `publisher.*` block. With
  `sender.exchange` set and no `publisher.*` block, the exchange is declared with
  defaults (`direct`, non-durable). `publisher.exchange` is **not** a second name
  and is ignored by the declare.
- **The declare is best-effort — it never takes a route down.** If the broker
  rejects it (`PRECONDITION_FAILED` when the exchange already exists with
  different topology, or `ACCESS_REFUSED` under least-privilege credentials that
  lack `configure`), the session logs a warning, increments
  `AMQP091PublisherDeclareFailed`, and continues. Publishing still works when the
  exchange already exists; a genuinely-absent exchange the bridge cannot create
  instead fails visibly at publish time (404 → retry/DLQ). This makes it safe to
  point a sender at an externally-managed exchange without pre-declaring a
  matching `publisher.*` topology.

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
**Capabilities:** `stateful_session`, `source_redelivery`

The AMQP 1.0 adapter works with any broker that speaks the AMQP 1.0 wire
protocol: Apache ActiveMQ Artemis, Solace PubSub+, Apache Qpid, Azure
Event Hubs (direct AMQP), and AWS MQ for ActiveMQ. Built on
[github.com/Azure/go-amqp](https://github.com/Azure/go-amqp).

A `Session` owns the TCP connection and AMQP session. Receivers and senders
create links on that session. Topology (queues, topics) is broker-managed --
AMQP 1.0 does not declare exchanges or queues. The `options:` block is nested:
session settings under `options.session`, receiver settings under
`options.receiver`, sender settings under `options.sender`.

### YAML Example

```yaml
sessions:
  - id: artemis-conn
    transport: amqp10
    options:
      session:
        address: "amqp://localhost:5672"
        container_id: "bridge-node-01"
        connect_timeout: "30s"
        reconnect_delay: "1s"
        reconnect_max_delay: "30s"
        reconnect_multiplier: 2.0
        idle_timeout: "2m"
        max_frame_size: 65536
        sasl_mechanism: "plain"
        username: "admin"
        password: "admin"

receivers:
  - id: order-receiver
    transport: amqp10
    session_id: artemis-conn
    options:
      receiver:
        address: "queue://orders"
        link_credit: 20
        durability_mode: 0

senders:
  - id: event-publisher
    transport: amqp10
    session_id: artemis-conn
    options:
      sender:
        address: "topic://events"
        timeout: "30s"
        durability_mode: 0
        durable: true
```

### Session Options Reference (`options.session.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | SINGLE broker URL to dial, e.g. `amqp://host:5672` or `amqps://host:5671`. No client-side broker list / failover -- resolve to a load balancer or DNS for HA. |
| `container_id` | string | -- | AMQP container ID identifying this client. **Must be unique per replica** -- two replicas sharing a container ID collide on the broker (esp. for durable subscriptions, which are keyed by container-id + link name). |
| `connect_timeout` | duration | `30s` | Dial timeout per connection attempt |
| `reconnect_delay` | duration | `1s` | Initial delay before reconnect |
| `reconnect_max_delay` | duration | `30s` | Reconnect backoff ceiling |
| `reconnect_multiplier` | float | `2.0` | Reconnect backoff growth factor |
| `idle_timeout` | duration | `2m` | Connection idle timeout sent to broker |
| `max_frame_size` | int | `65536` | Maximum AMQP frame size in bytes |
| `link_close_timeout` | duration | `5s` | Deadline for closing a link/session during cleanup |
| `connection_monitor_fallback` | duration | `30s` | Fallback liveness re-check cadence (real disconnects use `Conn.Done()` immediately) |
| `sasl_mechanism` | string | `""` | `""` (PLAIN when `username` set, else no SASL), `plain`, `external` (mTLS client cert), or `anonymous` |
| `username` | string | -- | SASL PLAIN username |
| `password` | string | -- | SASL PLAIN password |
| `tls.enable` | bool | `false` | Enable TLS |
| `tls.ca_cert_file` | string | -- | CA certificate PEM file path |
| `tls.cert_file` | string | -- | Client certificate PEM file path |
| `tls.key_file` | string | -- | Client private key PEM file path |
| `tls.insecure_skip_verify` | bool | `false` | Skip server certificate verification |

### Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Queue or topic address to consume from |
| `link_credit` | int | `10` | Prefetch credit; how many messages the broker sends ahead |
| `durability_mode` | int | `0` | AMQP terminus durability for the receiver link (`0` none, `1` configuration, `2` unsettled-state). `> 0` makes the subscription **durable**. |
| `subscription_name` | string | -- | Pins the AMQP link name so a durable subscription survives reconnects. When empty and `durability_mode > 0`, a stable name is derived from `container_id` + `address`. |
| `routing` | string | `anycast` | `anycast` or `multicast` (Artemis routing type; case-insensitive) |

> **Durable subscription identity.** A durable subscription
> (`durability_mode > 0`) is identified by **container-id + link name**. A
> stable link name is required for the broker to *resume* an existing
> subscription rather than orphaning it and creating a new one on every
> reconnect -- set `subscription_name` (or rely on the derived
> `container_id`+`address` name) and keep `container_id` stable and unique per
> replica.

### Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `address` | string | -- (required) | Target address to publish to |
| `timeout` | duration | `30s` | Send timeout per message |
| `durability_mode` | int | `0` | AMQP terminus durability for the sender link |
| `durable` | bool | `true` | Sets the AMQP message `durable` header on outbound messages. **Unset defaults to `true` (persistent)** -- set `false` to opt into non-persistent (faster, lost on broker restart) sends. |
| `routing` | string | `anycast` | `anycast` or `multicast` (Artemis routing type; case-insensitive) |

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
- **Delayed retry is broker-controlled.** A `Retry(ctx, after>0, err)` cannot
  hold the message client-side. The adapter settles with `ModifyMessage`
  (`DeliveryFailed=true`) and attaches the `x-opt-delivery-time` message
  annotation (absolute ms-epoch) asking the broker to schedule redelivery; the
  broker owns the timing. Brokers that ignore the annotation redeliver
  immediately -- the deferral is counted on `AMQP10DelayedRetryUnhonored`.

### AWS MQ (Managed AMQP 1.0)

AWS MQ for ActiveMQ supports the AMQP 1.0 protocol. Connect using the
broker's AMQP endpoint with TLS:

```yaml
sessions:
  - id: aws-mq-conn
    transport: amqp10
    options:
      session:
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
| `plan_driven_subscriptions` | Yes | -- | -- | Yes | -- | -- | Subscribes only when the session manager reconciles the plan |
| `visibility_extension` | -- | Yes | Yes | -- | -- | -- | Auto-renew message lock / visibility |
| `source_redelivery` | -- | Yes | -- | Yes | Yes | -- | Broker redelivers unacknowledged messages |
| `delayed_send` | -- | Yes | -- | -- | -- | -- | Native delayed delivery (SQS `delay_seconds`) |
| `http_endpoint` | -- | -- | -- | -- | -- | Yes | Exposes HTTP endpoints |

Additional capabilities defined in `ports.Capability` but not currently
declared by any built-in transport: `shared_consumer`.

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

HTTP webhook ingress fanned out to an SQS archive, MQTT device commands, and an
SSE dashboard -- three transports on one bridge:

```yaml
bridge:
  id: multi-transport-bridge
http:
  admin_addr: ":8080"
  admin_api_key: "change-me-please-16"
sessions:
  - id: mqtt-primary
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: "tcp://mqtt.example.com:1883"
        client_id: "bridge-01"
receivers:
  - id: webhook-in
    transport: http
    options:
      path: "/transport/http/receivers/webhook-in/messages"
      api_key: "webhook-key-min-16ch"
senders:
  - id: sqs-archive
    transport: sqs
    options: { queue_name: "event-archive", region: "eu-west-1", batch_size: 10 }
  - id: mqtt-commands
    transport: mqtt
    session_id: mqtt-primary
    options:
      sender:
        default_topic: "devices/commands"
        qos: 1
  - id: sse-dashboard
    transport: http
    options: { mode: "sse", heartbeat_interval: "15s", max_clients: 50 }
bindings:
  - id: to-archive
    sender_id: sqs-archive
    address: "event-archive"
  - id: to-commands
    sender_id: mqtt-commands
    address: "devices/commands"
  - id: to-dashboard
    sender_id: sse-dashboard
    address: "dashboard-events"
routes:
  - id: webhook-fanout
    receiver_id: webhook-in
    bindings: ["to-archive", "to-commands", "to-dashboard"]
    policy: { max_in_flight: 100 }
```

> **A binding's `address:` is its per-destination target.** Each binding names
> a sender (`sender_id`) and an `address` -- the transport-level destination
> (SQS queue, MQTT topic, AMQP routing key/link address) that the runtime
> propagates as `OutboundMessage.Address`, overriding the sender's default for
> that fan-out leg. Transport connection/behaviour settings live on the sender
> only; a binding needs no `options:` block (`DestinationBinding.Config` is not
> consumed at runtime), so bindings are bare across every transport.
