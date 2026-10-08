# MQTT 3.1.1 message ids

> Part of [MQTT 3.1.1](mqtt-311.md).

An MQTT 3.1.1 PUBLISH carries a topic, a payload, a QoS, a RETAIN flag and a
DUP flag. It has no properties, so no producer id can arrive. The session gives
each message an id itself, and `options.session.message_id` chooses how:

| `message_id` | Envelope id | Marked `x-bridge.generated-id` |
|---|---|---|
| omitted or `random` (the default) | A fresh random id, new on every broker redelivery | Yes |
| `content_hash` | `mqtt-sha256:` and a hash of the broker session, the topic and the payload, the same on every redelivery to that session | No |

`content_hash` is valid only with `protocol_version: v3.1.1`. On an MQTT 5
session the configuration fails with `INVALID_CONFIG`: an MQTT 5 producer can
send its own id in the `mqtt.message-id` user property or in correlation data.
A value other than `random` or `content_hash` is rejected too.

```yaml
sessions:
  - id: legacy-broker
    transport: mqtt
    options:
      session:
        broker_url: "ssl://legacy.example.com:8883"
        client_id: "bridge-legacy-01"
        protocol_version: v3.1.1
        message_id: content_hash   # random (default) or content_hash
```

## `random`

Every delivery gets a new id, including a broker redelivery of the same
message. This is the behaviour described under
[No headers on ingress](mqtt-311.md#no-headers-on-ingress):

- The replay cap cannot count retries. With a finite `max_replay_attempts`, a
  message whose processing fails is dead-lettered or dropped on its first
  failure. `max_replay_attempts: 0` retries without a limit.
- `shared_outbox` cannot recognise a broker redelivery, so the message can
  reach downstream twice. That is normal at-least-once delivery.

Nothing is ever dropped as a duplicate.

## `content_hash`

The id is `mqtt-sha256:` followed by a SHA-256 digest, encoded as base64url
without padding. The digest covers these fields, in this order:

1. the session's effective client id, after `client_id_suffix` is applied;
2. the session's broker URLs, as one field: each URL in the canonical form the
   durable session identity uses (userinfo removed; see
   [deployment identity](mqtt.md#deployment-identity)), in configured order,
   joined by a newline;
3. the topic;
4. the payload.

Every field except the payload is prefixed with its byte length as a
big-endian 8-byte integer. QoS, RETAIN, DUP and the packet id are not part of
the hash. So:

- a broker redelivery to the same session keeps its id;
- a retained message the broker replays to the same session after a reconnect
  keeps its id;
- the same topic and payload received by a different session, with another
  client id or another broker, gets a different id;
- changing `client_id`, `client_id_suffix` or the broker list changes every id.

For client id `c`, broker `tcp://broker:1883`, topic `t` and payload `p` the id
is `mqtt-sha256:J9sTAITCmYSboGvwPskEehxc2_vzajBMr1WEj0MBI1k`.

The id becomes the message's envelope id, and GoBridge uses the envelope id
as:

- the key the replay cap counts retries on;
- the `shared_outbox` duplicate key;
- an input to the dead-letter (DLQ) entry id, together with the route,
  binding and source ids;
- the input to the deduplication id a sender sets for the next hop:
  - SQS FIFO `MessageDeduplicationId`, derived from the payload, the subject
    and the envelope id. The SQS FIFO deduplication window is 5 minutes.
  - Azure Service Bus `MessageId`, which the entity deduplicates on when
    duplicate detection is enabled on it.
  - HTTP `Idempotency-Key`.
  - `mqtt.message-id` on a publish to an MQTT 5 broker. A downstream GoBridge
    on an MQTT 5 session reads it as a producer id and deduplicates on it.

The id is not marked `x-bridge.generated-id`, so:

- the replay cap counts retries, and `max_replay_attempts` applies as
  configured;
- `shared_outbox` recognises a broker redelivery, acks it and does not send it
  again.

Because retries are counted, a failing delivery on a Persistent or Exclusive
QoS 1/2 session is retried by recycling the connection
([settlement recovery](mqtt-settlement-recovery.md)), and on MQTT 3.1.1 every
reconnect also replays the retained messages.

**Trade-off: identical messages share one id.** Two different messages that
one session receives with the same topic and payload get the same id, so
GoBridge treats them as one message. On any route, `direct_hold` included:

- they share one replay budget: retries of both count against one
  `max_replay_attempts`;
- they share one DLQ entry: if both are dead-lettered, the second dead-letter
  write is suppressed and counted only on `DLQDuplicateSuppressed`.

On a `shared_outbox` route the second is also acked and dropped as a
duplicate, with no error and no DLQ entry. Only `OutboxDuplicateSuppressed`
counts it. A downstream sink that deduplicates on one of the ids listed above
can collapse the two as well.

An outbox row keeps its id while it is pending or claimed, and for `retention`
(default `1h`) after it was delivered. During a sink outage rows stay pending,
so two identical messages hours apart can collapse. See the `retention` key in
the [configuration reference](../configuration-reference.md#store-config-fields).

For example, a sensor publishes `21.5` to `plant/7/temp` at 10:00 and again at
10:20. Both readings get the same id. A `shared_outbox` route with the default
`retention` forwards only the 10:00 reading.

Use `content_hash` only when every distinct message carries something unique,
such as a timestamp, a sequence number or an event id. Keep `random` when the
same payload can legitimately repeat:

| Payload | Example | `message_id` |
|---|---|---|
| Carries a timestamp, a sequence number or an event id | `{"seq":1042,"temp":21.5}` | `content_hash` |
| A heartbeat | `alive` | `random` |
| A status | `ON`, `OFF` | `random` |
| A raw reading | `21.5` | `random` |
| A command that can repeat | `{"cmd":"open","valve":3}` | `random` |

A repeated command is the costly case. An operator opens valve 3, closes it
and opens it again. Under `content_hash` the second `open` has the same id as
the first, so it can be dropped.

Neither setting carries headers over MQTT 3.1.1. GoBridge never wraps the
payload or encodes metadata in the topic to carry them, because the client on
the other side of the broker can be any MQTT client.
