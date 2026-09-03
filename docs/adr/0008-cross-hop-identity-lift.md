# 0008 — Cross-hop bridge-to-bridge identity lift

Status: accepted
Date: 2026-07-04
Deciders: GoBridge core

## Context

Three bridge-to-bridge propagated reserved headers carry the identity a
downstream bridge needs to deduplicate, preserve ordering, and correlate a
message across a `bridge → broker → bridge` relay: `x-bridge.idempotency-key`,
`x-bridge.dedup-id`, `x-bridge.ordering-key`. Lose them at a hop and dedup and
ordering suppression break downstream of the relay.

[ADR 0001](0001-reserved-header-trust-model.md) strips every `x-bridge.*` key
from untrusted transport input at ingress (`StripReservedHeaders`,
`domain/messaging/headers.go`). A message arriving on a transport is external
input, so the strip is unconditional and correct — but it erases the three
identity keys along with everything else, so a naive relay drops the identity at
each receiving hop.

`NewEnvelope` funnels every constructed envelope through that strip:
`NewHeadersFromMap` applies `StripReservedHeaders` semantics to the
caller-supplied header map (`domain/messaging/envelope.go`,
`domain/messaging/headers.go`). Any value that must survive ingress has to
enter by another door.

## Decision

Lift the three identity keys from the ingress wire into the typed
`EnvelopeInput` fields `IdempotencyKey` / `DeduplicationID` / `OrderingKey`
(`domain/messaging/envelope.go`). `NewEnvelope` re-stamps them into their
reserved headers with `SetHeader` **after** the strip
(`domain/messaging/envelope.go`) — the same typed-field, post-strip
mechanism ADR 0001 uses for the route override.

The difference from ADR 0001: there the value originates in-process (an
internally-constructed binding override). Here the value originates from
untrusted wire data — an SQS message attribute, an AMQP application property, or
an HTTP header — any of which a principal with broker/queue write can set. The
lift reads that wire value into a typed field by design, then lets `NewEnvelope`
re-stamp it after the strip has run.

Per-transport sources (the lift is the sole path; the wire header is still
stripped):

- **SQS** — `convertMessage` (`adapters/aws/transport/sqs/acl_inbound.go`):
  idempotency key from the `x-bridge.idempotency-key` message attribute (`:216`,
  `bridgeAttrString` `:239`); dedup ID and ordering key from the native FIFO
  coordinates `MessageDeduplicationId` / `MessageGroupId` (`:217-218`).
- **AMQP 1.0** — `messageToEnvelope`
  (`adapters/amqp/transport/amqp10/acl_inbound.go`): all three from
  application properties (`:235-237`, `bridgeHeaderString` `:249`).
- **HTTP** — `receiver.go`: from the `Idempotency-Key` / `X-Dedup-Id` /
  `X-Ordering-Key` request headers
  (`adapters/http/transport/receiver.go`), lifted into `EnvelopeInput`
  (`:438-440`).

The lift is **unconditional** — none of the three converters consult
`trust_bridge_headers`. That flag is a route-policy field
(`domain/routing/policy.go`) evaluated later in the route runner
(`runtime/route/runner.go`); it governs preservation of the broader
bridge-to-bridge header set (correlation-id, causation-id, tenant-id,
forwarded-*, …). Identity must survive even when that broader preservation is
off, so the three keys are lifted regardless.

## Consequences

- Cross-hop idempotency, deduplication, and ordering work across a
  `bridge → broker → bridge` relay. On FIFO SQS the dedup ID and ordering key
  ride the native `MessageDeduplicationId` / `MessageGroupId` fields and never
  consume a message-attribute slot.
- **Availability-via-collision surface.** The lifted idempotency key propagates
  to downstream idempotency-keyed dedup points — e.g. the HTTP ingress LRU
  window (`adapters/http/transport/dedup.go`). A spoofed or predicted
  idempotency key can suppress a *different, legitimate* message there. This is
  an availability concern, not forgery: broker/queue write access already
  implies message injection. Accepted and disclosed in
  `docs/transports/sqs.md`; mitigated by IAM scoping of broker/queue write and
  by unguessable keys.
- **Distinct from ADR 0001's route-override case.** The route override is
  internally-constructed only and never sourced from the wire; a wire
  `x-bridge.route-override` stays discarded. Identity lift deliberately sources
  typed fields from the wire. The two coexist because routing control and
  identity propagation have different trust needs: steering must never honor
  external input, identity must survive it.

## Rejected alternatives

- **Gate the lift behind `trust_bridge_headers`.** Drops cross-hop dedup and
  ordering in the common case where ingress is untrusted and the flag is off.
  Identity must survive regardless of who fed the receiver, so gating it defeats
  the purpose.
- **Sign or verify the identity keys.** Adds key management and per-message
  crypto to defend an availability property, not an integrity one. The exposure
  is duplicate-suppression collision, already bounded by broker/queue write
  access; IAM scoping plus unguessable keys is cheaper and covers the same
  surface.
- **Lift the full bridge-to-bridge header set unconditionally.** Preserving
  correlation-id, causation-id, tenant-id, and forwarded-* from untrusted input
  would let an external producer spoof tenant identity. That set stays behind
  `trust_bridge_headers` — the trusted-peer opt-in with its own audit — and is
  out of scope here. Only the three identity keys, whose worst case is duplicate
  suppression, are lifted unconditionally.
