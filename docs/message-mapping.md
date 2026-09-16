# Message mapping between transports

This page shows what a message looks like on each side of the bridge. It covers
AWS SQS, MQTT 5 and AMQP 1.0. Use it when you write the service that reads from
or writes to a queue or broker the bridge serves, and need to know which
attributes, properties and identities you will actually see.

Every message passes through one internal shape, the **envelope**. A receiving
transport turns its message into an envelope. A sending transport turns the
envelope into its own message. So each transport has two tables below: *into
the envelope* and *out of the envelope*. To follow a message from MQTT to SQS,
read "MQTT into the envelope", then "What every route does", then "SQS out of
the envelope".

The envelope has six parts:

| Part | What it is |
|---|---|
| identity | A message ID. Used for deduplication and for counting retries. |
| subject | An optional logical name for the event, such as `order.created`. It is not a topic or queue name. See [Subject vs. Address](transport-configuration.md#subject-vs-address). |
| payload | The message body, as bytes. No transport changes it. |
| headers | A map of name to value. Values keep a type: text, number, true/false, time or bytes. |
| creation time | When the message was created. Transports that carry a send time set it; otherwise it is the receive time. |
| expiry | An optional time after which the message is dead-lettered or dropped instead of delivered. |

The full envelope definition is in
[Message flow, delivery modes and processors](internals/architecture-message-flow.md).

## What every route does

Between receiving and sending, every route applies the same header rules. These
rules explain most surprises.

- **Headers starting with `x-bridge.` are removed on arrival.** The name check
  ignores letter case. A publisher or producer outside the bridge can therefore
  not set any `x-bridge.*` header, so nobody can pretend to be a bridge (for
  example by forging a tenant). A route that receives only from another trusted
  GoBridge can keep the bridge-to-bridge headers with `trust_bridge_headers:
  true` (see [Routes and Runtime Reference](routes-and-runtime-reference.md)).
  That setting keeps only what the receiving transport passed on. Each
  transport already drops most `x-bridge.*` properties itself and keeps only the
  few named in its table below.
- **Some `x-bridge.*` headers are never sent under their own name.** They
  are private bookkeeping: `x-bridge.route-id`, `x-bridge.route-override`,
  `x-bridge.source-id`, `x-bridge.content-type`, `x-bridge.generated-id` and
  `x-bridge.correlation-data`. The route removes all six even with
  `trust_bridge_headers: true`. A sending transport may still put a value into
  its own native field: MQTT turns `x-bridge.content-type` into Content Type and
  `x-bridge.correlation-data` into Correlation Data.
- **A new `x-bridge.correlation-id` is created** when none survived. With the
  default settings it is always a fresh random value. It is not the
  publisher's correlation value.
- **`traceparent` and `tracestate` always pass through.** They do not start
  with `x-bridge.`, so W3C trace context survives every hop.
- **Retry counts from another transport are removed.** A route keeps only the
  retry-count header of the transport it receives from
  (`sqs.ApproximateReceiveCount`, `asb.delivery-count` or
  `amqp10.delivery-count`) and removes the others. Otherwise a publisher could
  forge one and make its message look over the retry limit.
- **Other headers pass through unchanged**, including the transport-specific
  ones such as `mqtt.topic` or `sqs.SenderId`. Whether the sending transport can
  carry each one is decided in its table below.

## AWS SQS

### SQS into the envelope

| SQS message | Envelope | Notes |
|---|---|---|
| `MessageId` | identity | The same on every redelivery of that SQS message. |
| body | payload | Unchanged. With the receiver option `sns_unwrap`, an SNS notification body is replaced by the message inside it. |
| message attribute | header of the same name | String and Number attributes become text. Binary attributes become bytes. Names starting with `x-bridge.` are dropped. |
| `Subject` attribute (String) | subject | Optional. The attribute also stays a header named `Subject`. |
| system attributes | headers `sqs.<name>` | The receiver asks for all of them. `sqs.SentTimestamp` becomes a time and `sqs.ApproximateReceiveCount` a number; the others stay text. `SentTimestamp` also sets the envelope's creation time. |

The route reads `sqs.ApproximateReceiveCount` to count retries and
`sqs.SentTimestamp` to measure age. Neither header is removed afterwards, so a
sending transport may forward them. See "Crossing transports" below.

### SQS out of the envelope

| Envelope | SQS message | Notes |
|---|---|---|
| payload | body | Characters SQS refuses make the send fail permanently. |
| subject | `Subject` attribute | Only when the envelope has a subject. |
| headers | message attributes of the same name | See the rules below. |
| identity | not sent | SQS assigns its own `MessageId`. A receiving transport's own ID header, such as `mqtt.message-id`, is sent as an attribute. |

How headers become attributes:

- **Types.** Text, true/false and times (RFC 3339) become String. Whole numbers
  of type `int`, `int32` or `int64`, and decimals, become Number. Bytes become
  Binary. Other types are dropped without being counted. That includes unsigned
  numbers, such as `amqp10.delivery-count`.
- **Skipped on purpose.** Headers starting with `sqs.`, the private bookkeeping
  headers listed above, and a header named `Subject`.
- **Invalid names.** SQS accepts names of up to 256 characters made of
  letters, digits, `_`, `-` and `.`. A longer name, a name with any other
  character, a name starting with `AWS.` or `Amazon.`, or a name with a leading,
  trailing or doubled `.` is dropped without being counted.
- **FIFO queues.** `x-bridge.ordering-key` and `x-bridge.dedup-id` become the
  message group ID and deduplication ID instead of attributes. An SQS FIFO
  receiver fills them from the incoming message's own group and deduplication
  IDs, and AMQP 1.0 from the application properties of those names. The route
  removes both on arrival unless `trust_bridge_headers: true`, so by default the
  sender's configured group ID and its own hash are used.

SQS allows ten attributes per message, and the `Subject` attribute counts as
one of them. When more headers qualify, the sender keeps them in this order and
drops the rest. Inside each group, names are sorted alphabetically:

1. `traceparent`, `tracestate` and `x-bridge.idempotency-key`;
2. every other header, including `mqtt.*`, `amqp10.*` and anything the publisher
   sent;
3. the bridge-to-bridge headers: `x-bridge.correlation-id`,
   `x-bridge.causation-id`, `x-bridge.tenant-id`, `x-bridge.forwarded-from` and
   `x-bridge.forwarded-hop`.

The sender also keeps the body plus every attribute's name, type and value
inside `max_message_bytes` (1 MiB by default). An attribute that does not fit is
skipped, and a later, smaller one may still be kept. It never makes the body
smaller. Dropped attributes are counted on `SQSDroppedAttributes`. See
[AWS SQS](transports/sqs.md) for the options.

## MQTT 5

### MQTT into the envelope

| MQTT 5 publish | Envelope | Notes |
|---|---|---|
| user property `mqtt.message-id` | identity | Checked first. |
| Correlation Data | identity, when there is no `mqtt.message-id` | Binary Correlation Data gives the identity `mqtt-bin:<base64>`. |
| neither of the two | identity, newly created | The adapter creates a random ID and also adds it as `mqtt.message-id`. A broker redelivery gets a different ID. See [MQTT ingress headers](transports/mqtt-ingress-headers.md#envelope-identity-and-no-id-redelivery). |
| payload | payload | Unchanged. |
| topic | header `mqtt.topic` | The topic is not the subject. |
| QoS, RETAIN flag | headers `mqtt.qos` (number), `mqtt.retained` (true/false) | |
| user property `gobridge.subject` | subject | Used up, so it never becomes a header. Ignored when unsafe or over 256 bytes. An ordinary publisher does not send it, so the subject is usually empty. |
| Content Type | header `x-bridge.content-type` | Removed by the route on arrival, so it never reaches a sending transport. |
| Response Topic | header `mqtt.response-topic` | |
| Correlation Data | header `x-bridge.correlation-id` when it is text, else `x-bridge.correlation-data` holding the bytes as unpadded URL-safe base64 text | The route removes both on arrival unless `trust_bridge_headers: true`, and always removes `x-bridge.correlation-data`. By default the value survives only as the identity. |
| Message Expiry Interval | expiry | Receive time plus the interval. |
| other user properties | headers of the same name | Dropped: names starting with `x-bridge.`, the names `mqtt.topic`, `mqtt.qos` and `mqtt.retained`, and keys or values over 256 bytes, not valid UTF-8, or containing control characters. When a name repeats, the last value wins. |

A publish with more than 128 user properties is not converted at all. The whole
publish is dropped as poison and counted on `MQTTIngressPoisonDropped`. The same
happens when the payload is over `max_payload_bytes`.

Other user properties that are too long or unsafe are counted on
`MQTTIngressHeaderDropped`. A `mqtt.message-id` or `gobridge.subject` value that
is too long or unsafe is ignored without being counted.

### MQTT out of the envelope

| Envelope | MQTT 5 publish | Notes |
|---|---|---|
| identity | user property `mqtt.message-id` | A header with that name is ignored, so the identity always wins. |
| payload | payload | Unchanged. |
| subject | user property `gobridge.subject` | Only when the envelope has a subject. The subject never chooses the topic. |
| destination address | topic | The address template, with each `{name}` replaced by the text header of that exact name. A missing header or an empty topic fails the send. Without an address, the sender's `default_topic` is used. |
| sender `qos`, `retain` | QoS, RETAIN flag | Set per sender, not per message. QoS 1 waits for PUBACK, QoS 2 for PUBCOMP. |
| `x-bridge.correlation-data`, else `x-bridge.correlation-id` | Correlation Data | Normally the random correlation ID the route created. |
| `x-bridge.content-type` | Content Type | Normally absent, because the route removes it. |
| `mqtt.response-topic` | Response Topic | |
| expiry | Message Expiry Interval | Only when the envelope has an expiry. Never less than one second. |
| other text headers | user properties of the same name | Skipped: `mqtt.topic`, `mqtt.qos`, `mqtt.retained` and the private bookkeeping headers. Headers that are not text, such as numbers, times and bytes, are dropped and counted on `MQTTNonStringHeaderDropped`. |

A topic, property or user property over 65,535 bytes fails the publish
permanently instead of being cut short. See [MQTT](transports/mqtt.md) for the
options.

## AMQP 1.0

### AMQP 1.0 into the envelope

| AMQP 1.0 message | Envelope | Notes |
|---|---|---|
| `message-id` property | identity, and header `amqp10.message-id` | Turned into text: a UUID in its usual form, a number in decimal, binary in hex. When absent, a random ID is created and a redelivery gets a different one. See [Envelope identity](transports/amqp10.md#envelope-identity-publish-a-message-id). |
| body | payload | Data sections are joined. A text or binary value body is accepted. Any other body is rejected at the broker. |
| `subject` property | subject, and header `amqp10.subject` | The link address is never used as a subject. |
| `correlation-id`, `content-type`, `content-encoding`, `to`, `reply-to`, `group-id`, `group-sequence`, `reply-to-group-id` properties | headers `amqp10.<name>` | |
| `creation-time` | creation time, and header `amqp10.creation-time` | |
| `absolute-expiry-time` | expiry, and header `amqp10.absolute-expiry-time` | |
| `delivery-count` | header `amqp10.delivery-count` | Used to count retries. It is an unsigned number, so an SQS sender drops it. |
| application properties | headers of the same name | Names starting with `x-bridge.` or `amqp10.` are dropped. Binary values become hex text, UUIDs become text. |

### AMQP 1.0 out of the envelope

| Envelope | AMQP 1.0 message | Notes |
|---|---|---|
| `amqp10.message-id` header, else identity | `message-id` property | A producer can therefore choose the AMQP `message-id`. |
| payload | body | One data section. |
| subject | `subject` property | The `amqp10.subject` header is never used for this. |
| `amqp10.correlation-id` header | `correlation-id` property | Any value AMQP accepts as an ID, such as text or bytes, is passed as it is. |
| `amqp10.content-type`, `amqp10.content-encoding`, `amqp10.to`, `amqp10.reply-to`, `amqp10.group-id`, `amqp10.group-sequence`, `amqp10.reply-to-group-id` headers | properties of the same name | Text values only, except `group-sequence`, which takes a whole number. |
| `amqp10.creation-time` header (a time), else creation time | `creation-time` property | A message received over AMQP keeps its original creation time. |
| expiry | `absolute-expiry-time` property | |
| sender `durable` | `durable` header | `true` unless the sender sets `durable: false`. |
| destination address | link target address | A fixed address. A different address per message fails the send. |
| other headers | application properties of the same name | Headers starting with `amqp10.` and the private bookkeeping headers are not sent. |

The broker's answer decides what happens next. Accepted settles the message.
Released or Modified is a temporary failure and is retried. Rejected depends on
the error condition the broker attaches: `amqp:resource-limit-exceeded` is
treated as throttling and retried, while a rejection with no condition is a
permanent failure. See [AMQP 1.0](transports/amqp10.md) for the options.

## Crossing transports: what to expect

These results follow from the tables above. They are the ones people usually
notice first.

**SQS to MQTT or AMQP**

- **SQS details reach the broker.** The route keeps the `sqs.*` headers, and
  each sending transport sends what it can carry. MQTT sends the text ones as
  user properties, including `sqs.SenderId`, which is the AWS identity that sent
  the SQS message. `sqs.SentTimestamp` (a time) and
  `sqs.ApproximateReceiveCount` (a number) are not text, so MQTT drops them.
  AMQP sends all of them as application properties.
- **A `Subject` attribute is sent twice.** Once as the subject, and once as a
  user property or application property named `Subject`.
- **Correlation is not carried over.** The broker sees a new random
  correlation value, not one the producer set. A producer that needs its own
  value should send it as an ordinary attribute, or as `amqp10.correlation-id`
  for AMQP.
- **Binary attributes do not reach MQTT.** MQTT user properties hold text only.

**MQTT or AMQP to SQS**

- **The SQS `MessageId` is new.** Use `mqtt.message-id` or `amqp10.message-id`
  to recognise a redelivered message. For MQTT, that attribute is also present
  when the adapter created the ID, and then a redelivery has a different value.
  When the publisher sends no ID, deduplicate on a key inside the payload.
- **MQTT has no `Subject` attribute** unless the publisher sent
  `gobridge.subject`. The topic is in `mqtt.topic`.
- **MQTT Correlation Data and Content Type do not arrive** with the default
  settings, because MQTT stores them in `x-bridge.*` headers that the route
  removes. Ask the publisher to send them as ordinary user properties if the
  consumer needs them.
- **AMQP 1.0 `correlation-id` and `content-type` do arrive**, as the attributes
  `amqp10.correlation-id` and `amqp10.content-type`.
- **Some headers do not fit.** Ten attributes is a small budget. Headers late in
  the order above are dropped first.
