---
applyTo: "adapters/**"
---

# Adapters: rules every transport, store and credential adapter shares

These hold for every package under `adapters/`. The file for the specific
technology (MQTT, SQS, Service Bus, AMQP, HTTP, stores) adds to this one.
`make lint` already checks layering, the ACL boundary (`acl_*.go`), typed plugin
config and registry symmetry, so those need no review comment.

## Conventions lint does not check

- An adapter file that implements a port ends with a compile-time assertion,
  so a signature drift fails the build instead of the plugin lookup at runtime:

  ```go
  var _ ports.Receiver = (*Receiver)(nil)
  var _ ports.Sender = (*Sender)(nil)
  var _ ports.TransportFactory = (*Factory)(nil)
  ```

- Constructors take functional options named `WithXxx(value)`. A new
  positional parameter or an options struct passed to `New` breaks the shape
  every other adapter uses.
- A reference to another domain context goes through `domain/shared`.

## Subject and Address

- A sender publishes to `OutboundMessage.Address`, then to its configured
  default destination. It never reads the destination from `Envelope.Subject`.
  `Subject` is the logical name; `Address` is where the transport sends. See
  `docs/internals/architecture-message-flow.md` §Subject vs. Address.

## Reserved headers and identity

- Inbound `x-bridge.*` headers are untrusted. The runtime strips them at ingress
  (ADR-0001). A receiver that needs to carry a trusted value sets a typed
  `EnvelopeInput` field; it does not add a new `x-bridge.*` header.
- Bridge-to-bridge identity (idempotency key, dedup ID, ordering key) is lifted
  into `EnvelopeInput` fields, following each transport's rule in ADR-0008.

## Settlement

- `Ack` and `Retry` on one `Delivery` are mutually exclusive, and a second call
  is safe. A settlement the protocol cannot perform returns
  `shared.ErrNotSupported`; it never silently acks.
- A failed settlement is returned, never swallowed. A swallowed failure loses
  or duplicates the message with no signal.

## Credentials

- Rotation picks one shape on purpose (ADR-0002, `docs/credentials-rotation.md`):
  - swap-capable clients (SQS, Service Bus) build the new client first and
    swap only on success, so a failed build leaves the old client live;
  - connection-bound transports (MQTT, AMQP 1.0, AMQP 0-9-1) commit the new
    credentials, then force a reconnect.
- `ApplyCredentials(ctx, *connectivity.CredentialSet)` gets the full set and
  acts on what changed. Reactive re-resolve on `NOT_AUTHORIZED` is the optional
  `SetAuthFailureCallback(func(error))` method, found by type assertion. The
  adapter does not import `bridge`, and does not emit the resolver or refresher
  metrics itself (`docs/internals/plugin-credential-and-observability-adapters.md`).
- Credential values never reach a log, an error string or a metric label.
  Credential material comes from `credentials_uri`, never from inline YAML.

## Sessions and lifecycle

- A transport that declares `CapExclusiveIdentity` is single-use: `Start`
  after `Close` returns `shared.ErrUnavailable` and never reconnects. The
  runtime relies on this to hand the lease to a standby; a quiet reconnect
  breaks lease fencing (`docs/internals/plugin-transport-adapters.md`).
- A receiver with `Close(ctx)` accepts `Run` after `Close`. The route runner
  closes it when each run ends and restarts a failed route by running the same
  instance again. `Close` releases what that run held, settling or handing back
  its deliveries, and never latches the receiver shut: a latch turns one broker
  fault into a route that stays down until the process restarts (`ports.Receiver`,
  `transporttest` `RunAfterClose`). This is not the exclusive-session rule
  above: that one is about a `Session`, this one about a `Receiver`.
- A factory that declares `CapDedicatedIngressSession` keeps its receiver
  reservation on the `Session`, not on the `Factory`, so aliases cannot bypass
  it.
- Every network call and every teardown path is bounded by a context deadline
  or a configured timeout. An unbounded close under a mutex wedges shutdown.

## Tests

- Docker-backed tests use the matching `testutil/*local` helper, which skips
  under `-short` or without Docker. They assert through the port
  (`ports.Receiver`, `ports.Delivery`, `ports.Sender`), not through vendor SDK
  types (TESTS.md §5.4).
- A test in one adapter never imports a sibling adapter (TESTS.md §3.1).
