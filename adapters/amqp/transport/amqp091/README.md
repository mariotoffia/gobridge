# amqp091 — AMQP 0-9-1 Transport Adapter

Transport adapter for RabbitMQ (AMQP 0-9-1) implementing `ports.Session`, `ports.Receiver`, `ports.Sender`, and `ports.BatchSender`.

Uses the [rabbitmq/amqp091-go](https://github.com/rabbitmq/amqp091-go) client library.

## Architecture

A `Session` owns a single AMQP connection with automatic reconnection (exponential backoff, 1 s initial, 30 s max). `Receiver` and `Sender` each open their own AMQP channel from that connection.

```mermaid
graph LR
    subgraph Session
        Conn[Connection]
    end

    subgraph Receiver
        RCh[Channel]
        Consumer
    end

    subgraph Sender
        SCh[Channel]
        Publisher
        Confirm[Publisher Confirm]
    end

    Conn --> RCh
    Conn --> SCh
    RCh --> Consumer
    SCh --> Publisher
    Publisher --> Confirm

    Consumer -->|amqp.Delivery| Delivery
    Delivery -->|Ack| Broker[Broker]
    Delivery -->|Retry| Broker
```

`Reconcile` declares exchanges, queues, and bindings from a `SessionPlan`. It runs on first connect and again after each reconnect.

## Settlement Mapping

| `ports.Delivery` Method | AMQP 0-9-1 Operation | Notes |
|---|---|---|
| `Ack(ctx)` | `delivery.Ack(false)` | Single-message acknowledgement |
| `Retry(ctx, after, reason)` | `delivery.Nack(false, true)` | Requeue immediately; `after` is logged but not enforced (no native delayed redelivery) |
| `Extend(ctx, deadline)` | — | Returns `ErrNotSupported`; AMQP 0-9-1 has no visibility timeout |

Settlement is guaranteed at-most-once via `sync.Once`.

## Header Mapping

### System Properties (ingress and egress)

| AMQP 0-9-1 Property | Envelope Header Key | Go Type |
|---|---|---|
| `MessageId` | `amqp091.message-id` | `string` |
| `CorrelationId` | `amqp091.correlation-id` | `string` |
| `ContentType` | `amqp091.content-type` | `string` |
| `ContentEncoding` | `amqp091.content-encoding` | `string` |
| `ReplyTo` | `amqp091.reply-to` | `string` |
| `Type` | `amqp091.type` | `string` |
| `AppId` | `amqp091.app-id` | `string` |
| `DeliveryMode` | `amqp091.delivery-mode` | `uint8` |
| `Priority` | `amqp091.priority` | `uint8` |
| `Expiration` | `amqp091.expiration` | `string` |
| `Timestamp` | `amqp091.timestamp` | `time.Time` |

### Delivery-only Properties (ingress)

| AMQP 0-9-1 Property | Envelope Header Key | Go Type |
|---|---|---|
| `Exchange` | `amqp091.exchange` | `string` |
| `RoutingKey` | `amqp091.routing-key` | `string` |
| `DeliveryTag` | `amqp091.delivery-tag` | `uint64` |
| `Redelivered` | `amqp091.redelivered` | `bool` |
| `ConsumerTag` | `amqp091.consumer-tag` | `string` |

User-defined AMQP headers pass through directly. Reserved `x-bridge.*` headers are stripped at ingress to prevent injection.

## Error Mapping

| AMQP Code | Name | `domain.BridgeError` | Transient |
|---|---|---|---|
| 320 | connection-forced | `ErrConnectionLost` | yes |
| 403 | access-refused | `ErrNotAuthorized` | no |
| 404 | not-found | `ErrNotFound` | no |
| 405 | not-allowed | `ErrForbidden` | no |
| 406 | not-implemented | `ErrNotSupported` | no |
| 501, 502, 503, 505 | frame/syntax/command/unexpected-frame | `ErrProtocolError` | no |
| 504 | channel-error | `ErrUnavailable` | yes |
| 530 | not-allowed | `ErrForbidden` | no |
| 540 | not-implemented | `ErrNotSupported` | no |
| 541 | internal-error | `ErrUnavailable` | yes |

Network errors and `context.DeadlineExceeded` map to `ErrTimeout`. `context.Canceled` maps to `ErrUnavailable`.

## Configuration Reference

### SessionOptions

| Field | Type | Default | Description |
|---|---|---|---|
| `BrokerURL` | `string` | — (required) | AMQP URI, e.g. `amqp://localhost:5672/` |
| `Username` | `string` | `""` | Injected into broker URL if no userinfo present |
| `Password` | `string` | `""` | Injected into broker URL if no userinfo present |
| `Vhost` | `string` | `""` | AMQP virtual host |
| `Heartbeat` | `time.Duration` | `10s` | Connection heartbeat interval |
| `ConnectTimeout` | `time.Duration` | `30s` | Dial timeout per attempt |
| `ReconnectDelay` | `time.Duration` | `1s` | Initial delay before reconnect (grows exponentially to 30 s) |
| `TLS` | `*TLSConfig` | `nil` | TLS settings; when enabled, uses `amqp091.DialTLS` |

#### TLSConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `Enable` | `bool` | `false` | Activate TLS |
| `CACertFile` | `string` | `""` | Path to CA certificate PEM |
| `CertFile` | `string` | `""` | Path to client certificate PEM |
| `KeyFile` | `string` | `""` | Path to client private key PEM |
| `InsecureSkipVerify` | `bool` | `false` | Skip server certificate verification |

### ReceiverConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `QueueName` | `string` | `""` | Queue to consume from |
| `ConsumerTag` | `string` | `""` | Consumer tag; auto-generated if empty |
| `AutoAck` | `bool` | `false` | Automatic acknowledgement (disables manual settlement) |
| `Exclusive` | `bool` | `false` | Exclusive consumer |
| `PrefetchCount` | `int` | `10` | QoS prefetch count |
| `PrefetchSize` | `int` | `0` | QoS prefetch size in bytes |

### SenderConfig

| Field | Type | Default | Description |
|---|---|---|---|
| `Exchange` | `string` | `""` | Target exchange |
| `RoutingKey` | `string` | `""` | Routing key; falls back to `envelope.Subject` |
| `Mandatory` | `bool` | `false` | Return unroutable messages |
| `Immediate` | `bool` | `false` | Return messages when no consumer ready |
| `Timeout` | `time.Duration` | `30s` | Per-publish timeout (applied when context has no deadline) |

### Options Map Keys

Configuration structs can be built from `map[string]any` via `SessionOptionsFromMap`, `ReceiverConfigFromOptions`, and `SenderConfigFromOptions`. Map keys use snake_case: `broker_url`, `heartbeat`, `connect_timeout`, `reconnect_delay`, `queue_name`, `consumer_tag`, `auto_ack`, `prefetch_count`, `exchange`, `routing_key`, etc.

## Capabilities

The `BridgeFactory` advertises two capabilities:

| Capability | Constant | Meaning |
|---|---|---|
| Stateful session | `ports.CapStatefulSession` | Session maintains a persistent connection with reconnection |
| Source redelivery | `ports.CapSourceRedelivery` | Failed messages can be requeued to the source queue via nack |

## Usage Example

```go
package main

import (
    "context"
    "log/slog"
    "os"

    "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
    "github.com/mariotoffia/gobridge/domain"
    "github.com/mariotoffia/gobridge/ports"
)

func main() {
    ctx := context.Background()
    logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

    // Create and start a session.
    sess := amqp091.NewSession(amqp091.SessionOptions{
        BrokerURL:      "amqp://guest:guest@localhost:5672/",
        Heartbeat:      10 * time.Second,
        ConnectTimeout: 30 * time.Second,
    }, domain.SessionModeReadWrite, logger)

    if err := sess.Start(ctx); err != nil {
        panic(err)
    }
    defer sess.Close(ctx)

    // Declare exchange, queue, and binding.
    err := sess.Reconcile(ctx, domain.SessionPlan{
        Subscriptions: []domain.SubscriptionPlan{{
            Topic:   "my-queue",
            Options: map[string]any{
                "exchange":      "my-exchange",
                "exchange_type": "direct",
                "routing_key":   "events",
                "durable":       true,
            },
        }},
    })
    if err != nil {
        panic(err)
    }

    // Create a sender.
    sender := amqp091.NewSender(amqp091.SenderConfig{
        Exchange:   "my-exchange",
        RoutingKey: "events",
        Session:    sess,
        Logger:     logger,
    })

    // Publish a message.
    _ = sender.Send(ctx, &domain.Envelope{
        ID:      "msg-1",
        Subject: "events",
        Payload: []byte(`{"event":"created"}`),
    })

    // Create a receiver and consume.
    receiver := amqp091.NewReceiver(amqp091.ReceiverConfig{
        QueueName:     "my-queue",
        PrefetchCount: 10,
        Session:       sess,
        Logger:        logger,
    })

    _ = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
        env := del.Envelope()
        logger.Info("received", "id", env.ID, "payload", string(env.Payload))
        return del.Ack(ctx)
    })
}
```

## BridgeFactory

`BridgeFactory` implements `ports.TransportFactory` and creates sessions, receivers, and senders from declarative `config.*Def` definitions. It wraps the lower-level factories (`Factory`, `ReceiverFactory`, `SenderFactory`) for use with the bridge runtime.

```go
factory := amqp091.NewFactory(logger, metricsExporter)
```

## Reconnection

The session runs a background goroutine that monitors `NotifyClose` from the AMQP connection. On disconnect:

1. Emits `SessionDisconnected` event.
2. Retries with exponential backoff (1 s to 30 s) plus jitter.
3. Re-runs `Reconcile` with the stored `SessionPlan`.
4. Emits `SessionConnected` on success.

Receiver detects channel closure and waits for `SessionConnected` or `SessionReconciled` before re-establishing its consumer.

Sender re-opens its channel (with publisher confirms) on the next `Send` call after a channel error.
