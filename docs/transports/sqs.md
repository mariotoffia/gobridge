# AWS SQS

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `sqs`
**Factory:** `sqs.NewFactory(logger)`
**Capabilities:** `visibility_extension`, `source_redelivery`, `delayed_send`

SQS is stateless -- no sessions are needed. The `options:` block is flat (keys
directly under `options:`). Each receiver and sender opens its own AWS SDK
client. Receivers support long polling and automatic visibility extension.
Senders support batching, delayed delivery, and FIFO queues.

## YAML Example

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

## Receiver Options Reference

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

## Sender Options Reference

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

**Address validation is deliberately lenient.** Parse-time `Validate` checks only
field ranges and consistency — it does not require a queue reference, so a config
naming a wrong or arbitrary queue URL parses cleanly
(`adapters/aws/transport/sqs/config_plugin.go:87-123`). Queue identity is
enforced later: `ValidateQueue` at build time (`config_plugin.go:129-134`) and
the sender's `ValidateAddress`, which is offline and structural — a name-only
sender accepts any URL whose trailing segment matches the queue name, so a wrong
region or account passes the build and is only caught at first `Send` once the
canonical URL resolves (`sender.go:124-150`). Do not rely on config parse errors
to catch queue-URL typos.

## Resilience Behavior

- **Long-poll default.** `wait_time_seconds` defaults to `20` (maximum SQS
  long-poll duration) when not explicitly configured, preventing accidental
  short-polling which causes excessive API costs.
- **Receiver initialization timeout.** SQS receiver startup (client creation,
  queue URL resolution) is bounded by `init_timeout` (default 30s), preventing
  indefinite hangs when AWS credentials or endpoints are unavailable. An
  initialization failure returns `Run`'s error **without** closing the receiver's
  `Started()` channel, so a readiness probe never briefly observes a ready route
  for a receiver that failed to start. Supervise on `Run`'s returned error, not
  on `Started()` alone (`adapters/aws/transport/sqs/receiver.go:105-108`).
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

