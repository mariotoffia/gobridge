---
applyTo: "adapters/mqtt/**"
---

# MQTT transport (paho)

Adds to `adapters.instructions.md`. Sources: ADR-0002, ADR-0003, ADR-0009,
ADR-0010, ADR-0011 and `docs/transports/mqtt*.md`.

## Settlement and ingress

- PUBACK / PUBCOMP is sent only after the runtime acks, and acks are released
  in receive order. That is what lets an in-flight message survive a crash on a
  Persistent or Exclusive session (`mqtt-behavior.md` §Settlement Semantics).
- Only local-cap violations may be acked and dropped: `max_payload_bytes`, the
  128 KiB metadata cap, or more than 128 User Properties, counted on
  `MQTTIngressPoisonDropped`. A malformed packet, or one larger than the
  advertised Maximum Packet Size, still fails the session closed.
- MQTT has no NACK, so `Retry` means recycling the connection. A `Retry` that
  won is never followed by a protocol ack. QoS 0 and Ephemeral sessions return
  `ErrNotSupported` for `Retry` (`mqtt-settlement-recovery.md`).
- An emit rejection on Persistent/Exclusive QoS 1/2 is a bounded, rate-limited
  recycle; on Ephemeral QoS 1/2 it is ack, drop and record. Both go through
  `MQTTReceiverEmitRejected`. A stranded delivery pins a Receive-Maximum slot
  and wedges ingress.

## Identity

- Envelope identity is `mqtt.message-id`, then correlation data (binary as
  `mqtt-bin:<base64url>`), then a UUIDv4 minted once per publish. It is never
  derived from packet ID, topic, payload, QoS or DUP — packet IDs are reused
  within a session (`mqtt-ingress-headers.md`).
- `mqtt.generated-id` is stripped from every inbound publish before the mint
  decision, and a minted ID is marked with `x-bridge.generated-id`.
- `x-bridge.correlation-data` is internal. It is never emitted as a user
  property; on egress it wins over `x-bridge.correlation-id`.

## Subscriptions (ADR-0003)

- Unmatched publishes are buffered for `DefaultUnmatchedGrace`. After grace, a
  covered QoS 1/2 topic is retained un-acked, never dropped.
- An orphan UNSUBSCRIBE is exact-topic, sent once per process, guarded by
  `topicCoveredLocked`, and bounded by `orphanUnsubscribeTimeout`.
- An empty plan unsubscribes the managed subscriptions it applied, and only
  those. Broker-only filters the bridge did not create are never touched.
- Managed history forgets a filter only on UNSUBACK `0x00` or `0x11`. A failed
  or partial unsubscribe stays durable. A pinned shared replay is never acked
  or dropped; it takes the migration-required fail-closed path
  (`mqtt-durable-sessions.md`).

## Loop prevention and client IDs

- `no_local` defaults to off and applies only to ordinary subscriptions. It is
  never set on `$share/…`: MQTT 5 §3.8.3.1 makes that a Protocol Error and the
  broker disconnects (ADR-0010).
- `x-bridge.forwarded-from` / `x-bridge.forwarded-hop` are reserved but not
  enforced. Code and comments must not claim a hop limit exists.
- `client_id_suffix` is `hostname` or `nonce`. An unknown token or a failed
  hostname lookup fails the build, the nonce never falls back to a bare
  timestamp, and a suffix is rejected with `session_mode: exclusive`.
  Takeover damping (`takeoverStabilityWindow`, capped `takeoverPenalty`,
  `MetricMQTTSessionTakeover`) stays (ADR-0011).

## Egress and rotation

- The sender publishes to `OutboundMessage.Address`, else `default_topic`.
  `Subject` travels as the `gobridge.subject` user property.
  `Sender.NonDurableEgress` stays true for QoS ≥ 1: durability comes from the
  route mode, not the protocol (ADR-0009).
- Credential rotation is commit-then-reconnect: `Session.ApplyCredentials`
  swaps `liveCreds` / `opts` under `s.mu`, then disconnects. Build-first would
  open a second concurrent connection with the same client ID (ADR-0002).
