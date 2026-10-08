# MQTT 3.1.1

> Part of the [MQTT transport reference](mqtt.md).

## Overview

The `mqtt` transport speaks MQTT 5.0 by default. Set
`options.session.protocol_version: v3.1.1` on a session to connect it to a
broker or managed service that speaks only MQTT 3.1.1, such as Azure IoT Hub, a
legacy device fleet, or a broker configured for 3.1.1.

The session still runs on the MQTT 5 client. A translator sits between the
client and the socket, below the connection guard, and rewrites every packet:
MQTT 5 into MQTT 3.1.1 on the way out, MQTT 3.1.1 into MQTT 5 on the way in.
Settlement, reconnect, subscriptions and recovery above it work as on MQTT 5.
What MQTT 3.1.1 cannot express is either rejected when the configuration is
validated or listed on this page as degraded. The decision and the full packet
mapping are in [ADR 0022](../adr/0022-mqtt-311-by-wire-translation.md).

MQTT 3.1 (protocol name `MQIsdp`, level 3) is not supported. There is no
automatic fallback: a session speaks the version you configure.

## Enable it

| `protocol_version` | Speaks |
|---|---|
| omitted, empty, or `v5` | MQTT 5.0 (the default) |
| `v3.1.1` | MQTT 3.1.1 |

Any other value is rejected. The value is a string: the options decoder is
strict, so a bare YAML `5` decodes as a number and is refused.

This example bridges a legacy MQTT 3.1.1 broker to an MQTT 5 broker. The
legacy session is persistent, so the broker keeps its QoS 1 messages while the
bridge is away.

```yaml
bridge:
  id: legacy-to-central

stores:
  managed_subscriptions:
    type: sqlite
    options:
      path: /var/lib/gobridge/state/managed-subscriptions.db
  dlq:
    type: sqlite
    options:
      path: /var/lib/gobridge/state/dlq.db

sessions:
  - id: legacy-broker
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: "ssl://legacy.example.com:8883"
        client_id: "bridge-legacy-01"
        protocol_version: v3.1.1   # v5 (default) or v3.1.1
        # At least the broker's per-client in-flight limit (see Broker limits).
        receive_maximum: 192
        # No session_expiry_interval and no no_local: MQTT 3.1.1 cannot send them.
        username: "bridge"
        password: "secret"
  - id: central-broker
    transport: mqtt
    session_mode: ephemeral
    options:
      session:
        broker_url: "ssl://central.example.com:8883"
        client_id: "bridge-central-01"
        username: "bridge"
        password: "secret"

receivers:
  - id: legacy-telemetry
    transport: mqtt
    session_id: legacy-broker
    topics:
      - topic: "plant/+/telemetry"
        qos: 1

senders:
  - id: central-telemetry
    transport: mqtt
    session_id: central-broker
    options:
      sender:
        default_topic: "plants/telemetry"
        qos: 1

bindings:
  - id: to-central
    sender_id: central-telemetry
    session_id: central-broker
    address: "plants/telemetry"

routes:
  - id: legacy-to-central
    receiver_id: legacy-telemetry
    delivery_mode: direct_hold
    dispatch_mode: single
    bindings: [to-central]
    policy:
      allow_unfenced: true   # one replica consumes this subscription
```

## Options MQTT 3.1.1 rejects

On `v3.1.1` the configuration fails with `INVALID_CONFIG` when it sets any of
these:

| Option | Why | Instead |
|---|---|---|
| `no_local: true` | MQTT 3.1.1 has no No-Local. Loop prevention must not disappear without notice ([ADR 0010](../adr/0010-mqtt-loop-prevention-contract.md)). | Remove it, or use `v5`. |
| `session_expiry_interval` other than `0` | MQTT 3.1.1 cannot send a session expiry. The broker decides how long the session lives. | Remove it and set the lifetime on the broker, or use `v5`. |
| `password` without `username` | MQTT 3.1.1 forbids it (MQTT-3.1.2-22). | Set `username`, or use `v5`. |
| `clean_start: true` with `session_mode: persistent` | MQTT 3.1.1 has no flag that starts clean and still keeps the session afterwards. | Remove it, or use `session_mode: ephemeral`. |

The same rules run everywhere a session can be built or changed:

- when the configuration is loaded (every rule but the `clean_start` one,
  which needs the session mode);
- when the factory builds the session, and in deployment preflight;
- after `credentials_uri` resolves, for the username and password;
- on a live credential rotation, before anything changes, so a rejected
  rotation leaves the working session untouched;
