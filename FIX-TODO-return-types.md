# FIX-TODO — Adapter return-type cleanup (gates `ireturn`) - DONE

**Status:** DONE 2026-05-05 — `ireturn` is enabled in `.golangci.yml`
with a curated allow-list, and `make lint` is green across the
repository.

The allow-list is grouped by rationale (six categories documented
inline in `.golangci.yml`):

1. ireturn standard tokens (`error`, `empty`, `anon`, `stdlib`,
   `generic`).
2. Transport / store seams returned by `ports.TransportFactory` and
   `ports.StoreFactory` implementations
   (`Receiver`, `Sender`, `Session`, `LeaseStore`, `OutboxStore`,
   `DLQStore`).
3. Runtime accessors (`RouteLocator`).
4. Domain / port polymorphic seams: `domain.DrainStrategy`,
   `domain/clock.{Clock,Timer,Ticker}`, `ports.DestinationResolver`,
   `ports.CredentialRepository`, `ports.Loader`, `ports.Watcher`,
   `ports.Span`, `ports.Tracer`.
5. Adapter-internal mock seams (lower-cased package-private
   interfaces declared solely so unit tests can substitute the real
   client): `amqp091.amqpConnection`, `amqp10.amqpConn`,
   `sqs.sqsAPI`, `bootstrap.parameterResolver`.
6. Third-party SDK interfaces we cannot avoid returning: OpenTelemetry
   metric types (`Int64Counter`, `Float64Gauge`, `Float64Histogram`,
   `metricdata.Aggregation`) and AWS CDK construct types (`Stack`,
   `IFileSystem`, `AccessPoint`, `SecurityGroup`, `FargateService`,
   `FargateTaskDefinition`).

`_test.go` files are excluded from `ireturn` via a path rule —
test-local fakes that satisfy `ports.*` interfaces are inherently
interface-returning by construction; `ireturn` is a production rule.

The constructor sweep originally described in this plan was already
complete: every adapter `NewX` constructor returns a concrete struct,
and only the polymorphic factory seam returns port-typed values. No
source rewrites were required.

> Carve-out from the architectural TODOs that survived the
> April 2026 sprint. Companion files: `FIX-004.md`, `FIX-006.md`,
> `FIX-TODO-clock-injection.md`, `FIX-TODO-error-wrapping.md`,
> `FIX-TODO-test-quality.md`.

## Why this exists

The `ireturn` golangci-lint rule enforces "Accept Interfaces, Return
Concrete Types" — exported functions should return their concrete
type so callers get the full API surface (testing helpers, version
fields, internal accessors). Returning an unnamed interface from an
exported function hides intent and forces type assertions on the
caller side.

Today some adapter constructors return `ports.X` interfaces directly
(e.g. `func NewSender(...) ports.Sender`). The architectural
intent was sometimes to "wire it via the port" — but the right
pattern is to return the concrete type and let the caller assign it
to a `ports.X` variable. That preserves the concrete-type API for
tests and admin / debugging access.

Enabling `ireturn` without a sweep would fail many adapter
constructors that currently return ports interfaces.

## Current state (snapshot at FIX-009)

- Many adapter packages return `ports.X` from `NewX` constructors
  (a deliberate, but per-the-rule incorrect, choice).
- The compile-time assertion pattern `var _ ports.Sender = (*Sender)(nil)`
  is the right way to verify the concrete type satisfies the port
  WITHOUT hiding the concrete type from callers.
- `.golangci.yml` has `ireturn` listed in the `enable:` block but
  commented out with `# TODO(FIX-TODO-return-types.md): enable
  once exported functions stop returning unnamed interfaces in
  adapter code.`

## Approach

### Phase 1 — Inventory

Run `ireturn` once with custom config:

```bash
# Add ireturn to .golangci.yml temporarily, then:
make lint-go 2>&1 | grep ireturn | head -50
```

Expected violation surface:
- Every `func NewX(...) ports.Y` in adapters (~20–30 sites).
- Possibly some in `processors/`, `httpapi/`, `runtime/`.

