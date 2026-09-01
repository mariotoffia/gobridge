# MQTT settlement recovery

How the MQTT transport recovers a delivery that was received but never
settled — a refused emit, a failed `Delivery.Retry` — without wedging ingress
on the broker's Receive-Maximum window.

Split out of [MQTT behaviour](mqtt-behavior.md#settlement-semantics), which
remains the entry point for settlement semantics, the guarantee matrix,
resilience, backpressure and ingress headers.

---

A successful `Delivery.Retry` on a QoS 1/2 delivery has transport-specific
semantics because MQTT has no per-message NACK. On a **Persistent** or
**Exclusive** session, Retry leaves the PUBLISH protocol-unsettled and requests
one connection recycle. Ack and Retry are mutually exclusive and idempotent: a
Retry that wins can never be followed by a protocol Ack for that delivery. QoS 0
and every Ephemeral-session Retry remain `ErrNotSupported` because a reconnect
cannot redeliver them safely.

A receiver **emit error** (the route runner refuses the delivery outright)
takes the same path: the un-acked delivery would otherwise be stranded — MQTT
brokers do not redeliver on a live connection — pinning a Receive-Maximum
slot until an unrelated teardown, and wedging ingress as strands accumulate.
What can be done about it is decided by whether the broker session RESUMES:

- **QoS 1/2 on a Persistent or Exclusive session** — the receiver requests the
  identical bounded, rate-limited recovery recycle (Warn-logged), so the broker
  redelivers the stranded delivery. Each recycle redelivers **every** unsettled
  delivery on the session (duplicates for innocent in-flight messages —
  absorbed by `shared_outbox` dedup, unmeasured on `direct_hold`). Counted
  `outcome=recovering`.
- **QoS 1/2 on an Ephemeral session** — `clean_start=true` discards the
  server-side session at the next disconnect, so no recycle can ever bring the
  delivery back. Withholding the acknowledgement would buy no recovery and pin
  a Receive-Maximum slot for the whole life of the connection, so the bounded
  policy is the one QoS 0 already gets: **ack, drop, and record the loss**
  (Warn-logged, `outcome=lost`). The receive window recovers immediately; the
  message is gone. A deployment that cannot accept that loss must use a
  Persistent or Exclusive session for its QoS 1/2 ingress.
- **QoS 0** — no acknowledgement to withhold and no redelivery, so it is lost.
  Counted `outcome=lost`.

All three count on `MQTTReceiverEmitRejected`, separated by its `outcome` tag.

Recovery applies these safety bounds without introducing a recovery-specific
config knob:

- readiness drops below Full synchronously when Retry queues recovery. Queueing
  immediately arms Session Present enforcement: any subsequent ConnectionUp with
  `Session Present=false` irreversibly fails that recovery, even before the
  worker owns the gate. The request carries no active-attempt or target-epoch
  evidence; the worker publishes those only after acquiring the session gate, so
  an ordinary reconcile that wins first cannot validate or abort it;
- concurrent requests coalesce into one recycle;
- the router stops accepting new callbacks (bounded by `reconcile_timeout`), then
  waits for already-accepted settlements under **no adapter-local bound** — each
  is bounded by its own route's send-wedge, processor and store ceilings, so a
  tighter bound here would misread cooperative slowness as an unrecoverable drain
  failure and restart unrelated routes. The hard deadline below is the outer one.
  Ingress is black-holed for the length of that drain (the recycle is about to
  discard it anyway); QoS 1/2 is redelivered by the resumed session, QoS 0 in the
  window is lost, and both are counted on `MQTTRouterStalePurged`. Only settlement
  recovery drains this way — a reconcile-driven recycle (managed-subscription
  cleanup, failed-reconcile teardown) keeps `reconcile_timeout` on both phases,
  because those callers can run on a context with no deadline of their own;
- completed recovery attempts are spaced by at least **30 seconds**, using the
  session clock, to prevent a DLQ-outage reconnect storm;
- ordinary reconciliation, credential/TLS reload, managed cleanup, orphan
  cleanup and settlement recovery share one context-aware session serialization
  gate. Every public entry acquires it with its own context; private helpers
  never reacquire it, so there is no nested wait or ABBA lock order. A caller
  cancelled while queued leaves promptly;
- one hard deadline covers waiting for that gate, the settlement drain,
  disconnect, reconnect, and replacement-generation reconcile. It reuses the
  post-acquire activation timing derived from `connect_timeout`,
  `reconcile_timeout` and `unmatched_grace`; there is no duplicate setting;
- the rebuild preserves `client_id` and session expiry, forcing `clean_start=false`;
- CONNACK must report **Session Present**, or the broker cannot prove the
  unsettled packet survived. That evidence is stamped with the exact connection
  epoch; recovery captures its target epoch after reconnect and rejects any
  other. Session Present alone is not completion: readiness stays degraded until
  exact-epoch replacement reconciliation succeeds in the same deadline;
- every queued or active recovery failure (gate timeout/cancellation, drain,
  disconnect/reconnect, Session Present, or reconcile) enters one idempotent
  terminal transition. It clears pending attempt state, latches a permanent
  error, quiesces ingress, disconnects the generation within the activation
  bound, emits one terminal SessionError, then closes the lifecycle channel. One generation-scoped drain state (`not-started`, `in-progress`,
  `finished`) gives exactly one owner the settlement barrier: terminal teardown
  starts it only from `not-started`, joins the same signal while `in-progress`,
  and disconnects immediately once `finished` — so a Session Present failure
  before or during the drain can neither start a second drain nor signal the
  manager ahead of the shared barrier. The manager tears down before releasing an
  exclusive lease; its supervisor retries once, and the single-use contract then
  escalates `ErrSessionUnrecoverable` for orchestrator replacement. Future Retry,
  Reconcile, credential and Start calls return the terminal error rather than
  reactivating the dead Session instance.

The adapter tracks every current-connection QoS 1/2 packet from receipt until a
successful protocol Ack or connection-epoch change. Deep health exposes
`unsettled_count`, `oldest_unsettled_age_ms`, `receive_window_utilization`, and
`recovery_recycle_count`. The corresponding metrics are:

| Metric | Kind/unit | Meaning |
|---|---|---|
| `MQTTUnsettled` | gauge, packets | Current-epoch QoS 1/2 packets awaiting protocol settlement. |
| `MQTTOldestUnsettledAge` | gauge, seconds | Age of the oldest current-epoch unsettled packet. |
| `MQTTReceiveWindowUtilization` | gauge, ratio | `unsettled_count / receive_maximum`; sustained values near 1 indicate ingress is close to flow-control exhaustion. |
| `MQTTSessionRecoveryRecycle` | counter | Actual recycle attempts started after acquiring the session gate. Queue timeout/cancellation before recycle does not increment. |
| `MQTTReceiverEmitRejected` | counter | Deliveries the route pipeline refused at emit. `outcome=recovering` is QoS 1/2 on a session that resumes (Persistent/Exclusive): left un-acked, redelivered by the recycle above. `outcome=lost` is anything nothing can redeliver — QoS 0, or QoS 1/2 on an Ephemeral session — and is ACKED so it stops pinning the receive window. |

All of these use the existing `session_id` tag, plus `outcome` on
`MQTTReceiverEmitRejected`. Message IDs, topics, and failure reasons are
deliberately not dimensions, so cardinality remains bounded.

**Ephemeral sessions have a loss window.** An Ephemeral session keeps no offline
retention: during any disconnect the broker queues nothing for it, so messages it
would have delivered are lost with no redelivery on reconnect, and a runtime
reconfig swap leaves an unavoidable delivery gap. Persistent and Exclusive
sessions (`clean_start=false` with a non-zero `session_expiry_interval`) close
the offline and reconfig gaps — the broker queues inbound deliveries while the
client is away and redelivers them on resume — but no mode makes OUTBOUND
in-flight QoS 1/2 state durable: it lives only in memory and a bridge restart or
crash loses it.

**MQTT QoS 1/2 alone is not durable egress, and neither wired delivery mode is
unconditionally loss-proof.** autopaho keeps the outbound packet queue **in
memory**, so a publish that is in flight (sent, PUBACK/PUBCOMP not yet received)
when the process dies is lost at the *protocol* level, and MQTT QoS 2 is **not**
exactly-once across a restart. `client_id` / `clean_start=false` do not help —
they resume *broker-side* state, not the *client-side* outbound queue. What each
delivery mode recovers, and the conditions it depends on, is stated in the
matrix below:

- **`direct_hold`** (the default) holds the source delivery un-acked until the
  broker returns PUBACK (QoS 1) / PUBCOMP (QoS 2). It recovers an in-flight loss
  **only when the source transport/session redelivers** the un-acked
  input. A QoS 0 source, an Ephemeral clean-start source that restarts, or a
  source broker that already expired its offline queue has nothing to redeliver,
  so there is no held recovery.
- **`shared_outbox`** invokes the sender from a version-fenced persisted outbox
  record and marks it complete only **after** the send returns. It recovers a
  loss **only for records that acquired a unique durable identity and were
  successfully persisted** before the crash. A record that never reached the
  store (crash before Persist) has nothing to replay, and two legitimate
  equal-valued publishes that lack a producer ID are not the same record.

Neither mode proves that a broker-retained message existed before the bridge
received it, and neither turns an ambiguous send into a certainty. Pair
loss-sensitive egress with `shared_outbox` (or the redelivery-backed
`direct_hold`), keep producers stamping a stable `mqtt.message-id`, and keep the
downstream idempotent. A file-backed Paho session store is a deferred
alternative, not wired today — see
[ADR 0009](../adr/0009-durable-outbound-mqtt-session-state.md).
