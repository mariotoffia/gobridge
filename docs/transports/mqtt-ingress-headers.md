# MQTT ingress headers

At ingress the adapter stamps the delivered MQTT metadata onto the envelope
headers under the reserved `mqtt.*` namespace:

| Header | Value | Meaning |
|--------|-------|---------|
| `mqtt.topic` | string | The concrete topic the broker delivered on (distinct from the logical subject carried in the `gobridge.subject` user property). |
| `mqtt.retained` | bool | Whether the broker delivered the publish with the retained flag set. |
| `mqtt.qos` | int | The delivered QoS level (0, 1, or 2). |

These keys are bridge-owned. An inbound MQTT user property whose name collides
with `mqtt.topic`, `mqtt.retained`, or `mqtt.qos` is dropped during conversion,
so a remote publisher cannot spoof the delivered topic, retained state, or QoS.
Read them downstream to branch on retained snapshots or on the QoS a message
arrived at.

A publish carrying **binary** MQTT v5 Correlation Data additionally gets
`x-bridge.correlation-data` — the raw bytes in unpadded URL-safe base64. That
key is in the reserved namespace rather than `mqtt.*`, so no producer on any
source transport can set it: only an ingress adapter that actually read binary
correlation off the wire installs it, and MQTT egress decodes it back to
byte-identical Correlation Data. Textual correlation data uses
`x-bridge.correlation-id` as before.

### User-property ingestion and the header filter

Beyond the reserved `mqtt.*` keys, every MQTT v5 **user property** on an inbound
publish is copied onto the envelope headers under its own name, so a route can
filter or branch on peer-supplied metadata. Two admission rules apply per
property, and a value that fails either is **dropped** (the key never appears on
the envelope):

- **Length.** A key or value longer than 256 **bytes** is dropped (a bound on
  per-message header memory; note this is UTF-8 byte length, so a multi-byte
  value reaches the cap in fewer visible characters).
- **Content.** Both the key and the value must be **valid UTF-8 with no control
  characters**. MQTT v5 user properties are UTF-8 string pairs by spec, so
  ordinary non-ASCII text is preserved — `location: Malmö` arrives intact. Only
  genuinely unsafe values (invalid UTF-8, or embedded control characters such as
  `NUL`, newline, or other `unicode.IsControl` runes) are rejected, matching the
  reserved-header safety model.

Every dropped user property is counted on **`MQTTIngressHeaderDropped`**, the
ingress counterpart to egress's `MQTTNonStringHeaderDropped`. A non-zero rate
means a peer is sending headers the bridge cannot admit (over-length or unsafe);
watch it if a route that filters on a peer header starts misrouting — a silent
drop here is now observable rather than invisible. (Reserved-key collisions
described above are a separate, deliberate anti-spoof drop and are not counted on
this metric.)

### Envelope identity and no-ID redelivery

Inbound identity uses this precedence:

1. a valid `mqtt.message-id` user property, which GoBridge peers stamp from
   `Envelope.ID`;
2. MQTT correlation data — its textual value when the bytes are a safe header
   string, otherwise `mqtt-bin:<base64url>` derived from the raw bytes, so a
   producer that identifies its message with **binary** correlation data (NUL
   bytes, non-UTF-8) keeps one identity across redelivery instead of falling
   through to case 3;
3. an RFC 4122 UUIDv4 generated once for the received publish and stamped on the
   router-owned Paho publish before buffering or fan-out.

Every handler reached by one publish therefore sees the same generated
`Envelope.ID`. Two separate publishes receive separate IDs even when their topic
and payload bytes are identical. Packet ID, topic, payload, QoS, and DUP are
never fallback identity inputs: packet IDs are reusable within an MQTT session
and none of those fields proves application-event identity.

**Who owns the ID namespace.** Cases 1 and 2 are producer-supplied, so whoever
may publish to the subscribed topics owns that source's envelope-ID space. MQTT
gives a subscriber no per-producer identity, so the bridge cannot attribute an
ID to one publisher on a shared topic — treat publish rights on a source as
rights over its identity space, and authorize accordingly.

The space is scoped to the source: IDs from different sources reach different
routes and bindings, and the outbox holds one record per
(partition, envelope ID, binding), so an ID is only ever compared with IDs from
the same source. Within one source, two publishes that reuse an ID are one
identity: the second is suppressed as an already-durable record and its delivery
is acked. That is correct for a redelivery and indistinguishable from a producer
reusing an ID for a different message, so every suppression is counted on the
`OutboxDuplicateSuppressed` metric, tagged with the route. A sustained non-zero
rate on one route means that source's producers are colliding — no other signal
distinguishes the two cases.

**Provenance is ingress-owned.** `x-bridge.generated-id` marks an
adapter-minted identity, and the adapter-internal `mqtt.generated-id` user
property that carries it between the callback and envelope construction is
stripped from every inbound publish before ingress decides whether to mint. A
publisher that sends `mqtt.generated-id` alongside its own stable
`mqtt.message-id` therefore cannot have that identity classified as unstable —
which would otherwise terminalize (DLQ or drop) its first transient failure.

**Binary correlation data round-trips.** Raw correlation bytes that cannot be a
header string are retained under the reserved `x-bridge.correlation-data`
envelope header as unpadded URL-safe base64, and egress decodes them back to
byte-identical MQTT Correlation Data. The header is internal-only, so it is
never emitted as a user property and no producer — on this transport or any
other — can set it. The retained bytes win on egress: the runtime stamps
`x-bridge.correlation-id` on every envelope that arrives without one, so
preferring the textual value would replace a binary producer's identity bytes
with a generated bridge id on every hop.

A broker redelivery without `mqtt.message-id` or correlation data may receive a
new ID and therefore duplicate downstream. MQTT cannot prove that a no-ID
publish is the same application event across reconnect and packet-ID reuse.
GoBridge deliberately accepts that at-least-once duplicate because delivering a
possible duplicate is safer than silently collapsing two legitimate equal-valued
publishes in `shared_outbox`. Producers that require stable deduplication across
redelivery must provide a stable `mqtt.message-id` (preferred) or correlation
identity and reuse it for every delivery attempt.

**Replay-cap consequence.** Because a no-ID publish is re-minted a
fresh envelope id on every broker redelivery, the runtime's replay ledger — which
keys count-less sources on that id — cannot accumulate attempts for it. The
adapter marks such an envelope as adapter-generated (`x-bridge.generated-id`,
an internal-only header that never leaves the process). On a transient delivery
failure the runtime therefore refuses to retry it: rather than recycling the
whole source session forever (a single poison message could head-of-line-block
all ingress), it terminally routes the message to the DLQ (or drops it, per
`OnPermanentFailure`) with reason `unstable_identity`. A producer that supplies
a stable `mqtt.message-id`/correlation data — or a trusted bridge-to-bridge
`x-bridge.dedup-id`/`x-bridge.idempotency-key` — restores countability and gets
the full `MaxReplayAttempts` retry budget.
