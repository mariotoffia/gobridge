# MQTT behaviour

How the MQTT transport behaves at run time: message settlement,
reconnection, backpressure, shared subscriptions and ingress headers.

See [MQTT](mqtt.md) for sessions and a worked example, and
[MQTT options](mqtt-options.md) for the field-by-field reference.

---

## Settlement Semantics

> **QoS 2 is NOT exactly-once across a bridge restart.** autopaho keeps the
> outbound packet queue **in memory**, so an in-flight QoS 1/2 publish (sent,
> PUBACK/PUBCOMP not yet received) is lost at the MQTT-protocol level on a crash
> or restart — `client_id` / `clean_start=false` resume *broker-side* state, not
> the *client-side* outbound queue. Whether the wired delivery modes
> (`direct_hold`, `shared_outbox`) recover this loss on the source side is
> conditional. It depends on source redelivery, durable outbox persistence, and
> producer identity. See the [source-to-destination guarantee
> matrix](#source-to-destination-guarantee-matrix) below for the rows that are
> safe and the rows that can still lose or duplicate. Operators evaluating an
> end-to-end exactly-once claim must account for this — see also
> [ADR 0009](../adr/0009-durable-outbound-mqtt-session-state.md).

MQTT deliveries are acknowledged **after** the bridge settles them, not on
receipt. The adapter connects with manual acknowledgement and holds the PUBACK
(QoS 1) / PUBCOMP (QoS 2) until the runtime acks the delivery — after the
downstream send or outbox persist succeeds. Acks are released in receive order,
so an in-flight message survives a crash and is redelivered by the broker when
a Persistent/Exclusive session resumes.

### Ingress cap violations are acked-and-dropped (poison escape)

The one deliberate exception to ack-after-settlement: an inbound publish that
violates a **local representational cap** — `max_payload_bytes`, the ingress
metadata byte cap (128 KiB), or the User Property count cap (128) — while
fitting the CONNECT-advertised Maximum Packet Size. The broker enforces only
the whole-packet limit (`max_payload_bytes` + the 128 KiB metadata allowance),
so a compliant broker forwards such a packet from any authorized publisher.
The adapter **acks and drops it** (`MQTTIngressPoisonDropped`, Error log once
per violation class) instead of failing the session: an un-acked rejection
would be redelivered on every `clean_start=false` resume and latch the session
terminal forever — a publisher-triggerable permanent kill switch. The ack is an
acknowledged, counted loss of a message the bridge was configured to refuse;
alert on any non-zero value and follow
[the ingress-poison runbook](../runbooks/mqtt-ingress-poison.md). Malformed
packets and totals above the advertised Maximum Packet Size — producible only by
a broken broker — still fail the session closed at the raw pre-decode guard.

### Bounded recovery from an unsettled delivery

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
On a durable session the receiver requests the identical bounded, rate-limited
recovery recycle (Warn-logged), so the broker redelivers the stranded
delivery; each recycle redelivers **every** unsettled delivery on the session
(duplicates for innocent in-flight messages — absorbed by `shared_outbox`
dedup, unmeasured on `direct_hold`). A refused **QoS 0** delivery has no ack to
withhold and no redelivery, so it is lost; both cases count on
`MQTTReceiverEmitRejected`, separated by its `outcome` tag.

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
| `MQTTReceiverEmitRejected` | counter | Deliveries the route pipeline refused at emit. `outcome=recovering` is durable (QoS 1/2): left un-acked, redelivered by the recycle above. `outcome=lost` is QoS 0: no ack to withhold and no redelivery, so the message is gone. |

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

### Source-to-destination guarantee matrix

The delivery guarantee is conditional on five inputs: the source QoS/session, the
route delivery mode, whether the publish carries a producer identity, the outbox
store's durability and whether the record was persisted, and where the failure
falls relative to the Persist boundary, the envelope TTL, and the replay/poison
budget. "No source-side loss" means the bridge does not drop the message — not
exactly-once: an accepted-then-unconfirmed send can still duplicate at the
destination, so downstream idempotency is required in every row.

| Source QoS / session | Delivery mode | Producer identity | Outbox store & persist state | Persist / recovery boundary | Guarantee |
|---|---|---|---|---|---|
| QoS 1/2, Persistent/Exclusive | `direct_hold` | any | n/a (source-side hold) | resume within source session/queue expiry | No source-side loss: un-acked input is redelivered on resume and the in-flight publish is re-sent. |
| QoS 1/2, Persistent/Exclusive | `direct_hold` | any | n/a | resume after source session/queue expiry | Possible loss: the source broker dropped the queued input before the bridge resumed. |
| QoS 1/2, Ephemeral (clean start) | `direct_hold` | any | n/a | any restart | Possible loss: a clean-start restart cannot redeliver the unsettled input packet. |
| QoS 0, any session | `direct_hold` | any | n/a | any | Possible loss: QoS 0 has no source redelivery, so `direct_hold` has nothing to hold. |
| any source | `shared_outbox` | unique | durable store (SQLite/DynamoDB), **persisted** | envelope TTL not expired, within replay/poison budget | No source-side loss: once a uniquely-identified record is durably persisted, the outbox drainer replays it **independently of the source session** — the source QoS/session no longer matters. |
| QoS 1/2, Persistent/Exclusive | `shared_outbox` | any | durable store, crash **before** Persist | source redelivers on resume | No source-side loss: Persist precedes the source ack, so a crash before Persist leaves the source un-acked; it redelivers and the record is built and persisted on replay. |
| QoS 0 or Ephemeral (clean start) | `shared_outbox` | any | crash **before** Persist | no source redelivery | Possible loss: before a successful Persist there is no durable record, and a QoS 0 / clean-start source cannot redeliver. |
| any source | `shared_outbox` | unique | **volatile** store (in-memory; unit-test only, not production), "persisted" | process restart | Possible loss: an in-memory outbox does not survive a restart, so no record remains to replay. |
| QoS 1/2 | `shared_outbox` | **missing** (no producer ID) | durable, persisted | any | No silent collapse and no cross-redelivery dedup: each publish gets a fresh per-publish UUID, so two equal-valued events both flow and a broker redelivery of one publish duplicates it. |
| QoS 1/2 | `shared_outbox` | **reused** (same ID for distinct events) | durable, persisted | any | Collapse of a distinct event: the second event reuses the first's dedup key (`partition` + `EnvelopeID` + `binding`); its Persist returns `ErrDuplicateRecord` and is acked-and-dropped. A supplied producer ID is preserved and trusted as identity. |
| QoS 1/2, source broker offline | either | any | n/a | source queue/session expiry or capacity drop before receipt | Possible loss: the source broker can expire or drop its offline/session queue before the bridge ever receives the message. |
| any | `shared_outbox` | any | durable, persisted | `ReplayCount` > `MaxReplayAttempts` **and** `ReplayBudget` elapsed since first attempt | Permanent failure: the record reaches the terminal action below. A record whose envelope TTL passes is expired first, per `OnExpired`. |
| any (stable identity) | `direct_hold` | present, or bridge dedup/idempotency key | n/a | source attempts reach `MaxReplayAttempts` (count only, no wall-clock gate) | Permanent failure: the source delivery reaches the terminal action below. Count-less sources are counted by the bridge-owned replay ledger keyed on the stable identity. |
| QoS 1/2 (no stable identity) | `direct_hold` | **missing** (no producer ID) | n/a | first transient failure | Permanent failure on the first failure: a count-less source with an adapter-generated id cannot be counted (each broker redelivery mints a fresh id), so it is terminally DLQ'd/dropped with reason `unstable_identity` rather than recycling the source session forever. Supply `mqtt.message-id`/correlation data to get the full `MaxReplayAttempts` budget. |
| any | `direct_hold` | any | n/a | terminal action after permanent failure/expiry | Per `OnPermanentFailure`/`OnExpired` (default `dlq`): on a successful DLQ write (counts `MetricDLQEntries`) the **source delivery** is settled/ACKed; `drop` (or no DLQ store) records a drop metric and ACKs the source — loss by design. A DLQ **write failure** leaves the source **unsettled**, so it redelivers — never a silent drop. |
| any | `shared_outbox` | any | durable, persisted | terminal action after permanent failure/expiry | The source was already ACKed right after Persist. Per `OnPermanentFailure`/`OnExpired`, the drainer **completes the outbox record** (`OutboxStore.Complete`) only after a successful DLQ write (`MetricDLQEntries`) or a recorded `drop` (loss by design). A DLQ **write failure** leaves the record **pending/claimed**, so the drainer retries it — never a silent drop. |
| any | either | any | any | send accepted, response lost | Ambiguous: a send timeout after the destination accepted the publish is indistinguishable from a real failure, and a retry may duplicate. Downstream must dedupe. |

Producer identity is a stable `mqtt.message-id` (or MQTT correlation data); a
content hash of topic+payload is **not** a producer ID, because two legitimate
equal-valued events would hash the same and one would be silently collapsed. A
**reused** producer ID has the same effect from the other direction: a supplied
ID is trusted as identity, so a distinct event carrying a duplicate ID collapses
into the first record. When no producer ID is present, GoBridge stamps a fresh
per-publish UUID so distinct publishes stay distinct — see
[Envelope identity and no-ID redelivery](#envelope-identity-and-no-id-redelivery).

`shared_outbox` durability is only as strong as the store behind it, and it
protects a record only from the moment Persist succeeds. Before that point there
is nothing durable: an over-capacity or unavailable store makes Persist fail or
block, so the source delivery is retried or DLQ'd — no persisted work is lost,
because none exists yet. After a successful Persist, a pending record is **not**
subject to a store-retention TTL. The production SQLite and DynamoDB stores and
the in-memory fake never evict a pending or claimed record; retention only
compacts terminal (completed/expired) records. A durably persisted record is
instead bounded by:

- **envelope expiry** — if the envelope carries a TTL, the drainer skips and
  expires it once past due (`MetricOutboxExpiredBeforeSend`, or a bulk expiry
  sweep) rather than sending it late;
- **replay / poison budget** — the drainer poisons a record to the DLQ only when
  its `ReplayCount` exceeds `MaxReplayAttempts` **and** the wall-clock
  `ReplayBudget` (default 15 minutes, measured from the first attempt) has
  elapsed; a legacy record with no first-attempt timestamp falls back to the
  `CreatedAt`/`poisonMinAge` age gate. `direct_hold` instead poisons the source
  delivery on the attempt-count cap alone — counting count-less sources through
  the bridge-owned replay ledger keyed on a stable identity. A count-less source
  that supplies **no** stable identity (adapter-generated envelope id) cannot be
  counted, so instead of looping forever it is terminally sinked on the first
  transient failure (`unstable_identity`); see
  [Envelope identity and no-ID redelivery](#envelope-identity-and-no-id-redelivery);
- **store durability loss** — a volatile (in-memory) store on restart, or
  operator deletion / row corruption of a durable store.

What the bridge **records** is the settlement and outbox state it observes:
`unsettled_count`, outbox record status, DLQ entries, and drop counters. What it
**cannot know** is whether a message sat in a broker's offline queue before the
bridge connected, or whether a destination that never returned a response
committed the publish. Those unknowns are why the matrix labels those rows
possible-loss or ambiguous rather than safe.

The only way an in-flight loss becomes bridge-level loss outside the rows above is
a delivery mode that acks the source *before* the transport confirms the publish.
No such mode exists today, so the bridge emits a route-aware startup advisory
(`bridge.egressDurabilityAdvisory`) that stays silent for both current modes and
exists only to flag such a future mode.

## Resilience Behavior

- **Publish timeout — route policy vs. sender timeout.** The sender applies the
  **stricter** of `options.sender.timeout` and the caller's remaining context
  deadline. On a bridge route the dispatcher always wraps each send in the route's
  `policy.send_timeout` (default 30s), so that deadline is the ceiling: a shorter
  `sender.timeout` tightens the publish (useful for a route that must fail fast
  to a slow broker), a longer one is capped and does not extend it. The
  60-second safety-net fires **only** when there is no caller deadline at all —
  a direct library consumer calling `Send` outside a route dispatcher with
  `timeout` at `0`.
- **Error classification is type-driven, not text-driven.** `MapError`
  (`errors.go`) classifies exclusively on typed values, so an SDK upgrade cannot
  silently change retry behavior by rewording a message:
  - a server CONNECT denial (`*autopaho.ConnackError`) classifies from its
    MQTT v5 reason code through the same table as DISCONNECT — e.g. `0x88`
    → `ErrUnavailable`, `0x86`/`0x87` → `ErrNotAuthorized`, `0x89` →
    `ErrBrokerBusy`;
  - the SDK's link-down sentinels (`autopaho.ConnectionDownError`,
    `paho.ErrConnectionLost`) → `ErrConnectionLost`;
  - `paho.ErrInvalidArguments` → `ErrProtocolError` (**permanent** — autopaho
    itself refuses to retry these, so the bridge must not either);
  - dial and I/O failures arrive as `*net.OpError` and classify through
    `net.Error`: `Timeout()` → `ErrTimeout`, otherwise `ErrConnectionLost` —
    covering `connection refused`, `no route to host` and `network unreachable`
    by type rather than by text;
  - anything unrecognized → `ErrUnavailable` (transient).
- **Ingress properties are session-owned copies.** The router converts incoming
  MQTT Properties into an owned envelope before dispatch. Config-driven
  composition binds at most one receiver per session, so no route shares its
  dispatch or acknowledgment domain.
- **Password rotation rebuilds the session.** A rotated password calls
  `Session.Reload`, which rebuilds the connection manager so a fresh CONNECT
  carries the new credentials. It does **not** call
  `ConnectionManager.Disconnect`: in paho.golang v0.23.0 that cancels the CM
  root context and is terminal -- the client never reconnects and `Health()`
  would still report the session up. TLS material rotates through the same
  `Reload` path. See [Credential Rotation](../credentials-rotation.md).
- **Rotation during an outage recovers on its own.** A rotation `Reload` that
  fails because the broker is unreachable signals terminal death; the runtime
  supervisor re-Starts the session with jittered backoff, so it reconnects by
  itself once the broker returns.
- **Granted-QoS downgrade is surfaced.** A SUBACK below the requested level
  silently removes offline/redelivery coverage while the route still assumes the
  requested guarantee, opening a disconnect-gap loss window. The reconcile loop
  keeps the requested QoS as its delta baseline (a stable downgraded sub is not
  re-subscribed every cycle) and counts `MQTTQoSDowngraded` with a loud warning
  once per subscription transition. Any non-zero value warrants checking the
  broker's QoS-cap policy.
- **Retained replay is suppressed on reconnect.** Persistent and Exclusive
  re-subscribes carry MQTT 5 **Retain Handling = 1** ("send retained only if the
  subscription did not already exist"), so a resuming `clean_start=false` session
  is not flooded with a retained replay for every filter on every reconnect.
  Ephemeral sessions use Retain Handling = 0: each connect is a fresh
  subscription with no prior broker-side state, so the retained snapshot is the
  intended first delivery.

## Backpressure and dispatch

The publish callback paho invokes must return quickly or the client stops
servicing PINGRESP/PUBACK and the connection dies of keepalive starvation. The
adapter therefore hands each inbound publish to a serialized dispatch queue and
returns:

- The **dispatch queue** and the pending buffer below share one admission budget
  sized to the effective `receive_maximum` (default **192**). When it is full a
  **QoS 0** publish is dropped (`MQTTRouterDropped`, logged with the refusing
  bound — an exhausted budget and a full pending buffer have different remedies).
  A **QoS 1/2** publish is never dropped: it reclaims the oldest pending QoS 0's
  slot, and waits only when the budget is entirely QoS 1/2, where the broker's
  Receive-Maximum window bounds the wait. Waiting behind QoS 0 instead would
  stall the callback goroutine that also reads PINGRESP, and the broker's window
  excludes QoS 0, so nothing would relieve it.
- The **pre-registration pending buffer** absorbs the CONNACK backlog that
  arrives before receivers register (see [Session Modes](#session-modes)). It has
  two independent bounds applied asymmetrically by QoS: an entry-count cap sized
  to `receive_maximum` (default **192**) and a **64 MiB** payload ceiling
  (`defaultPendingBytesLimit`). The byte ceiling governs **QoS 0 only**. A QoS 0
  publish over either cap is dropped (`MQTTRouterDropped`) — it carries no
  delivery contract. A **QoS 1/2** publish is never dropped for the byte ceiling:
  it evicts the oldest QoS 0 entry to reclaim memory and buffers regardless,
  bounded by the count cap. QoS 1/2 memory needs no byte cap because the broker's
  Receive-Maximum flow control never delivers more than `receive_maximum` un-acked
  QoS 1/2 at once; the complete packet/window allocation is covered by the
  validated ingress byte model above. The single path that drops a QoS 1/2
  publish is the count cap hit with no QoS 0 left to evict — reachable only when
  a broker exceeds the Receive Maximum it was granted (a protocol violation).
  That publish is acked-and-dropped (which keeps paho's in-order ack stream
  draining) and counted on `MQTTRouterOverflowDropped`, so any non-zero value
  points at a broker bug, not operator mis-sizing. Publishes held in the buffer
  count on `MQTTRouterBuffered`.

### Capacity sizing

Sustained QoS 1/2 ingress throughput is bounded by the un-acked in-flight
window and how fast the bridge settles:

```
max sustained msg/s ≈ receive_maximum / avg settlement latency (s)
```

where settlement latency is the route's end-to-end accept time — outbox
persist for `shared_outbox`, target accept for `direct_hold`. With the default
`receive_maximum: 192` and a 20 ms settlement, that is ~9,600 msg/s per
session; a 200 ms downstream caps the same session at ~960 msg/s. Levers, in
order:

1. **`receive_maximum`** — widens the in-flight window; memory cost is
   `receive_maximum × max_payload_bytes`-shaped and validated against
   `ingress_memory_budget_bytes` (see the [ingress byte
   model](#ingress-byte-model)); the broker must also allow the window.
2. **Route `max_in_flight`** — concurrency downstream of dispatch; raising it
   reduces settlement latency until the target saturates. It participates in
   the same validated memory budget.
3. **`max_payload_bytes`** — smaller payloads let the same memory budget hold
   a larger window (`ConfigureIngressMemory` derives the largest safe
   `receive_maximum` automatically when it is left unset).

QoS 0 is not flow-controlled by `receive_maximum`: a QoS 0 flood sheds at the
dispatch queue (`MQTTRouterDropped`) rather than backpressuring the broker.
Watch `MQTTReceiveWindowUtilization` (sustained → 1.0 means the window, not the
network, is the ceiling) and `MQTTOldestUnsettledAge` (rising means the
downstream, not MQTT, is the bottleneck).

The dispatch queue, broker receive window, route concurrency, current packet,
whole-packet ceiling, and runtime bookkeeping are all included in the validated
byte bound. A non-compliant broker can still put one decoded packet in the SDK
before the callback sees it, but an oversize body is rejected before the adapter
copies or enqueues it; QoS 1/2 remains unacknowledged, preserving
at-least-once semantics.

## Shared subscriptions (`$share`)

MQTT declares the `shared_consumer` capability. A subscription filter of the
form `$share/<group>/<filter>` is a shared subscription: the broker
load-balances the topic's deliveries across every client in `<group>`, so
several bridge instances (or several receivers) consume one logical subscription
as a scale-out group instead of each receiving a full copy.

Declare the shared filter in the receiver's `topics[]` exactly as the broker
expects it (`$share/<group>/<filter>`). The adapter strips the `$share/<group>/`
prefix before matching, so routing keys off the concrete topic the broker
delivers on, not the `$share` wrapper. Ordinary (non-shared) subscriptions are
unaffected.

### Each scale-out instance needs a UNIQUE `client_id`

Shared-subscription scale-out and `client_id` interact in a way that is easy to
misconfigure into a self-DOS. The broker load-balances a `$share` group across
**distinct sessions**, and a session is keyed by `client_id`. So:

- **Scale-out (multiple active consumers): give every replica a UNIQUE
  `client_id`.** Two live instances that reuse one `client_id` are, to the
  broker, the *same* session — the second connect triggers a **session
  takeover** (MQTT `0x8E`) that kicks the first off, which reconnects and kicks
  the second off, and so on. The result is a reconnect storm that consumes
  nothing (self-DOS), not load-balancing. Use `session_mode: ephemeral` (unique
  `client_id` + `clean_start=true`) and give each replica a distinct `client_id`
  — set `client_id_suffix: hostname` (or `nonce`) so one shared config file still
  yields a unique id per pod (recipe below).
- **A shared/stable `client_id` is only safe behind an exclusive lease.** In
  `session_mode: exclusive`, one holder connects at a time (the lease
  guarantees it), so a stable `client_id` is correct and a takeover is a
  legitimate lease **failover**. But with a single active holder a `$share`
  group has exactly one member, so it **serialises** rather than scales the
  subscription.

The adapter cannot see the other replicas' `client_id`s from one process, so it
**detects the symptom**: when `$share` subscriptions are configured on a
non-Ephemeral session it warns once about the unique-`client_id` requirement,
and a session takeover while `$share` is active (outside Exclusive mode) is
logged at **Error** on the first occurrence — that combination is the
smoking-gun of a reused `client_id`. `MQTTSessionTakeover` counts every
takeover; a persistent non-zero rate on a `$share` deployment means the
`client_id`s are colliding.

#### Recipe: unique `client_id` per replica from one config file

A Kubernetes Deployment or ECS service scales one config (ConfigMap / task
definition) to `replicas: N`, so every pod reads the **same** `client_id` — the
self-DOS above. `client_id_suffix` resolves it at build time without per-pod
templating:

```yaml
sessions:
  - id: telemetry-in
    transport: mqtt
    session_mode: ephemeral        # scale-out, NOT exclusive
    options:
      session:
        broker_url: tls://mqtt.prod.example.com:8883
        client_id: telemetry-consumer   # shared base in the ConfigMap
        client_id_suffix: hostname       # -> telemetry-consumer-<pod name>
        clean_start: true
```

- `hostname` appends the container/pod hostname (`telemetry-consumer-web-7d9f-abc12`).
  On K8s the pod name is already unique and stable for the pod's life, and it
  shows up verbatim in broker logs — prefer it when the hostname is distinct
  (the default for Deployments and StatefulSets).
- `nonce` appends 8 hex characters from `crypto/rand`, unique per **process**.
  Use it when replicas do not have distinct hostnames (some flat container
  networks) — but note a pod restart yields a *new* id, so the broker sees a new
  ephemeral session rather than a resumed one (fine for `clean_start: true`).

> **Do not set `client_id_suffix` on an exclusive session.** Exclusive failover
> needs a *stable shared* `client_id` so the standby can resume the dead owner's
> broker session; a per-instance suffix would strand queued QoS 1/2 messages on
> every failover. The build **rejects** `client_id_suffix` when `session_mode:
> exclusive`. See [scenario 08](../scenarios/08-clustered-exclusive-sessions.md).

## Ingress headers (`mqtt.*`)

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

