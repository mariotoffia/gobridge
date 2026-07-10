# AWS SQS

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `sqs`
**Factory:** `sqs.NewFactory(logger)`
**Capabilities:** `visibility_extension`, `source_redelivery`, `delayed_send`

SQS is stateless -- no sessions are needed. The `options:` block is flat (keys
directly under `options:`). Each receiver and sender opens its own AWS SDK
client. Receivers support long polling and automatic visibility extension.
Senders support delayed delivery and FIFO queues. Route dispatch is
**per-message**: the runtime calls `Send` once per envelope, so `batch_size`
governs only the direct `SendBatch` API (callers that hand the sender a slice of
messages) — it does not batch a route's traffic.

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
| `max_messages` | int | 10 | Messages per ReceiveMessage call (1--10). An explicit `0` is rejected by the plugin decoder -- omit the key for the default of 10. **Forced to 1 for FIFO queues** (`.fifo` suffix) so per-`MessageGroupId` order is preserved under the concurrent route runner. |
| `wait_time_seconds` | int | 20 | Long-poll duration in seconds (1--20). An explicit `0` (short-polling) is rejected by the plugin decoder -- omit the key for the 20s long-poll default. |
| `visibility_timeout` | int | 30 | Visibility timeout in seconds (0--43200) |
| `auto_extend` | bool | `true` | Renew visibility at a third of the timeout (a tick at `visibility_timeout/3`, floored at 1s). Ticking at a third rather than half leaves margin: a retry at the next tick still lands before the window lapses after one transient extend failure. |
| `sns_unwrap` | bool | `false` | Extract inner message from an SNS-to-SQS wrapper. Only bodies whose JSON `Type` is `Notification` **and** whose `TopicArn` is non-empty are unwrapped; a raw body is passed through unchanged. The bridge cannot verify the wrapper genuinely came from SNS — that guarantee is the queue policy restricting `sqs:SendMessage` to the topic (operator responsibility). |
| `init_timeout` | duration | `30s` | Bounds receiver startup (client creation + queue-URL resolution) |
| `poll_backoff_initial` | duration | `1s` | Starting delay after a failed `ReceiveMessage` call |
| `poll_backoff_max` | duration | `30s` | Maximum delay between poll retries (must be >= `poll_backoff_initial`) |
| `poll_backoff_multiplier` | float | `2.0` | Exponential growth factor for the poll backoff (must be >= 1.0) |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |
| `poison_max_receives` | int | `0` (disabled) | Adapter-enforced backstop for malformed ("poison") messages the receiver cannot convert. When `> 0` and a poison message's `ApproximateReceiveCount` reaches it, the receiver **deletes** the message to break an otherwise-unbounded redelivery loop, emitting `SQSPoisonDropped`. The delete **drops the message (no DLQ copy)**, so it is subject to two enforced guards (below): it must be `>= 2` unless `poison_drop_without_dlq` is set, and it must be **strictly greater** than any native `maxReceiveCount` (verified at startup) so native redrive — which *preserves* the payload — always wins. A native redrive policy remains the preferred loss-preventing mechanism. |
| `poison_drop_without_dlq` | bool | `false` | Explicit opt-in for the single most destructive backstop setting, `poison_max_receives: 1` (delete on the **first** conversion failure — no redelivery, no DLQ copy). Without it, `poison_max_receives == 1` is rejected at config time. It does **not** relax the startup guard that rejects a backstop preempting an existing native redrive policy. |

Either `queue_url` or `queue_name` must be provided.

> **Startup redrive validation (enforced).** On startup the receiver performs a
> best-effort `GetQueueAttributes` read of the source queue's native redrive
> policy (`maxReceiveCount` → DLQ):
>
> - **Backstop off** (`poison_max_receives: 0`): a queue with **no** redrive
>   policy is surfaced as the `SQSMissingRedrivePolicy` metric and a warning;
>   a queue with one is silent.
> - **Backstop on** (`poison_max_receives > 0`): the receiver **refuses to
>   start** (`INVALID_CONFIG`) when a native redrive policy is readable and
>   `poison_max_receives <= maxReceiveCount`. In that range the adapter's
>   destructive `DeleteMessage` fires *before* SQS can move the message to the
>   DLQ, so it would **silently pre-empt the DLQ and lose the payload**. Raise
>   `poison_max_receives` above `maxReceiveCount`, or set it to `0`.
>
> The check is **permission-gated and never fails startup for a permission
> reason**: a missing `sqs:GetQueueAttributes` grant degrades to a log (a
> loud warning when the backstop is on, since the destructive setting could
> not be verified against native redrive). It only fails startup for the
> definitive `poison_max_receives <= maxReceiveCount` conflict above.

## Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_url` | string | -- | Fully qualified SQS queue URL |
| `queue_name` | string | -- | Logical queue name (resolved at startup) |
| `region` | string | SDK default | AWS region |
| `endpoint` | string | -- | Override endpoint (for LocalStack) |
| `profile` | string | -- | AWS shared-config profile name |
| `delay_seconds` | int | 0 | Delivery delay in seconds (0--900). Backs the `delayed_send` capability. **Rejected on FIFO queues** -- see the FIFO delay rule below. |
| `batch_size` | int | 10 | Messages per SendMessageBatch (1--10) |
| `timeout` | duration | `30s` | Per-call send timeout |
| `message_group_id` | string | -- | Default FIFO message group ID |
| `fifo` | bool | `false` | Opt into per-envelope FIFO groups via the `x-bridge.ordering-key` header |
| `max_message_bytes` | int | 262144 | Message-size ceiling in bytes (body + attributes). Raise to match a queue whose `MaximumMessageSize` is provisioned above 256 KiB; `0` keeps the 256 KiB default. |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |

Either `queue_url` or `queue_name` must be provided.

**FIFO build-time rule.** A `.fifo` queue send requires either
`message_group_id` (a default group) or `fifo: true` (per-envelope groups via
the `x-bridge.ordering-key` header). Configuring neither fails the build rather
than letting SQS reject every send at runtime with `MissingParameter`. When
`fifo: true` is set without a default group, a message missing the ordering-key
header is rejected per-message before the SDK call.

**FIFO rejects per-message delay.** `delay_seconds > 0` on a FIFO queue fails the
build: AWS refuses per-message `DelaySeconds` on a FIFO `SendMessage` /
`SendMessageBatch`, so every send would DLQ at runtime as `ErrInvalidPayload`.
FIFO is detected from the explicit `fifo: true` flag, a default
`message_group_id`, or the `.fifo` suffix. Use a per-queue delay, or a standard
(non-FIFO) queue.

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

## Credential resolution

`credentials_uri` is resolved by the bridge credential store at build time and
threaded into the adapter as the **initial** credentials: the receiver and
sender build their first SQS client from a
`credentials.NewStaticCredentialsProvider` seeded with the resolved key. A later
rotation swaps the client atomically -- SQS is stateless per request, so there
is no connection to churn.

An auth failure on the send or receive path is not classified permanent
immediately. A freshly-granted IAM role or queue policy commonly takes 10--120s
to propagate, during which SQS returns `AccessDenied` for a condition that will
self-heal. Each receiver and sender holds a bounded, clock-driven grace window
(120s) that keeps such failures transient and retryable; only once the window
lapses without a successful call does the failure escalate to a permanent
`NOT_AUTHORIZED`. A successful call resets the window. This closes the
rotation-gap in which a transient auth error would otherwise DLQ or drop messages
and ack the source. (KMS `AccessDenied` on the send path is already treated as
temporary by classification, independent of this grace.)

Only long-lived static keys are supported on this path. A resolved access-key ID
with an `ASIA…` prefix (temporary/STS material) is rejected with
`ErrTemporaryCredentialsUnsupported`, surfaced as `NOT_AUTHORIZED` (permanent):
the credential model carries no session token, so a static provider built from
it would fail every request. Leave `credentials_uri` unset to fall back to the
SDK default provider chain (environment, shared config, or an instance/task
role), which may itself be STS-backed. See
[Credential Rotation](../credentials-rotation.md).

## Bridge-to-bridge identity across an SQS hop

