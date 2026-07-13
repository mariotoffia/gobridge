# 0010 — MQTT bridge-to-bridge loop-prevention contract

Status: accepted
Date: 2026-08-14
Deciders: GoBridge core

## Context

A message bridge that both subscribes and publishes on the same broker can feed
its own output back into its input. The sharpest case is a **single session on a
single broker** whose subscription filter overlaps its publish topic: MQTT
delivers every publish back to the same client that sent it, the bridge
re-forwards it, and the loop amplifies without bound — a self-inflicted broker
meltdown that no downstream dedup can absorb, because each turn of the loop mints
a new message.

A second, looser case is a **cross-bridge relay** (`bridge → broker → bridge →
broker → …`) where two or more bridges relay each other's traffic in a cycle.
Here the messages carry bridge-stamped provenance, so the loop is in principle
observable, but the reserved-header trust model complicates it: every `x-bridge.*`
key is stripped from untrusted transport input at ingress
([ADR 0001](0001-reserved-header-trust-model.md), `StripReservedHeaders`), so a
provenance marker only survives a hop if the bridge lifts it back in through a
typed field the way [ADR 0008](0008-cross-hop-identity-lift.md) lifts the
identity keys.

The bridge needs a stated position on both cases rather than leaving loop
avoidance entirely to operator topology design.

## Decision

**Break the dangerous same-session loop at the transport with an opt-in MQTT 5
No-Local flag; treat the cross-bridge relay as an operator-topology and
dedup-layer concern, with the reserved-header namespace reserved for a future
hop-provenance contract.**

- **Same-session self-delivery — opt-in `no_local`, default off.** When the
  session sets `no_local: true`, every **ordinary** subscription is issued with
  the MQTT 5 No-Local flag (`subscribeSpec.NoLocal`, threaded through
  `session_reconcile.go` into `pahoConn.Subscribe`), so the broker never delivers
  a message back to the session that published it. The default is **off** to
  preserve the least-surprising MQTT contract — a session receives its own
  publishes — because legitimate single-session round-trip topologies depend on
  it, and turning No-Local on unconditionally broke self-delivery integration
  paths. Operators who bridge overlapping MQTT filters on one session opt in.

- **Shared subscriptions never set No-Local.** A `$share/…` subscription is
  issued **without** the No-Local flag even when `no_local: true`, because MQTT 5
  §3.8.3.1 makes No-Local on a shared subscription a Protocol Error the broker
  answers with a DISCONNECT. The subscribe path suppresses it for the shared case
  so enabling `no_local` on a session that also uses `$share` cannot wedge the
  connection.

- **Cross-bridge provenance headers are reserved, not yet enforced.**
  `x-bridge.forwarded-from` and `x-bridge.forwarded-hop` are declared reserved
  headers classified **bridge-to-bridge propagated** (`IsBridgeToBridgeHeader`,
  `domain/messaging/headers.go`). Reserving them means an application payload
  cannot forge them — an inbound copy from untrusted wire input is stripped at
  ingress unless a bridge lifts it through a typed field (the ADR 0001 / 0008
  mechanism). They are reserved **for** a hop-count / provenance loop-cut but are
  **not stamped, incremented, or hop-limited today**. Cross-bridge cycles are
  currently broken by design at the operator layer: distinct bridges use distinct
  `client_id`s (No-Local is per-connection, so it does not span bridges), the
  idempotency/dedup layer collapses a message that loops back with a stable key,
  and operators must not wire a broker cycle.

- **The contract is documented for operators.** The `no_local` key, its default,
  and the `$share` exception are on the MQTT transport page
  ([transports/mqtt.md](../transports/mqtt.md#session-options-reference-optionssession)).

## Consequences

- A single-session bridge with overlapping subscribe/publish filters can stop its
  own self-amplifying loop with one config flag, without restructuring topics.
- Existing single-session round-trip deployments are unaffected: the default-off
  choice means enabling the fix is a deliberate act, never a silent behavior
  change on upgrade.
- Enabling `no_local` alongside `$share` is safe — the shared subscriptions
  transparently drop the flag rather than tripping a broker DISCONNECT.
- Cross-bridge loops remain an operator responsibility. The bridge does not yet
  refuse to relay a message it previously forwarded, so a misconfigured broker
  cycle can still circulate traffic; the dedup layer bounds the *duplication* but
  not the *circulation*. Reserving the provenance headers keeps the namespace
  clear for the enforcement step without shipping a half-wired hop counter now.
- The reserved-but-unstamped `forwarded-hop` header is a known, documented gap,
  not an accidental omission: closing it is a follow-on that lifts and increments
  the header, then drops a message whose hop count exceeds a bound.

## Rejected alternatives

- **No-Local on by default (or unconditional).** Rejected — it silently breaks
  legitimate same-session round-trip topologies that rely on receiving their own
  publishes, and it changes behavior on a version bump with no operator action.
  Opt-in keeps the MQTT-standard default and makes the loop-cut explicit.
- **No-Local on shared subscriptions.** Not an option — the broker rejects it as
  an MQTT 5 Protocol Error; issuing it would wedge the whole connection. Hence the
  unconditional `$share` suppression.
- **Config-lint that rejects any subscribe/publish filter overlap at build.**
  Rejected as the primary mechanism — filter overlap is legitimate in many
  fan-in/fan-out topologies, so a hard build failure would false-positive on
  correct configurations. No-Local cuts the *delivery* loop without forbidding
  the *overlap*.
- **Enforced max-hop-count drop via `forwarded-hop` now.** Deferred, not
  rejected — it requires lifting the header past the ingress strip (ADR 0008
  mechanism), stamping/incrementing it on egress, and choosing a bound. Shipping
  the reserved namespace without the half-wired counter avoids a header that
  looks enforced but is not.
