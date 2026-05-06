# Architecture Plan: Envelope Subject vs Transport Address

## Status

Planned. This is an architecture bug, not a single adapter test expectation bug.

## Constraint

No backward compatibility. Do not add compatibility shims, deprecated methods, or fallbacks from `Envelope.Subject` to transport address. Once this change is implemented, callers must provide a destination address through the new outbound send path when the transport requires one.

## Bug

`Envelope.Subject` currently has two incompatible meanings:

- Logical event subject, used by filters, resolvers, circuit breakers, HTTP/SSE payloads, AMQP 1.0 `Properties.Subject`, Azure Service Bus `Subject`, SQS `Subject` message attributes, and application-level routing rules.
- Transport address, used by runtime dispatch to tell MQTT which topic to publish to and AMQP 0-9-1 which routing key to use.

The immediate failure mode is visible in the AMQP 0-9-1 to MQTT cross-transport path:

1. AMQP publisher creates an envelope with `Subject = "cross-transport-test"`.
2. Runtime resolves `DispatchPlan.Address = "gap-cross/mqtt"`.
3. Runtime overwrites `Envelope.Subject` with the dispatch address before send.
4. MQTT receiver reports `Subject = "gap-cross/mqtt"` instead of the logical subject.

The deeper issue is that `DispatchPlan.Address` already exists, but `ports.Sender.Send(ctx, env)` has no address parameter. Runtime therefore smuggles the address through `Envelope.Subject`.

## Decision

`Envelope.Subject` means logical event subject only.

`DispatchPlan.Address` means the concrete transport destination for this dispatch. Examples:

- MQTT publish topic.
- AMQP 0-9-1 routing key.
- AMQP 1.0 link target address, when supported by the sender.
- SQS queue URL/name, when the sender supports dynamic addressing.
- Azure Service Bus queue/topic entity, when the sender supports dynamic addressing.

Runtime must never mutate `Envelope.Subject` to apply an outbound address.

## New Abstraction

Introduce an explicit outbound message type in `ports`:

```go
type OutboundMessage struct {
    Envelope *domain.Envelope
    Address  string
}
```

Change sender interfaces to take outbound messages:

```go
type Sender interface {
    Send(ctx context.Context, msg OutboundMessage) error
}

type BatchSender interface {
    Sender
    SendBatch(ctx context.Context, msgs []OutboundMessage) (int, error)
}
```

Runtime dispatch must pass `DispatchPlan.Address` as `OutboundMessage.Address` and pass a cloned or otherwise isolated envelope with merged dispatch headers. It must not mutate the original delivery envelope subject.

## Subject Propagation

Each transport must map logical subject independently from transport address.

Use transport-native subject support where it exists:

- AMQP 1.0: `Properties.Subject`.
- Azure Service Bus: `Message.Subject`.
- SQS: `Subject` message attribute.
- HTTP/SSE: JSON `subject`.

For transports without native subject support, use an explicit non-reserved carrier:

- MQTT user property: `gobridge.subject`.
- AMQP 0-9-1 header table: `gobridge.subject`.

Do not use `x-bridge.*` for the subject carrier. That prefix is reserved for bridge-internal headers and is stripped at ingress to prevent injection.

Transport addresses must be captured as transport-specific headers on ingress:

- MQTT topic: `mqtt.topic`.
- AMQP 0-9-1 routing key: `amqp091.routing-key`.
- AMQP 1.0 receiver address fallback: do not set `Envelope.Subject`; keep address metadata in transport headers only if useful.
- SQS queue URL/name fallback: do not set `Envelope.Subject`; keep queue metadata in transport headers only if useful.
- Azure queue/topic fallback: do not set `Envelope.Subject`; keep entity metadata in transport headers only if useful.

## Runtime Behavior

Direct hold:

- Resolve destination to `DispatchPlan`.
- Merge `DispatchPlan.Headers` into an outbound envelope copy.
- Send `ports.OutboundMessage{Envelope: outboundEnvelope, Address: plan.Address}`.
- Do not modify the source delivery envelope subject.

Shared outbox:

- Persist `OutboxRecord.Address` from `DispatchPlan.Address`.
- Persist the original logical `Envelope.Subject`.
- During drain, merge dispatch headers into an outbound envelope copy.
- Send `ports.OutboundMessage{Envelope: outboundEnvelope, Address: rec.Address}`.
- Do not overwrite `rec.Envelope.Subject`.

Hooks and DLQ:

- Add outbound address to egress hook events so observability does not rely on `Envelope.Subject`.
- Add address to DLQ entries or DLQ routing metadata for failed egress.
- Keep `Envelope.Subject` in hooks and DLQ as the logical event subject.

## Adapter Impact

### MQTT

Blast radius: high.

