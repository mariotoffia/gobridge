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
| `queue_name` | string | -- | Physical queue name (resolved at startup) |
| `queue_tags` | map of strings | -- | Required AWS resource tags selecting exactly one queue; alternative to URL/name |
| `queue_name_prefix` | string | -- | Optional discovery prefix; valid only with `queue_tags` |
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

Provide `queue_url`, `queue_name`, or `queue_tags`. URL and name may still be
supplied together; the URL takes precedence. Tags cannot be combined with either.

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
| `queue_name` | string | -- | Physical queue name (resolved at first send) |
| `queue_tags` | map of strings | -- | Required AWS resource tags selecting exactly one queue; alternative to URL/name |
| `queue_name_prefix` | string | -- | Optional discovery prefix; valid only with `queue_tags` |
| `region` | string | SDK default | AWS region |
| `endpoint` | string | -- | Override endpoint (for LocalStack) |
| `profile` | string | -- | AWS shared-config profile name |
| `delay_seconds` | int | 0 | Delivery delay in seconds (0--900). Backs the `delayed_send` capability. **Rejected on FIFO queues** -- see the FIFO delay rule below. |
| `batch_size` | int | 10 | Messages per SendMessageBatch (1--10) |
| `timeout` | duration | `30s` | Bounds lazy initialization and each send call |
| `message_group_id` | string | -- | Default FIFO message group ID |
| `fifo` | bool | `false` | Opt into per-envelope FIFO groups via the `x-bridge.ordering-key` header |
| `max_message_bytes` | int | 1048576 | Message-size ceiling in bytes (body + attributes). `0` keeps the 1 MiB default, which is the service's own default `MaximumMessageSize`; set it to match a queue provisioned below that. |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |

Provide `queue_url`, `queue_name`, or `queue_tags`. URL and name may still be
supplied together; the URL takes precedence. Tags cannot be combined with either.

## Queue discovery by tags

Use a stable physical `queue_name` when one is known. Otherwise, `queue_tags`
lets an embedded configuration identify a queue without a deployment-time URL
token or manual URL resolution:

```yaml
senders:
  - id: orders-out
    transport: sqs
    options:
      region: eu-west-1
      queue_tags:
        application: orders
        environment: production
      queue_name_prefix: orders-  # Optional; omit if the name is unknown.
bindings:
  - id: to-orders
    sender_id: orders-out
    address: "sqs:queue"
```

`address: "sqs:queue"` means “use this sender's configured queue” and is exposed
as `sqs.QueueAddress` in Go. It does not enable per-message queue selection.
The same selector fields work on receivers. A binding cannot change its sender's
queue or discovery scope: the runtime constructs the sender only from
`SenderDef.Config`. Bindings may repeat the identical selector or carry partial
non-reference options, but must not select another queue, region, profile,
endpoint, or credential source. CDK rejects those changes before embedding,
registry validation, or IAM grants. Binding options do not construct a separate
sender or merge into its configuration.

The adapter uses only the SQS APIs: paginated `ListQueues` with `MaxResults`
and `NextToken`, followed by `ListQueueTags` on every candidate. The configured
AWS credentials and region determine the scan's account and region. All required
tag keys and values must match, including the presence of keys whose value is
empty. The optional prefix narrows the names returned by SQS.

- Exactly one observed match: use its queue URL.
- No match: return transient `UNAVAILABLE`. Receiver readiness stays false;
  the runtime owns retries. Senders return the transient error from their
  lazy initialization.
- Multiple matches: return permanent `INVALID_CONFIG` with an ambiguity error.
  The adapter never chooses the first queue.
- An API or permission failure: preserve its classified error and underlying
  cause; do not turn it into a no-match result.

Discovery runs during receiver startup or the sender's first `Send`/`SendBatch`.
Receiver `init_timeout`, sender `timeout`, and any earlier caller deadline bound
the scan. Failed discovery is not cached. Successful discovery pins the URL in
the adapter instance; it never replaces the logical selector in the configuration.
Later tag changes do not retarget a running adapter. Recreate the adapter to
resolve again. This is an observed scan, not an atomic snapshot of AWS tags.

A selector must contain 1–50 tags. Keys must contain 1–128 UTF-8 characters;
values may contain 0–256. Empty maps, null selectors, and empty keys are invalid.
The prefix is at most 80 characters and permits letters, digits, hyphens,
underscores, and periods. Neither selector values nor prefixes receive defaults.

