---
applyTo: "processors/**,circuitbreaker/**"
---

# Processors and the circuit breaker

Sources: ADR-0001, `docs/internals/architecture-message-flow.md`,
`docs/internals/architecture-contracts-and-clustering.md` and
`docs/internals/ddd-aggregates.md`.

- A processor is an onion layer: `Process(ctx, env, next)`. The built-in order
  is filter, transform, circuit breaker, tenant, then dispatch.
- Processors run after the ingress strip and are trusted, so they may set a
  route override. An in-band `ActionRoute` that falls through on an unknown
  binding is intentional.
- A processor never rewrites `Subject` to steer the destination. The address
  travels on `ports.OutboundMessage.Address`.
- A failure returns a classified `BridgeError`. Rejected-class codes
  (`INVALID_PAYLOAD`, `PAYLOAD_TOO_LARGE`, `SCHEMA_VIOLATION`,
  `MESSAGE_FILTERED`, …) are never retried; `MESSAGE_FILTERED` follows
  `on_filtered`. An unclassified error lands in the wrong class and is retried
  or dead-lettered by mistake.