- `PublishFromEnvelope` must take address from `OutboundMessage.Address` or `SenderOptions.DefaultTopic`.
- Remove publish topic fallback from `env.Subject`.
- Sender must fail when no address and no default topic are configured.
- On send, include `gobridge.subject` user property when `Envelope.Subject` is non-empty.
- On receive, set `Envelope.Subject` from `gobridge.subject` user property.
- On receive, always set `Headers["mqtt.topic"] = pub.Topic`.
- Do not expose `gobridge.subject` as a normal application header after mapping it to `Envelope.Subject`.

### AMQP 0-9-1

Blast radius: high.

- Sender routing key must come from configured `RoutingKey` or `OutboundMessage.Address`.
- Remove routing key fallback from `env.Subject`.
- Sender must fail when no routing key can be resolved and the exchange requires one.
- On send, include `gobridge.subject` in the AMQP header table when `Envelope.Subject` is non-empty.
- On receive, set `Envelope.Subject` from `gobridge.subject` header when present.
- On receive, keep broker routing metadata in existing `amqp091.routing-key` and related headers.
- Do not set `Envelope.Subject` from `Delivery.RoutingKey`.

### AMQP 1.0

Blast radius: medium.

- Keep `Envelope.Subject` mapped to `Message.Properties.Subject`.
- Remove receiver fallback from link address to `Envelope.Subject`.
- Sender links are currently address-bound. Initially validate `OutboundMessage.Address`:
  - Empty address means use configured sender link address.
  - Non-empty address must match configured address, or send fails as invalid destination.
- Dynamic per-address link creation can be a later task if needed.

### SQS

Blast radius: medium.

- Keep `Envelope.Subject` mapped to `Subject` message attribute.
- Remove receiver fallback from queue name or queue URL to `Envelope.Subject`.
- Existing sender queue URL/name remains configured sender state.
- If dynamic SQS addressing is added, it must use `OutboundMessage.Address`, not `Envelope.Subject`.
- FIFO deduplication must be reviewed because it currently hashes `env.Subject`; it should hash logical subject only, not destination address.

### Azure Service Bus

Blast radius: medium-low.

- Keep `Envelope.Subject` mapped to `Message.Subject`.
- Remove receiver fallback from queue/topic name to `Envelope.Subject`.
- Existing sender entity remains configured sender state.
- If dynamic Service Bus addressing is added, it must use `OutboundMessage.Address`, not `Envelope.Subject`.

### HTTP/SSE

Blast radius: low.

- HTTP ingress already treats JSON `subject` as logical subject.
- SSE already emits logical subject.
- Update sender interface signatures and tests only.

## Tasks

### 1. Define outbound send contract - DONE

- Add `ports.OutboundMessage`.
- Change `ports.Sender.Send` and `ports.BatchSender.SendBatch` signatures.
- Update fake senders and compiler errors across the repo.
- Acceptance: code compiles far enough to expose concrete runtime and adapter updates.

### 2. Add address to observability contracts - DONE

- Add `Address string` to `ports.DeliveryAttempt`.
- Add `Address string` to `ports.DeliveryOutcome`.
- Add `Address string` to `domain.DLQEntry` or equivalent DLQ routing metadata.
- Acceptance: egress hooks and DLQ records can report destination address without reading `Envelope.Subject`.

### 3. Fix direct-hold dispatch - DONE

- Stop assigning `env.Subject = plan.Address`.
- Build an outbound envelope copy for dispatch header merge.
- Pass `plan.Address` as `OutboundMessage.Address`.
- Populate hook `Address` fields.
- Acceptance: a unit test proves source `Envelope.Subject` remains unchanged after direct-hold send.

### 4. Fix shared-outbox dispatch and drain - DONE

- Persist `OutboxRecord.Address` without changing `OutboxRecord.Envelope.Subject`.
- During drain, build an outbound envelope copy.
- Pass `rec.Address` as `OutboundMessage.Address`.
- Populate hook and DLQ address fields.
- Acceptance: a unit test proves outbox drain sends to address while preserving logical subject.

### 5. Update MQTT adapter - DONE

- Change sender to use `OutboundMessage.Address` or default topic.
- Remove `env.Subject` topic fallback.
- Add `mqtt.topic` ingress header.
- Add `gobridge.subject` send and receive mapping.
- Update MQTT header conversion tests.
- Acceptance: MQTT round-trip preserves logical subject and records the publish topic separately.

### 6. Update AMQP 0-9-1 adapter - DONE

- Change sender to use configured `RoutingKey` or `OutboundMessage.Address`.
- Remove `env.Subject` routing key fallback.
- Add `gobridge.subject` send and receive mapping.
- Stop setting `Envelope.Subject` from `Delivery.RoutingKey`.
- Update AMQP 0-9-1 sender and receiver tests.
- Acceptance: routing key is preserved in headers, logical subject comes only from the explicit subject carrier.

