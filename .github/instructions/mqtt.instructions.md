---
applyTo: "adapters/mqtt/**"
---

# MQTT transport (paho)

Adds to `adapters.instructions.md`. Sources: ADR-0002, ADR-0003, ADR-0009,
ADR-0010, ADR-0011, ADR-0019, ADR-0020, ADR-0021, ADR-0022 and
`docs/transports/mqtt*.md`.

## Settlement and ingress

- PUBACK / PUBCOMP is sent only after the runtime acks, and acks are released
  in receive order. That is what lets an in-flight message survive a crash on a
  Persistent or Exclusive session (`mqtt-behavior.md` §Settlement Semantics).
- Only local-cap violations may be acked and dropped: `max_payload_bytes`, the
  128 KiB metadata cap, or more than 128 User Properties, counted on
  `MQTTIngressPoisonDropped`.
- A malformed packet, or one larger than the advertised Maximum Packet Size, is
  rejected before Paho decodes it. The reject drops the connection, never the
  session. It counts `MQTTIngressRejected` and tries to send DISCONNECT 0x95 or
  0x81; that write has a 1 s deadline, and it is skipped while a Paho write
  holds the guard's write lock. The guard then closes the socket and lets
  autopaho reconnect, with a streak-keyed backoff penalty and `Ready` false
  until a replacement connection holds for `connectionStabilityWindow`. Flag
  any change that latches a terminal error on a pre-decode reject (ADR-0021).
- A settlement recovery that fails after its drain finished is abandoned
  (`abandonRecoveryAttempt`): it starts the rate-limit cooldown and latches no
  terminal error of its own. Session Present = false on a recovery connect is a
  resume loss (`MQTTSessionResumeLost`), not a failure. A failed drain, and a
  failure before the attempt reaches its drain, stay terminal, because old
  route work may still act. A fail-closed path that is terminal outside a
  recovery (for example managed-subscription cleanup in the recovery's
  reconcile) stays terminal inside one; the abandon then does nothing
  (ADR-0021).
- A session that fails closed — ingress that does not quiesce within
  `reconcile_timeout` before a recycle, a failed recovery drain, a recovery
  that fails before its drain — latches `shared.ErrTransportClosedPermanently`
  and never starts again (single-use). The Supervisor and the AWS runtime then
  rebuild its reload unit in place with a fresh session, unless the failure
  carries `ErrProcessRestartRequired` or no unit can be rebuilt (ADR-0020).
- MQTT has no NACK, so `Retry` means recycling the connection. A `Retry` that
  won is never followed by a protocol ack. QoS 0 and Ephemeral sessions return
  `ErrNotSupported` for `Retry` (`mqtt-settlement-recovery.md`).
- An emit rejection on Persistent/Exclusive QoS 1/2 is a bounded, rate-limited
  recycle; on Ephemeral QoS 1/2 it is ack, drop and record. Both go through
  `MQTTReceiverEmitRejected`. A stranded delivery pins a Receive-Maximum slot
  and wedges ingress.
