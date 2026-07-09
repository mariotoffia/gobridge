# Azure Service Bus

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `servicebus`
**Factory:** `servicebus.NewFactory(logger)`
**Capabilities:** `visibility_extension`, `source_redelivery`, `delayed_send`

Azure Service Bus is stateless in GoBridge (no bridge-level sessions).
The `options:` block is decoded into a nested typed config: receiver settings
under `options.receiver`, sender settings under `options.sender`, and shared
connection/credential settings under `options.connection`. The transport
supports both queues and topic/subscription patterns, plus Azure SB sessions
for ordered processing.

> **Capabilities are mode-aware.** The transport advertises
> `visibility_extension`, `source_redelivery`, and `delayed_send`, but the builder
> narrows the set per route from the receiver config. A `PeekLock` queue honours
> all three. A `PeekLock` subscription drops `delayed_send`: a scheduled retry
> would address the topic and fan out to sibling subscriptions, so a delayed
> `Retry` falls back to an immediate `Abandon` (see [Retry Wire
> Semantics](#retry-wire-semantics)). A `ReceiveAndDelete` receiver honours none
> of them — the message is gone at receive time — so its empty set lets the
> runtime's "no retry + no DLQ = silent drop" check fire instead of being masked.

## YAML Example

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

## Receiver Options Reference (`options.receiver.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Service Bus queue name |
| `topic_name` | string | -- | Service Bus topic name |
| `subscription_name` | string | -- | Subscription on the topic |
| `session_id` | string | -- | Pin the receiver to ONE ASB session (cannot combine with `sub_queue` or `use_sessions`) |
| `use_sessions` | bool | `false` | Consume a session-enabled entity by accepting the **next available** session and rotating between sessions (cannot combine with `session_id` or `sub_queue`) |
| `max_messages` | int | 10 | Messages per receive call (1--100). Forced to 1 in `ReceiveAndDelete` mode (warned). |
| `max_wait_time` | duration | `30s` | Maximum wait for messages (>= 1s; a bare int decodes as nanoseconds and is rejected) |
| `receive_mode` | string | `PeekLock` | `PeekLock` or `ReceiveAndDelete` (case-insensitive; unknown values rejected). `ReceiveAndDelete` settles at the broker on receive — **at-most-once**: a crash after receive is unrecoverable loss. Rejected at config parse unless `allow_at_most_once: true` is also set. |
| `allow_at_most_once` | bool | `false` | Explicit opt-in required for `receive_mode: ReceiveAndDelete`. Without it the config fails to parse, because ReceiveAndDelete deletes at the broker on receive (`Ack` is a no-op, `Retry` is unsupported) and a crash after receive loses the message. |
| `sub_queue` | string | -- | `""`, `"deadletter"`, or `"transferdeadletter"` (case-insensitive) |
| `lock_duration` | duration | `30s` | Expected lock duration (for auto-extend). Accepted range 5s--5m; `0` → 30s default. |
| `auto_extend` | bool | `true` | Renew lock at 50% of duration |
| `max_lock_renewal_duration` | duration | `5m` | Caps total wall-clock time a single delivery's lock is auto-renewed. When the cap is hit the delivery's context is cancelled and renewal stops, so a hung pipeline cannot hold a message locked forever. Counted by `ASBLockRenewalCapExceeded`. |

Either `queue_name` or both `topic_name` + `subscription_name` are required.

> **Session-required entities.** A session-enabled queue or subscription needs
> either `session_id` (pin one session) or `use_sessions` (rotate over
> available sessions); with neither, the receiver fails fast with
> `ErrNotSupported` naming both remedies. `use_sessions` accepts the next
> available session, rotates to another when the held session idles **and**
> every outstanding delivery has settled, backs off quietly when no session is
> available (not counted as a failure), and sheds the held session on a
> receive error.

> **Removed:** the flat `prefetch` key no longer exists. The receiver runs at
> `max_messages` credit with no separate prefetch knob.

## Sender Options Reference (`options.sender.*`)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `queue_name` | string | -- | Service Bus queue name |
| `topic_name` | string | -- | Service Bus topic name |
| `default_session_id` | string | -- | Default ASB session for messages |
| `batch_size` | int | 10 | Upper bound on messages per batch |
| `timeout` | duration | `30s` | Per-call send timeout (applied per chunk in `SendBatch`) |

Either `queue_name` or `topic_name` is required.

## Connection Options (`options.connection.*`)

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

> **SDK retry (`options.connection.retry.*`).** The Azure SDK retries inside every
> client operation (send, receive, settlement) beneath gobridge's own retry policy
> and the poll backoff, so on a throttled namespace the layers multiply. Tune the
> SDK layer with `retry.max_retries` (SDK default 3; a negative value disables SDK
> retries so gobridge owns all retry), `retry.retry_delay` (initial backoff,
> default 4s), and `retry.max_retry_delay` (backoff cap, default 120s). Leaving the
> `retry` block unset keeps the SDK defaults unchanged.

## Retry Wire Semantics

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

The scheduled delay is honored only on a queue. On a topic subscription a
delayed `Retry` falls back to an immediate `Abandon` — the delay is dropped
because a scheduled message addresses the topic and would fan out to sibling
subscriptions. Redelivery still happens; only the delay is lost.

## Empty MessageID Fallback

A broker message with no `MessageID` does not get a fresh random envelope ID per
delivery — that would defeat downstream dedup, which would treat each redelivery
as a distinct message. The adapter derives a stable fallback ID from the broker
`SequenceNumber`, namespaced by the fully-qualified receive entity:
`asb-seq:<scope>:<sequence>`. The scope encodes the entity kind so no two
distinct entities can collide:

- queue: `q:<queue-name>`
- subscription: `s:<topic-name>:<subscription-name>`
- bare topic: `t:<topic-name>`

The `SequenceNumber` is unique only within one entity, so without the scope
prefix a queue and a subscription that each assign sequence number 5 would derive
the same fallback ID and cross-entity dedup could suppress a legitimate message.
The `q:`/`s:`/`t:` prefixes make the mapping injective, so that cannot happen. A
bridge-scheduled retry copy keeps its salted wire `MessageID` and restores the
first delivery's `MessageID` from `x-bridge.original-message-id`, so this
fallback only applies to messages that genuinely arrived without one.

## Credential Rotation

For a queue, a topic sender, or a non-session receiver, rotation is atomic: the
adapter builds a fresh client first and commit-and-swaps only on success, so
in-flight operations finish against the old client, new operations use the new
one, and a failed build leaves the old client serving. There is no window with no
client on this path.

A **pinned-session** receiver (`session_id`) is different. The broker holds an
exclusive lock on the session, so two clients cannot hold it at once. Rotation
closes the old link **before** building the replacement, leaving a brief window
where the receiver has no client. If the rebuild fails, the new connection stays
uncommitted and the rebuild is marked pending; the poll loop (or a re-push of the
same credentials) retries it, and `currentClient()` returns nil until it
succeeds. The gap is visible and recoverable — never a nil-panic — but it is a
real no-client window that the non-session path does not have.

## Resilience Behavior

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

