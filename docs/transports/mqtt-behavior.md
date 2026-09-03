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

The same guard bounds the one cap whose decode cost the wire does not bound. A
PUBLISH carrying more than 129 User Properties has its list cut to 129 on the
raw bytes before the SDK decodes it: the callback still sees a violation and
acks-and-drops the packet, but the decode never costs more than the retained
slot budgets, instead of the roughly 1.3 KiB per property the SDK would spend
on tens of thousands of five-byte properties. Every such packet is counted on
`MQTTIngressUserPropertiesTruncated`, and the count the publisher actually sent
is logged at Debug — the callback's Error log can only ever show 129.

### Bounded recovery from an unsettled delivery

A delivery the runtime received but never settled — a refused emit, a failed
`Delivery.Retry` — pins a broker Receive-Maximum slot until an unrelated
teardown. The bounded recovery that clears it, its per-mode policy (a resuming
session recycles; an ephemeral one acks, drops and records the loss), its
safety bounds and its metrics are documented separately:
[MQTT settlement recovery](mqtt-settlement-recovery.md).

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
[Envelope identity and no-ID redelivery](mqtt-ingress-headers.md#envelope-identity-and-no-id-redelivery).

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
  [Envelope identity and no-ID redelivery](mqtt-ingress-headers.md#envelope-identity-and-no-id-redelivery);
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
  broker's QoS-cap policy. A broker QoS **cap** fails every reconcile
  identically, so the same (filter, requested, granted) grant confirmed on three
  consecutive reconciles is treated as PERMANENT: the error then carries the
  permanent-closure marker and the session fails terminally instead of
  restarting into the identical downgrade forever (an exclusive owner would
  otherwise release and re-seize its lease every cycle). Lower the route's `qos`
  to the granted level, or lift the broker's cap.
- **A lost durable resume is signalled, not silently absorbed.** Persistent and
  Exclusive sessions dial `clean_start=false` because they want the broker to
  resume the subscriptions and the queued offline QoS 1/2 backlog. When CONNACK
  answers **Session Present=false** the broker had none — a session expiry
  elapsed during a long outage, a broker restart without persistence, or an
  exclusive standby connecting after `session_expiry_interval`. Re-subscribing
  then succeeds and readiness returns to Full, so the loss would be invisible:
  it counts `MQTTSessionResumeLost`, warns, and latches on `LastError` until the
  next converged reconcile. A cold start is exempt (nothing existed to lose);
  a non-empty managed subscription history makes even a first connect
  answerable, because it proves this `client_id` previously held broker-side
  filters. See
  [the broker-outage runbook](../runbooks/broker-outage-reconnect-storm.md).
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
  arrives before receivers register (see [Session Modes](mqtt.md#session-modes)). It has
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
   model](mqtt-options.md#ingress-byte-model)); the broker must also allow the window.
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

The  ingress header reference is on its own page: [MQTT ingress headers](mqtt-ingress-headers.md).