**IAM permissions.** In addition to the queue's exact send/receive permissions,
tag discovery requires `sqs:ListQueues` on `*` because AWS does not support
resource-scoping that action. `sqs:ListQueueTags` must cover **every scanned
candidate**, using queue ARNs in the account/region with the configured prefix,
or all queue names when no prefix is set. Granting it only on the intended queue
would abort discovery when a different candidate is inspected. These are
metadata permissions; send, receive, delete, and visibility grants must remain
scoped to the exact intended queue. URL/name modes need no tag-discovery grants.
Name resolution requires `sqs:GetQueueUrl`.

**CDK registration.** List the actual `awssqs.IQueue` in the construct's
`Queues` prop and its selector under the same key in `QueueTags`:
`Queues["orders"] = queue` and
`QueueTags["orders"] = gobridge.QueueTags{Tags: map[string]string{"application": "orders"}}`.
For the `bridgecfg` builder, also add the queue to a helper
`registry.NewQueueRegistry()` with `AddQueue("orders", queue)`, call
`BindQueueTags("orders", map[string]string{"application": "orders"}, "")`,
and check the returned error before passing `Ref("orders")` to the builder.
The map key is not a physical queue name. The builder uses explicit
selector bindings or a known physical name, never an automatically generated
`QueueUrl` token. For an owned queue, the construct applies the selector tags.
For an imported queue, the selector is an explicit contract that its producer
applies those tags; CDK neither scans AWS nor assumes imported tags are visible.
Synth validation requires one listed queue for each selector, while runtime
discovery also detects additional matching queues outside `Queues`.

Queue resolution honors explicit `region`; otherwise CDK uses the consuming
task stack's region (ECS supplies it to the SDK environment). A custom credential
profile does not erase that known region. It rejects known
region/account mismatches before granting access, and keeps the complete SQS
configuration during validation rather than reducing it to a queue name.
Name and tag discovery use the credential account, not the queue producer's
account: `ListQueues` cannot discover another account's queues.

For the normal task-role/default-credential path, the consuming stack supplies
the expected account. A configured profile, `credentials_uri`, or custom endpoint
makes the credential account unknown to CDK. Unresolved stack/queue environments
are also unknown. In those cases listing the queue is the caller's assertion that
the queue is in the actual runtime discovery scope; synth does not inspect
credentials or query AWS to prove that assertion. Deployment-specific SDK-chain
overrides outside plugin config must be reviewed against this default-credential
contract. Known mismatches fail; unknown identity is not presented as verified.
Direct queue URLs retain their non-embedded cross-account support.

Before embedding, `bridgecfg.ValidateEmbeddedSQSConfig` requires each SQS sender
and receiver to specify a physical name or tag selector. It rejects `queue_url`
even when literal or paired with a name, and rejects URL-shaped SQS binding
addresses. The check follows inherited transport kinds but does not inherit
queue options from sessions. Since SQS creates no live Session, receiver/sender
connection attachments are rejected; declare the transport explicitly and put
queue options on each receiver/sender. A binding's `session_id` is instead its
outbox/drainer owner and may reference a stateful MQTT session independently of
the SQS sender. A binding referencing a stateless SQS session is rejected.
Non-embedded configurations still support URLs.

If tag discovery finds a FIFO queue, the adapter enforces FIFO rules before
polling or sending: the receiver polls one message at a time, and the sender
requires a default group or `fifo: true` and rejects per-message delay.

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
(`adapters/aws/transport/sqs/config_plugin.go`). Queue identity is
enforced later: `ValidateQueue` at build time (`config_plugin.go`) and
the sender's `ValidateAddress`, which is offline and structural — a name-only
sender accepts any URL whose trailing segment matches the queue name, so a wrong
region or account passes the build and is only caught at first `Send` once the
canonical URL resolves (`sender.go`). Do not rely on config parse errors
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
count and message-size caps on egress. It holds rank-0 priority (dropped last),
but a near-maximum-size payload can still evict it — counted on the
dropped-attributes metric. That ceiling is the `max_message_bytes` default,
1 MiB, which tracks the service's own default `MaximumMessageSize`; a queue
provisioned below it needs the key set to match, or attributes are kept on a
body the queue then rejects outright. The
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
  on `Started()` alone (`adapters/aws/transport/sqs/receiver.go`).
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
- **Egress attribute priority.** SQS caps a message at 10 attributes and 1 MiB
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
