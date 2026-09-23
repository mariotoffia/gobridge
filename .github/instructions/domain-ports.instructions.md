---
applyTo: "domain/**,ports/**"
---

# Domain and ports

Sources: ADR-0001, ADR-0008, ADR-0010, DDD.md, UBIQUITOUS.md,
`docs/internals/ddd-aggregates.md` and
`docs/internals/plugin-transport-adapters.md`.

- `IsReservedHeader` is case-insensitive, and `MergeHeaders(...,
  protectReserved=true)` refuses to override a reserved header. A new identity
  or provenance value is a typed `EnvelopeInput` field, re-stamped in
  `NewEnvelope` after the strip — not a new trusted header.
- `x-bridge.forwarded-from` / `x-bridge.forwarded-hop` are reserved but not
  enforced. Code and comments must not claim hop-limit behaviour.
- `Envelope.Subject` is the logical event subject. Nothing in the runtime
  writes a destination into it; a sender reads its destination from
  `ports.OutboundMessage.Address`.
- Expiry checks take a `Clock`. `Clock` has no `Sleep`.
- A new error sentinel in `domain/shared` gets a row in the class and code
  tables (`TestSentinelClasses` and its neighbours in
  `domain/shared/errors_test.go`), so its class is pinned.
- A port contract change updates the matching conformance suite
  (`ports/storetest`, `ports/configstoretest`, `ports/transporttest`) in the same
  PR, so every backend gets the check (TESTS.md §3.3).
- Ports stay small. A new ability is an optional capability interface found by
  type assertion (the `OutboxReleaser` pattern), not a new method on the core
  port that every adapter must then implement.
- A new or changed term is a glossary row in `UBIQUITOUS.md`. The glossary is
  append-only: a change of meaning is a new `(correction)` row, and an existing
  row is never rewritten or merged.
