---
applyTo: "adapters/azure/**"
---

# Azure Service Bus transport

Adds to `adapters.instructions.md`. Sources: ADR-0002 and
`docs/transports/servicebus.md`.

- A delayed `Retry` schedules a copy that carries `x-bridge.retry-attempt` and
  `x-bridge.original-message-id`, with its `MessageID` salted by the attempt
  number. Both properties are stripped at ingress. Without them
  `MaxReplayAttempts` never fires and broker dedup discards the retry.
- If `CompleteMessage` fails ambiguously after scheduling, the scheduled copy
  is not cancelled. A duplicate is preferred over a loss.
- On a topic subscription, a delayed `Retry` falls back to an immediate
  `Abandon`: a scheduled message would fan out to sibling subscriptions.
- An empty `MessageID` falls back to `asb-seq:<scope>:<sequence>` with scope
  `q:`, `s:<topic>:<sub>` or `t:`. Never a random ID per delivery — the
  mapping must stay the same across redeliveries and distinct across entities.
- Rotation is build-first and generation-fenced: `commitRebuild(gen, …)`
  installs the new stack only if `r.rebuildGen == gen`. A failed build leaves
  `cfg.Connection` untouched and `rebuildPending` set.
- Pinned-session (`session_id`) rotation closes the old link before building.
  `currentClient()` returns nil in that gap; callers must handle nil, not panic.
- A rotated client secret clears `use_managed_identity`. A username with an
  empty secret is rejected with `ErrInvalidPayload` and the connection is left
  as it was.
- `ReceiveAndDelete` is at-most-once. It stays opt-in and validated.