- in `Start`, for a session a library consumer builds directly with
  `NewSession`. `Start` returns the error without dialling.

Each error names the key, the version and what to do. For example:

```text
mqtt: session.no_local is not available on session.protocol_version v3.1.1 (MQTT 3.1.1 has no No-Local; remove it, or use v5)
```

Persistent and Exclusive sessions still use a session expiry of 86400 seconds
internally when you leave it at `0`. It keeps durable identity and redelivery
admission meaningful. It is never sent, and on `v3.1.1` it logs no warning.

## Startup warning

Every `v3.1.1` session logs one Warn when it is created. It lists what the
session cannot do:

- carry headers or message identity. A retry decision on a minted id is
  terminal unless `max_replay_attempts` is `0`; `message_id: content_hash`
  replaces the minted id (see [Message ids](#message-ids));
- see a session takeover, other than as a connection that drops soon after it
  connects;
- see a publish the broker refuses;
- avoid retained messages being replayed on every reconnect;
- choose its in-flight window or session lifetime: the broker decides both,
  and its in-flight limit must fit `receive_maximum`.

## What behaves as on MQTT 5

- Ack after settlement, in receive order.
- QoS 0, 1 and 2.
- Session resume, and resume-loss detection from Session Present
  (`MQTTSessionResumeLost`).
- Startup grace, covered-retain and orphan cleanup.
- Managed subscription history.
- Settlement recovery and unit-rebuild containment.
- The poison escape for local ingress caps (`MQTTIngressPoisonDropped`).
- Credential rotation.
- TLS, WebSocket and proxies.
- Multi-URL failover.
- Keep-alive.
- Last Will (see [Last Will](#last-will)).
- The cluster and lease rules.
- `client_id_suffix`.
- QoS downgrade detection: the SUBACK carries the granted QoS.
- `bridge.max_mqtt_sessions`.
- The circuit breaker.

## What degrades

| Behaviour | On MQTT 3.1.1 |
|---|---|
| Headers and identity | Only the topic, payload, QoS and RETAIN flag cross the wire. On egress the sender drops every property before it validates the publish, so a header that could never be sent (over 65,535 bytes, or not valid UTF-8) cannot fail an otherwise valid publish. Every header, the subject and the expiry are dropped. For ingress, see [No headers on ingress](#no-headers-on-ingress) and [Message ids](#message-ids). |
| Session takeover | MQTT 3.1.1 has no DISCONNECT `0x8E`, so a takeover looks like any other connection loss. autopaho redials a connection that had come up with no delay, so two instances sharing a `client_id` would evict each other in a tight loop. A connection that drops within 30 s of coming up therefore feeds the takeover penalty: the first drop costs nothing, and each further one adds a reconnect penalty that starts at 1 s and doubles up to 64 s. A log line names a client-id collision as the likely cause. `MQTTSessionTakeover` is not counted, because the cause is inferred, not reported. The exclusive lease still guarantees one owner. |
| Publish refusal | PUBACK and PUBREC carry no reason code. A broker that refuses a publish either acks it (Mosquitto does) or closes the connection, which surfaces as `CONNECTION_LOST` (retryable). `throttle_retry_after` never applies. |
| Subscribe refusal | SUBACK `0x80` is the only failure code. It keeps its MQTT 5 meaning, `UNAVAILABLE` (transient), so a broker ACL denial is not classified `FORBIDDEN`. The reconcile still fails, as for any refused subscription. |
| Retained replay | MQTT 3.1.1 has no Retain Handling. Every reconnect re-subscribes every filter, and so does every QoS re-check, so each matching retained message is delivered again. These are at-least-once duplicates. They carry `mqtt.retained=true`, so a consumer can filter them. |
| Flow control | `receive_maximum` is not sent, but the session enforces it, and it still sizes the dispatch queue and the pending buffer. The broker's per-client in-flight limit must not exceed it. See [Broker limits](#broker-limits). |
| Maximum packet size | Not advertised. An oversized PUBLISH is acked and dropped instead of being refused by the broker; see [Oversized publishes](#oversized-publishes). Egress has no broker limit to check against, so its cap falls back to the protocol maximum, as it does when an MQTT 5 CONNACK omits the property. |
| UNSUBACK detail | Every filter reports Success. Managed cleanup therefore always takes its connection-recycle path: one extra reconnect per cleanup. Orphan cleanup on an ephemeral session never reports that a subscription survived cleanup. A broker that refuses an UNSUBSCRIBE anyway (Mosquitto dynamic-security ACLs can) looks like success, and the managed history forgets a filter the broker still holds. The broker must permit UNSUBSCRIBE for every filter the session subscribes. |
| Session lifetime | Broker policy; see [Broker limits](#broker-limits). `session_expiry_interval` is rejected. |
| Shared subscriptions | `$share/` depends on the broker. Mosquitto, EMQX, HiveMQ, VerneMQ and AWS IoT accept it from MQTT 3.1.1 clients. Azure Event Grid disconnects the client. The transport still declares `shared_consumer`, because capabilities belong to the transport, not to one configuration. |
| Error classification | CONNACK return codes 1 to 5 map to the nearest MQTT 5 reason code; see [CONNACK return codes](#connack-return-codes). There is no server-busy (`0x89`) or quota (`0x97`) signal. |

### No headers on ingress

An MQTT 3.1.1 PUBLISH carries a topic, a QoS, a RETAIN flag and a payload,
nothing else. The envelope gets `mqtt.topic`, `mqtt.qos` and `mqtt.retained`,
as on MQTT 5. None of these arrive:

- the producer's `mqtt.message-id` and correlation data;
- the subject (`gobridge.subject`);
- the expiry, content type and response topic;
- `traceparent`;
- user properties.

A header an MQTT 5 publisher set does not survive the hop to an MQTT 3.1.1
subscriber. With the default `message_id: random`, every message gets a minted
id, which `mqtt.message-id` also carries, and is marked `x-bridge.generated-id`.
A broker redelivery gets a new id
([envelope identity](mqtt-ingress-headers.md#envelope-identity-and-no-id-redelivery)).

So with `random` every MQTT 3.1.1 message is **count-less**: it has no stable
key and no native receive count. What that means for a route:

- With a finite `max_replay_attempts`, the route's replay cap treats every
  retry decision for the message as already at the cap. That covers a
  recoverable processor failure and an outbox persist failure that is not a
  deadline, as well as a send. The message is dead-lettered or dropped on its
  first such failure instead of being retried through redelivery.
- On `direct_hold`, a failing send is still retried in process for
  `send_retry_budget` first. Once that is spent, the message is sunk with
  category `unstable_identity`.
- `max_replay_attempts: 0` opts the route into unbounded retry. That is the
  existing contract for count-less sources.
- `shared_outbox` cannot deduplicate a broker redelivery.

`message_id: content_hash` gives each message a stable id instead, so its
retries are counted and `shared_outbox` recognises a redelivery. It has a
trade-off; see [Message ids](#message-ids).

## Message ids

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

### `random`

Every delivery gets a new id, including a broker redelivery of the same
message. This is the behaviour described under
[No headers on ingress](#no-headers-on-ingress):

- The replay cap cannot count retries. With a finite `max_replay_attempts`, a
  message whose processing fails is dead-lettered or dropped on its first
  failure. `max_replay_attempts: 0` retries without a limit.
- `shared_outbox` cannot recognise a broker redelivery, so the message can
  reach downstream twice. That is normal at-least-once delivery.

Nothing is ever dropped as a duplicate.

### `content_hash`

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

## Broker limits

MQTT 3.1.1 leaves two things to the broker that an MQTT 5 client asks for
itself: how many unacknowledged QoS 1/2 publishes the broker sends at once, and
how long it keeps a session after the client disconnects. The defaults below
are each broker's own. Only Mosquitto is tested here; see
[broker support](mqtt-broker-support.md).

| Broker | Per-client in-flight limit (default) | Session lifetime (default) |
|---|---|---|
| Mosquitto | `max_inflight_messages`: 20 | never expires |
| EMQX | `max_inflight`: 32 | 2 h |
| AWS IoT Core | 100 | 1 h |

**Set `receive_maximum` to at least the broker's in-flight limit.** The default,
192, is above each limit in the table. The session tracks the QoS 1/2 packet
ids it has received and not yet acknowledged. When the broker sends one more
than `receive_maximum` allows, the session refuses it before the client sees
it:

- the connection drops, and `MQTTIngressRejected` counts the reject;
- nothing is acked or lost, and the broker redelivers on the next connection;
- the Error log asks you to raise `receive_maximum` to at least the broker's
  per-client in-flight limit;
- the session reconnects with the reject backoff and reads not ready, as for
  any [ingress reject](../runbooks/mqtt-ingress-poison.md#broker-sends-a-malformed-or-oversized-packet).

This repeats on every connection until the two limits fit. The session never
wedges. QoS 0 is not counted, as on MQTT 5.

A broker may also resend an unacknowledged PUBLISH on the same connection,
which MQTT 3.1.1 allows (EMQX `retry_interval`). The session drops that copy
while the original is still in flight. Settling the original acks it.

**Session lifetime.** A Persistent or Exclusive session that stays offline
longer than the broker keeps it loses its queued messages. The next connect
reports a resume loss (`MQTTSessionResumeLost`).

## Oversized publishes

MQTT 3.1.1 cannot tell the broker a Maximum Packet Size, so the broker forwards
a PUBLISH of any size. The session never buffers it whole:

1. It reads the topic and packet id.
2. It keeps the first `max_payload_bytes + 1` payload bytes.
3. It hands that truncated publish to the client at once.
4. It discards the rest of the payload on the next read.

The publish callback then acks and drops the publish because it is above
`max_payload_bytes`, counted on `MQTTIngressPoisonDropped`. It is never
redelivered, and later traffic flows. Because the truncated publish is handed
over before the rest is read, its ack can reach the broker while the rest is
still being discarded. If that takes longer than the keep-alive and the
connection drops, the broker already holds the ack and does not send the
publish again.

The ack still waits behind earlier unsettled deliveries, because acks go out in
receive order. To bound how long a large transfer can take, set the broker's
own maximum packet size (Mosquitto `max_packet_size`, EMQX `max_packet_size`).

A packet of any other type above the session's maximum packet size, or a
malformed MQTT 3.1.1 packet, is refused as an ingress reject
(`MQTTIngressRejected`). Only a broken broker or intermediary produces one.

## CONNACK return codes

MQTT 3.1.1 has six CONNACK return codes. The session maps each to the nearest
MQTT 5 reason code, and then classifies it like any MQTT 5 CONNACK
([error classification](mqtt-behavior.md#resilience-behavior)).

| MQTT 3.1.1 return code | MQTT 5 reason code |
|---|---|
| 0, connection accepted | `0x00` Success |
| 1, unacceptable protocol version | `0x84` Unsupported protocol version |
| 2, identifier rejected | `0x85` Client identifier not valid |
| 3, server unavailable | `0x88` Server unavailable |
| 4, bad user name or password | `0x86` Bad user name or password |
| 5, not authorized | `0x87` Not authorized |

Codes 4 and 5 surface as `NOT_AUTHORIZED`. Any other value is a malformed
packet.

## Last Will

The Last Will works on MQTT 3.1.1, but will properties do not exist there. A
graceful `Close` sends DISCONNECT, and the broker discards the will. Every
other end of a connection sends no DISCONNECT, so the broker publishes the
will. That includes an ingress reject: on MQTT 3.1.1 the session closes the
socket without a DISCONNECT. On MQTT 5 the same reject tries to send
DISCONNECT `0x95` or `0x81`, and Mosquitto discards the will when one arrives
([ADR 0021](../adr/0021-contain-mqtt-recovery-and-ingress-reject-in-session.md)).

## Switching an existing session

On `v3.1.1` the protocol version is part of a session's durable identity. A
broker need not resume a session that was created over the other version; AWS
IoT Core does not. Switching a Persistent or Exclusive session between `v5` and
`v3.1.1` is therefore handled like a `client_id` change:

- the supervisor refuses it as a live reload;
- the managed subscription history needs the documented migration
  ([managed-filter migration](../runbooks/mqtt-managed-subscription-migration.md));
- drain the broker backlog before you switch.

On `v5` the identity is what it was before MQTT 3.1.1 support existed, so
existing sessions keep their stored history. The client-ID collision check
ignores the version: two sessions with one `client_id` on one broker collide
whatever their versions.

An Ephemeral session has no durable identity. Changing its protocol version is
an ordinary rebuild of its reload unit.

## What MQTT 3.1.1 cannot do

These MQTT 5 features have no MQTT 3.1.1 form. The adapter never uses them, so
there is no option to reject: topic alias, subscription identifiers, enhanced
authentication (AUTH), request/response information, server reference and will
properties.

These cannot be offered on MQTT 3.1.1 at all: a session expiry interval,
broker-side message expiry, publish refusal verdicts, a takeover reason, and
headers without changing the payload.

ADR 0022 lists the extensions that were considered and not done: No-Local
through a broker-specific "bridge bit", a payload envelope that carries
headers, and suppressing retained replay on a resumed session.

## Proof

The tests that prove MQTT 3.1.1 behaviour against a real broker are listed on
[MQTT broker support](mqtt-broker-support.md#proved-on-mqtt-311).
