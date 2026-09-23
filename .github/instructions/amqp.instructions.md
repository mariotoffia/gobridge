---
applyTo: "adapters/amqp/**"
---

# AMQP transports (0-9-1 and 1.0)

Adds to `adapters.instructions.md`. Sources: ADR-0002, ADR-0008,
`docs/transports/amqp091.md` and `docs/transports/amqp10.md`.

## AMQP 0-9-1 (`adapters/amqp/transport/amqp091/`)

- Settlement: `Ack` is `Ack(false)`, `Retry` is `Nack(false, true)` (the delay
  is not enforced), `Extend` returns `ErrNotSupported`.
- A message without `message_id` gets a minted ID plus `x-bridge.generated-id`,
  and the runtime then never requests a requeue. Otherwise a deterministic
  failure requeues forever.
- A per-dispatch `OutboundMessage.Address` beats `routing_key`; if both are
  empty the send is rejected. `mandatory: false` needs the explicit
  `allow_unroutable_drop` opt-in.
- A publish timeout abandons the wedged channel to the reaper; it never closes
  the channel synchronously under the sender mutex. Abandoned channels are
  capped (then fail fast with `BROKER_BUSY`).
- At a `SendBatch` deadline the unconfirmed prefix is reported as transient,
  and a confirm that already arrived is always honoured.
- Rotation is close-then-redial. A sender never publishes on the old
  connection, and every teardown path is bounded.
- Publisher-side exchange auto-declare is best-effort and never takes a route
  down.
- Inbound TTL becomes an absolute deadline. An unmappable or "never" TTL means
  no expiry, never a deadline in the past.
- `CapExclusiveIdentity` is latched on first exclusive use, so the single-use
  rule applies from then on.

## AMQP 1.0 (`adapters/amqp/transport/amqp10/`)

- Settlement: `Ack` is `AcceptMessage`, `Retry(0)` is `ReleaseMessage`,
  `Retry(>0)` is `ModifyMessage(DeliveryFailed=true)` plus
  `x-opt-delivery-time`, counted on `AMQP10DelayedRetryDeferred`.
- Settlement is single-shot. A repeat call is a nil no-op only after a
  successful first call; while the first is in flight or after it failed, the
  repeat returns `ErrUnavailable`. A failed settlement is never swallowed.
- Identity lift in `messageToEnvelope`: all three keys come from application
  properties, unconditionally (ADR-0008).
- A message with a header section is countable via `delivery-count`. One with
  neither `message-id` nor a header section is uncountable and takes the
  generated-id path.
- Recovery stays narrow: connection, then receiver link, then sender link
  (rebuilt lazily on the next `Send`). A link error does not tear down the
  connection.
- A send timeout means the outcome is unknown: return an error so the runtime
  retries. Downstream must be idempotent.
- Rotation is commit-then-reconnect: swap `liveCreds` / `opts`, then close the
  connection. The single-use rule applies when run as an exclusive session.