### Phase 2 — Per-package rewrite

For each violation:

```go
// BEFORE:
func NewSender(session *Session, opts SenderOptions) ports.Sender {
    return &Sender{session: session, opts: opts}
}

// AFTER:
func NewSender(session *Session, opts SenderOptions) *Sender {
    return &Sender{session: session, opts: opts}
}

// And keep the assertion (it already exists):
var _ ports.Sender = (*Sender)(nil)
```

Callers that previously did:

```go
var snd ports.Sender = paho.NewSender(sess, opts)
```

become:

```go
snd := paho.NewSender(sess, opts) // *paho.Sender, satisfies ports.Sender via assignment context
// OR explicitly:
var snd ports.Sender = paho.NewSender(sess, opts)
```

Both work; the call site doesn't change semantically.

### Phase 3 — Configure `ireturn` for legitimate exceptions

Some interface returns are correct:

- `error` is always allowed (built-in exception).
- Returning a `ports.X` from a *factory method* on a TransportFactory
  IS the right pattern — the factory's contract is "produce a port
  implementation." Allow these via `ireturn.allow`.

Example config:

```yaml
settings:
  ireturn:
    allow:
      - error
      - empty
      - anon
      - stdlib
      # Factory methods return their port contract by design.
      - github.com/mariotoffia/gobridge/ports.Receiver
      - github.com/mariotoffia/gobridge/ports.Sender
      - github.com/mariotoffia/gobridge/ports.Session
      - github.com/mariotoffia/gobridge/ports.LeaseStore
      - github.com/mariotoffia/gobridge/ports.OutboxStore
      - github.com/mariotoffia/gobridge/ports.DLQStore
```

The factory pattern (`func (f *Factory) NewReceiver(...) (ports.Receiver, error)`)
is exempt because it's the polymorphic seam.

### Phase 4 — Sweep one adapter at a time

Recommended order: smallest adapter first, transports last.

1. Native stores (memory, sqlite)
2. Credentials adapters
3. Metrics / tracing adapters
4. Config-source adapters
5. AMQP / SQS / Service Bus / MQTT transports

Per package: rewrite constructors, build + test green, commit.

### Phase 5 — Enable `ireturn`

```yaml
linters:
  enable:
    - ireturn   # was commented out
```

`make lint` passes; a trial constructor returning `ports.X` (outside
the factory pattern) fails the gate.

## Cost estimate

- ≈ 20–30 constructor rewrites + caller updates.
- Per adapter: 30-60 min.
- Total: **1–2 dedicated days**.

## Risks

- **Caller assignment ambiguity.** When a caller writes
  `snd := paho.NewSender(...)` they get `*paho.Sender`, not
  `ports.Sender`. Method calls that exist on `*paho.Sender` but
  not on `ports.Sender` (e.g. testing-only methods) become
  callable. Usually fine; sometimes hides an unintended dependency.
- **Test fakes.** Mocks that satisfied `ports.X` may not satisfy
  the concrete type's method set. Audit test fakes that consumed
  the constructor return value.
- **Reflection-based code.** Anything using `reflect.TypeOf(snd)`
  on a constructor return now sees the concrete type, not the
  interface. Rare but watch for it.

## Acceptance

- `ireturn` is enabled in `.golangci.yml` with a documented allow
  list.
- `make lint` passes.
- A trial constructor returning a port interface (outside the
  documented allow list) fails the gate.
- Adapter callers can still assign the constructor return to a
  `ports.X` variable — the architectural seam is preserved.

## Related

- Original plan: `FINAL_DDD_HEX_CLEAN_FIX_PLAN.md` § FIX-005.
- Sibling carve-outs: `FIX-TODO-clock-injection.md`,
  `FIX-TODO-error-wrapping.md`, `FIX-TODO-test-quality.md`.
- The compile-time assertion pattern (`var _ ports.X = (*Y)(nil)`)
  is the architectural correctness check; this sweep removes the
  unnecessary interface-return that came alongside it.