The idempotency key travels as the `x-bridge.idempotency-key` message attribute,
which only a bridge egress emits. The dedup ID and ordering key are not carried
in a bridge attribute — they are the message's native FIFO coordinates,
`MessageDeduplicationId` and `MessageGroupId`, present on any FIFO message
whatever the producer: a peer bridge sets them deliberately, and a non-bridge
FIFO producer's coordinates are lifted the same way. See **Egress attribute
priority** under [Resilience Behavior](#resilience-behavior). These terms are
defined in the [Ubiquitous Language](../../UBIQUITOUS.md).

The `x-bridge.idempotency-key` attribute is subject to SQS's message-attribute
count and 256 KiB size caps on egress. It holds rank-0 priority (dropped last),
but a near-maximum-size payload can still evict it — counted on the
dropped-attributes metric. That 256 KiB ceiling is the `max_message_bytes`
default; raising it to match a queue whose `MaximumMessageSize` is provisioned
higher keeps a large body from evicting these best-effort attributes. The
native FIFO fields are not charged against the attribute budget and always
survive, so idempotency propagation is best-effort under cap pressure while
deduplication and ordering are not.

The receiving bridge lifts these values into the envelope's first-class fields at
ingress, before `messaging.NewEnvelope` strips every `x-bridge.*` key from the
untrusted header map. Without the lift the identity vanishes at the receiving
hop, and deduplication and ordering suppression break across a
bridge → SQS → bridge relay. The lift mirrors the AMQP 1.0 adapter, which raises
the same identity out of application properties on its own ingress. The values
land in first-class fields, so they survive the header strip regardless of the
route's `trust_bridge_headers` flag — that flag governs whether the `x-bridge.*`
header *keys* are kept, not the identity fields.

The lift reads this SQS message's own attributes and system fields. A
`bridge → SNS → SQS` path delivered non-raw (JSON) buries `x-bridge.idempotency-key`
inside the SNS envelope body, where the lift cannot reach it, so cross-hop
idempotency relies on direct bridge → SQS sends or raw-delivery SNS
subscriptions.

| Envelope field | Source on the SQS message | Notes |
|---|---|---|
| **Idempotency key** | message attribute `x-bridge.idempotency-key` (case-insensitive, `DataType=String`) | mirror of the AMQP 1.0 adapter |
| **Dedup ID** | **system** attribute `MessageDeduplicationId` | FIFO only; absent on standard queues |
| **Ordering key** | **system** attribute `MessageGroupId` | FIFO only; absent on standard queues |

**Trust model.** The bridge cannot verify the `x-bridge.idempotency-key`
attribute genuinely came from a peer bridge — anyone holding `sqs:SendMessage` on
the queue can set it. This is the same trust boundary as AMQP 1.0 (broker access
is trust) and the same operator responsibility the `sns_unwrap` note names above.

**Blast radius.** The lifted idempotency key propagates to downstream
idempotency-keyed dedup points — the HTTP receiver's ingress dedup window
(`adapters/http/transport/dedup.go`: a node-local, capacity-bounded LRU with no
time expiry, keyed on the idempotency key and shared across producers), or a
re-emitted FIFO dedup ID on a subsequent hop. A spoofed — or predicted, or
colliding — idempotency key can therefore suppress a *different, legitimate*
message downstream, not only the spoofer's own duplicate. This is an availability
consideration, not message forgery: write access to the queue already implies
message injection. Mitigate by (a) restricting `sqs:SendMessage` on the queue to
trusted principals, and (b) using unguessable idempotency keys so an attacker
cannot predict a legitimate message's key.

Dedup ID and ordering key are read **only** from the native FIFO fields;
`x-bridge.dedup-id` and `x-bridge.ordering-key` message attributes are ignored,
so neither can be injected through an attribute.

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
- **Batch error classification.** `SendMessageBatch` per-entry failures classify
  the AWS error `Code` with the SAME policy as single-send errors **before**
  falling back to the sender-fault verdict: KMS conditions (`KmsAccessDenied` →
  transient auth, `KmsThrottled` → throttled, `KmsDisabled`/`KmsInvalidState`/… →
  not-authorized) and throttling / service faults (`ThrottlingException`,
  `OverLimit`, `ServiceUnavailable`, `InternalError`) stay **retriable**. Only a
  code outside that set falls back to the fault flag — a caller-malformed request
  (`SenderFault=true`) is `ErrorRejected` and routed to DLQ, a server fault is
  transient. This keeps a per-entry retryable target outage (a KMS grant still
  propagating, request throttling) from becoming a terminal reject that costs the
  source its retry.
- **Poison-message handling.** A message the receiver cannot convert is dropped
  **without** a `DeleteMessage` by default, so the source queue's native redrive
  policy owns it (moving it to a DLQ *preserves* the payload). Startup validation
  surfaces a queue with no redrive policy (`SQSMissingRedrivePolicy` + warning;
  permission-gated, never fails startup), and the optional `poison_max_receives`
  backstop deletes a poison message once its `ApproximateReceiveCount` reaches the
  bound (`SQSPoisonDropped`) to break an unbounded redelivery loop where a native
  redrive policy cannot be configured. Because that delete is **destructive** (no
  DLQ copy), two guards prevent it from silently causing data loss: it must be
  `>= 2` unless `poison_drop_without_dlq` opts in, and startup **refuses to
  start** (`INVALID_CONFIG`) when a readable native `maxReceiveCount >=
  poison_max_receives` — otherwise the backstop would fire first and pre-empt the
  DLQ. See the Receiver Options Reference.
- **Adaptive auto-extend ticker.** When `Extend()` changes the SQS visibility
  timeout, the auto-extend ticker interval updates accordingly, preventing
  excessive or insufficient extend calls.
- **Terminal receive faults degrade the route.** A `ReceiveMessage` error that
  cannot self-heal — queue deleted, IAM revoked past the auth grace, invalid URL
  — is returned from the poll loop to the supervisor (`superviseRoute`) instead of
  being tight-retried behind an already-green readiness signal. The supervisor
  records the component error and restarts or degrades the route in isolation, so
  health reflects a non-functional route rather than reporting ready. Only
  genuinely transient errors stay on the retry-with-backoff path.
- **Per-batch client snapshot.** The poll loop snapshots the SQS client once per
  receive and binds it to every delivery from that batch, so `Ack`, `Retry`, and
  auto-extend run under the same principal that received the message. A credential
  rotation that swaps the client mid-batch cannot make settlement run under a
  different client (which would fail the delete/extension and redeliver the
  message).
- **Pre-delete visibility margin.** `Ack` stops auto-extension, then — when the
  remaining visibility window is shorter than the settlement budget — issues one
  final `ChangeMessageVisibility` to a floor (~15s: the 10s settlement bound plus
  a 5s buffer) before `DeleteMessage`. A `visibility_timeout` as low as 2--3s is
  otherwise permitted and a delete can take up to 10s, so without the margin the
  message could resurface to another consumer before the delete lands — a
  duplicate, and FIFO group churn. The extension is best effort: on failure or
  timeout `Ack` still proceeds to the delete.
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

