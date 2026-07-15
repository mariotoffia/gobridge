# 0009 — Durable outbound MQTT session state

Status: accepted
Date: 2026-07-13
Deciders: GoBridge core
Implemented: commit 4d8d76d (2026-07-13); the `NonDurableEgressReporter` boundary landed in commit 9d8effb (2026-07-10)

## Context

The paho MQTT adapter connects through autopaho with `cfg.Session` left nil, so
the connection manager uses autopaho's **default in-memory** packet/session store
(`adapters/mqtt/transport/paho/acl_session.go`, the `M-6` note on the
`autopaho.ClientConfig` build). That store holds the client-side outbound queue:
a QoS 1/2 PUBLISH that has been sent but whose PUBACK (QoS 1) / PUBCOMP (QoS 2)
has not yet arrived lives only in process memory.

The consequence is a hard ceiling: an **in-flight** outbound QoS 1/2 publish is
**lost at the MQTT-protocol level** when the process dies, and MQTT QoS 2 is
therefore **not exactly-once across a restart**. `client_id` / `clean_start=false`
do not close this — they resume *broker-side* session state (offline inbound
queueing, subscription retention), not the *client-side* outbound packet queue,
which is the volatile part.

This is easy to mistake for bridge-level message loss. It is not, because the
bridge does not delegate egress durability to the MQTT protocol.

## Decision

**Keep the in-memory autopaho store and make durable, at-least-once egress the
bridge's responsibility at the route layer, not the transport's.** The MQTT
sender is a non-durable boundary by design and it *declares* that fact so the
runtime can reason about it.

- **Non-durability is reported, not hidden.** The sender implements
  `ports.NonDurableEgressReporter`: `Sender.NonDurableEgress` returns true for
  QoS ≥ 1 (`adapters/mqtt/transport/paho/sender.go`). The bridge consults it in
  `egressDurabilityAdvisory` and warns **only** when a route's delivery mode
  would settle the source *before* this non-durable boundary.

- **The route delivery modes carry durability, under stated conditions.** Both
  wired modes recover an in-flight-publish crash on the source side, but each
  only within its own boundary:
  - `direct_hold` holds the source delivery un-acked until the broker returns
    PUBACK/PUBCOMP; a crash before that ack leaves the source message un-acked,
    so the source redelivers **when the source transport/session redelivers**. A
    QoS 0 source, an Ephemeral clean-start restart, or an expired source offline
    queue has nothing to redeliver.
  - `shared_outbox` invokes the sender from a version-fenced persisted outbox
    record and marks it complete only **after** the send returns; a crash before
    completion replays the record **when it acquired a unique durable identity
    and was persisted**, and idempotency keys collapse the duplicate.
  Because both modes recover the in-flight loss on the source side within those
  boundaries, no durability advisory fires today. The full conditional contract —
  the rows that are safe and the rows that can still lose or duplicate — is the
  [source-to-destination guarantee matrix](../transports/mqtt.md#source-to-destination-guarantee-matrix).

- **The stance is documented for operators.** The MQTT transport page states
  prominently that QoS 2 is not exactly-once across a restart and that durability
  comes from the route mode, not the protocol
  ([transports/mqtt.md](../transports/mqtt.md#settlement-semantics)).

## Consequences

- A bridge restart does not lose an in-flight publish **within each mode's
  boundary**: the source-side hold re-sends it when the source redelivers, and
  the outbox replay re-sends a persisted uniquely-identified record, with the
  dedup layer collapsing the redelivered duplicate. MQTT-protocol QoS 2
  exactly-once is *not* preserved across the restart; within the boundary it
  degrades to a collapsed duplicate. Outside the boundary — QoS 0, an Ephemeral
  clean-start source, a source offline-queue expiry, a crash before Persist, or
  an explicit drop policy — loss is possible, and an accepted-but-unconfirmed
  send can duplicate. The [guarantee
  matrix](../transports/mqtt.md#source-to-destination-guarantee-matrix) is the
  source of truth for which row applies.
- An operator evaluating an end-to-end exactly-once claim must account for the
  in-memory store: the guarantee is at-least-once-plus-dedup, delivered by the
  route mode, not exactly-once at the MQTT protocol.
- A delivery mode that acked the source *before* the transport confirmed the
  publish would turn an in-flight loss into bridge-level loss. No such mode
  exists today; adding one MUST either make this store durable or refuse to pair
  with the non-durable MQTT sender (the advisory is the guard rail).
- The memory footprint of the outbound queue is bounded by the broker's
  Receive-Maximum window, so the in-memory store carries no unbounded-growth
  risk.

## Rejected alternatives

- **File-backed autopaho `session.SessionManager` (durable client-side store).**
  Assigning a persistent store to `cfg.Session` would make in-flight outbound
  QoS 1/2 survive a restart at the protocol level. Deferred, not rejected: it
  adds a local-disk durability dependency and a crash-consistency surface that
  the route-layer outbox already covers within its boundary (a persisted,
  uniquely-identified record), so it buys protocol-level exactly-once we do not
  currently need. It remains the natural extension if a
  future route mode settles the source ahead of the transport. Tracked as the
  deferred `M-6` alternative.
- **Advertise MQTT QoS 2 as the durability guarantee.** Rejected — the in-memory
  store makes that claim false across a restart, and it would invite operators to
  drop the route-layer outbox that actually provides the guarantee.
- **Silently accept in-flight loss as acceptable.** Rejected — the loss is only
  acceptable *because* the route mode recovers it; leaving that implicit would
  let a future ack-early delivery mode reintroduce real loss unnoticed. Reporting
  non-durability through `NonDurableEgressReporter` makes the boundary explicit
  and machine-checkable.