- MQTT ingress is limited by counts only: `receive_maximum` per session (the
  configured value or 192; it sizes the dispatch queue and caps the messages
  waiting for a receiver) and `bridge.max_mqtt_sessions` per configuration
  ([configuration reference](../../docs/configuration-reference.md#bridge----bridge-settings)).
  Flag any new memory or byte-size estimate, budget or profile.
  `max_payload_bytes` is a protocol message-size limit announced as Maximum
  Packet Size, not an estimate (`mqtt-options.md` §Ingress limits are counts).

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
- The subscription-added hook reports only filters that were absent from the
  managed history when a reconcile first saw them and that a SUBACK then
  granted (a lower grant counts). A re-subscribe after a reconnect or restart
  never reports. It is called outside `s.mu`, from inside the reconcile, so it
  must not block or call back into the session (ADR-0019).

## Loop prevention and client IDs

- `no_local` defaults to off and applies only to ordinary subscriptions. It is
  never set on `$share/…`: MQTT 5 §3.8.3.1 makes that a Protocol Error and the
  broker disconnects (ADR-0010).
- `x-bridge.forwarded-from` / `x-bridge.forwarded-hop` are reserved but not
  enforced. Code and comments must not claim a hop limit exists.
- `client_id_suffix` is `hostname` or `nonce`. An unknown token or a failed
  hostname lookup fails the build, the nonce never falls back to a bare
  timestamp, and a suffix is rejected with `session_mode: exclusive`.
  Takeover damping (`connectionStabilityWindow`, capped `takeoverPenalty`,
  `MetricMQTTSessionTakeover`) stays (ADR-0011).

## Egress and rotation

- The sender publishes to `OutboundMessage.Address`, else `default_topic`.
  `Subject` travels as the `gobridge.subject` user property.
  `Sender.NonDurableEgress` stays true for QoS ≥ 1: durability comes from the
  route mode, not the protocol (ADR-0009).
- Credential rotation is commit-then-reconnect: `Session.ApplyCredentials`
  swaps `liveCreds` / `opts` under `s.mu`, then disconnects. Build-first would
  open a second concurrent connection with the same client ID (ADR-0002).

## MQTT 3.1.1 (ADR-0022)

- The translator (`mqtt311Conn`, `acl_mqtt311_conn.go`) is the only code that
  reads or writes the MQTT 3.1.1 wire format. Paho, the pre-decode guard, the
  router, delivery and reconcile keep MQTT 5 semantics. Above the translator,
  the protocol version is checked only to install the translator, validate,
  warn at startup, build `DurableSessionIdentity`, strip egress properties and
  damp short-lived connections. Flag any other version branch.
- A non-zero DISCONNECT is never written on 3.1.1: the translator writes
  nothing and the caller closes the socket, so the broker publishes the Last
  Will. Only reason `0x00` becomes a 3.1.1 DISCONNECT. A translator violation
  also closes the raw connection without a DISCONNECT.
- Clean Session derives from Clean Start and Session Expiry: Clean Start 1 with
  expiry 0 gives 1; Clean Start 0 with expiry above 0 gives 0; Clean Start 0
  with expiry 0 gives 1; Clean Start 1 with expiry above 0 is an error. A v5
  packet or field with no 3.1.1 form (AUTH, a topic alias, No Local, Retain As
  Published, a password without a username) fails the write and drops the
  connection; it is never silently weakened.
- Inbound 3.1.1 packets are validated exactly (fixed-header flags, exact
  lengths, CONNACK flags, non-zero packet ids, SUBACK codes, PUBLISH
  structure). A successful `packets.ReadPacket` decode is not 3.1.1
  validation.
- An oversized PUBLISH is truncated to `max_payload_bytes + 1` payload bytes
  and handed up before the rest is drained; the drain runs on the next `Read`.
  Never buffer the whole payload and never reject it: the router's cap acks and
  drops it (`MQTTIngressPoisonDropped`).
- The inbound in-flight set enforces `receive_maximum` and drops a live
  retransmission whose packet id is still in flight. An id leaves the set on
  PUBACK (QoS 1) or PUBCOMP (QoS 2), never on PUBREC. QoS 0 is not counted. A
  QoS 1 DUP copy that arrives after its PUBACK was written is dropped too,
  until the id is admitted again. A window overflow takes the violation path
  (`rejectPredecodeIngress`) before Paho sees the packet, so the read loop
  never blocks.
- On 3.1.1 the configuration is rejected (`shared.ErrInvalidConfig`) for
  `no_local`, a non-zero `session_expiry_interval`, a password without a
  username, and `clean_start: true` on a Persistent session. One validator,
  `validateProtocol`, runs on every path: `Config.Validate`,
  `ValidateEffectiveSession`, `Config.ApplyCredentials`, live
  `Session.ApplyCredentials` (before any mutation) and `NewSession` → `Start`.
- On 3.1.1 egress builds a publish with only topic, QoS, retain and payload,
  before field and packet-size validation.
- The protocol version is in `DurableSessionIdentity` on 3.1.1 only; a v5
  fingerprint must never change. `DurableSessionIdentityDomains` stays
  protocol-independent.
- On 3.1.1 a connection that drops within `connectionStabilityWindow` feeds the
  takeover streak and penalty, but never counts `MetricMQTTSessionTakeover`
  (ADR-0011).