### 7. Update AMQP 1.0 adapter - DONE

- Change sender signature.
- Keep logical subject mapped to `Properties.Subject`.
- Remove receiver fallback from configured address to `Envelope.Subject`.
- Validate non-empty `OutboundMessage.Address` against configured address.
- Update AMQP 1.0 conversion tests.
- Acceptance: messages without `Properties.Subject` produce empty `Envelope.Subject`.

### 8. Update SQS adapter - DONE

- Change sender and batch sender signatures.
- Keep logical subject mapped to `Subject` message attribute.
- Remove receiver fallback from queue name or queue URL to `Envelope.Subject`.
- Review FIFO deduplication input after subject semantics change.
- Update SQS sender, batch sender, receiver, and SNS unwrap tests.
- Acceptance: SQS messages without `Subject` attribute produce empty `Envelope.Subject`.

### 9. Update Azure Service Bus adapter - DONE

- Change sender and batch sender signatures.
- Keep logical subject mapped to `Message.Subject`.
- Remove receiver fallback from queue/topic entity to `Envelope.Subject`.
- Update Service Bus sender and receiver tests.
- Acceptance: Service Bus messages without native subject produce empty `Envelope.Subject`.

### 10. Update HTTP/SSE adapter - DONE

- Change sender signature.
- Keep HTTP ingress and SSE event subject unchanged as logical subject.
- Update tests and fakes.
- Acceptance: HTTP and SSE tests pass with the new sender contract.

**Status:** Resolved 2026-05-06. SSESender.Send validates OutboundMessage.Address against the configured logical identity (SetRouteID override > cfg.id): nil envelope short-circuits to shared.ErrInvalidPayload; non-empty Address that does not equal the identity returns shared.ErrInvalidTopic with both addresses in the diagnostic message before any marshal, fan-out, metric, or trace emission. Stale TODO(T03/T09) marker removed; dynamic per-message SSE channel routing documented as Non-Goal. HTTP POST ingress is intentionally untouched (still requires body.Subject) and Envelope.Subject flows through to the SSE payload's `subject` field unchanged. New subject_address_test.go covers nil envelope, mismatched Address (with diagnostic content + no metric emission), empty Address, matching Address, SetRouteID identity override, and HTTP→SSE round-trip subject preservation.

### 11. Update bridge builder and runtime wiring - DONE

- Update sender registries and route construction to use new sender signatures.
- Keep `DestinationBinding.Address` and `DispatchPlan.Address` as the single destination abstraction.
- Ensure configured binding address validation still happens before send.
- Acceptance: bridge builder tests pass without subject/address coupling.

### 12. Update cross-transport regression coverage

- Keep AMQP 0-9-1 to MQTT test expecting logical subject `cross-transport-test`.
- Add assertion that MQTT ingress header `mqtt.topic` equals `gap-cross/mqtt`.
- Add direct unit tests for MQTT to AMQP 1.0 and SQS to MQTT subject propagation if test infrastructure allows.
- Acceptance: cross-transport tests distinguish logical subject from transport address.

### 13. Update documentation

- Update `ARCHITECTURE.md` to define `Envelope.Subject` as logical event subject only.
- Update dynamic destination docs to say `DispatchPlan.Address`, not `Envelope.Subject`, drives outbound destination.
- Update transport configuration docs for MQTT topic and AMQP routing key behavior.
- Acceptance: docs no longer describe rendered addresses becoming `Envelope.Subject`.

### 14. Remove stale compatibility assumptions

- Search for tests or docs that assert resolved address appears in `Envelope.Subject`.
- Replace them with explicit address assertions or transport-specific headers.
- Remove comments that describe `env.Subject` as topic, routing key, queue URL, or destination.
- Acceptance: `rg "env.Subject|Envelope.Subject|subject"` finds no remaining destination-address semantics outside tests that intentionally check logical subject.

## Non-Goals

- Do not add a compatibility layer that still reads `env.Subject` as address.
- Do not introduce dynamic per-address senders for AMQP 1.0, SQS, or Azure Service Bus in the first pass.
- Do not rename `Envelope.Subject` in this change. Its semantic contract is enough.
- Do not use reserved `x-bridge.*` headers for cross-transport subject propagation.

## Review Checklist

- `Envelope.Subject` is never assigned from a dispatch address.
- Every transport that needs a destination reads it from `OutboundMessage.Address` or fixed sender configuration.
- Every transport maps logical subject independently from transport address.
- Ingress address metadata is available in headers for diagnostics and downstream routing when needed.
- Egress hooks and DLQ entries include address explicitly.
- Cross-transport tests assert both logical subject and transport address separately.
