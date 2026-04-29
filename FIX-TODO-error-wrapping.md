# FIX-TODO — Boundary error-wrapping sweep (gates `wrapcheck`)

> Carve-out from the architectural TODOs that survived the
> April 2026 sprint. Companion files: `FIX-004.md` (domain split),
> `FIX-006.md` (adapter ACL refactor), `FIX-TODO-clock-injection.md`,
> `FIX-TODO-return-types.md`, `FIX-TODO-test-quality.md`.

## Why this exists

The `wrapcheck` golangci-lint rule enforces that every error
crossing a package boundary is wrapped with context. Today the
codebase mixes wrapped and unwrapped errors at adapter boundaries,
which makes debugging production incidents harder than it needs to
be — a stack-trace-equivalent context chain (`fmt.Errorf("%w", err)`
or `domain.ErrXxx.Wrap(err)`) is the difference between a useful
error and a mystery one.

Enabling `wrapcheck` without a sweep would fail every adapter
package; many adapters return SDK errors directly with no wrapper.

## Current state (snapshot at FIX-009)

- `domain.BridgeError` has `.Wrap(err)` for wrapping infrastructure
  errors with classification (`ErrConnectionLost.Wrap(err)` etc.).
- `fmt.Errorf("%w", err)` is widely used for wrapping at internal
  boundaries (e.g. `fmt.Errorf("config: yaml parse: %w", err)`).
- Some adapters return raw SDK errors at port boundaries — those
  are the violations `wrapcheck` would catch.
- `.golangci.yml` has `wrapcheck` listed in the `enable:` block but
  commented out with `# TODO(FIX-TODO-error-wrapping.md): enable
  once boundary-error wrapping is swept across all adapters.`

## Approach

### Phase 1 — Inventory the violation surface

Run `wrapcheck` once with custom config to count current violations
per package:

```bash
# Add wrapcheck to .golangci.yml temporarily, then:
make lint-go 2>&1 | grep wrapcheck | sed 's/:.*//' | sort | uniq -c | sort -rn
```

Expected hot spots:
- `adapters/*/transport/*` — message-send and receive paths returning
  SDK errors after `Publish` / `Receive`.
- `adapters/*/store/*` — DDB / SQLite errors returning unwrapped
  from queries.
- `adapters/*/credentials/*` — secret-fetch errors.
- `runtime/*` — internal boundaries between sub-systems.

### Phase 2 — Decide per-call wrap vs classify

For each violation, choose:

- **Classify with a domain error** when the failure has a meaningful
  semantic class downstream (transient, permanent, expired,
  rejected). Use `domain.ErrXxx.Wrap(err)`. Preferred for adapter
  outbound calls.
- **Wrap with context** when the error is internal (no classification
  needed) but the caller benefits from knowing where it happened.
  Use `fmt.Errorf("subsystem: operation: %w", err)`.
- **Pass through** is correct only when the function is a thin
  pass-through wrapper that adds no context (rare; `wrapcheck` has
  an option to allow specific patterns).

### Phase 3 — Sweep one adapter at a time

Recommended order:

1. AWS SQS transport (most error paths)
2. Azure Service Bus transport
3. AMQP 0.9.1 transport
4. AMQP 1.0 transport
5. MQTT Paho transport
6. AWS DynamoDB stores (lease/outbox/dlq)
7. Native SQLite stores
8. Credentials adapters
9. Metrics / tracing adapters
10. Runtime internal boundaries

Per package: every error return wrapped or classified, build + test
green, commit.

### Phase 4 — Configure `wrapcheck` in `.golangci.yml`

Tune the linter's allow list for known-OK pass-throughs:

```yaml
settings:
  wrapcheck:
    ignoreSigs:
      - .Wrap(           # domain.BridgeError.Wrap
      - errors.New(
      - errors.Wrap(
      - fmt.Errorf(
    ignoreSigRegexps:
      - "^errors\\..*"
    ignorePackageGlobs:
      - "github.com/mariotoffia/gobridge/domain/*"
```

The exact allow list emerges as you sweep — start permissive,
tighten as patterns settle.

### Phase 5 — Enable `wrapcheck`

```yaml
linters:
  enable:
    - wrapcheck   # was commented out
```

`make lint` passes; a trial unwrapped error in any adapter fails
the gate.

## Cost estimate

- ≈ 80–120 violation sites across adapters and runtime.
- Per-adapter sweep: 1–3 hours each.
- Total: **2–4 dedicated days**.

## Risks

- **Over-wrapping.** Wrapping an error that's already wrapped (e.g.
  caller does `fmt.Errorf("y: %w", x)` where `x = fmt.Errorf("x: %w", inner)`)
  produces verbose chains. Stop wrapping once the context is
  actionable; wrapcheck's allow list catches most legitimate
  pass-throughs.
- **Domain-error classification creep.** Resist the urge to invent
  new `domain.ErrXxx` codes for every adapter quirk. Prefer
  `fmt.Errorf` with context unless the caller actually needs to
  switch on class.
- **Test changes.** Tests asserting on raw error values will need
  `errors.Is` updates. Plan for test churn proportional to source
  churn.

## Acceptance

- `wrapcheck` is enabled in `.golangci.yml`.
- `make lint` passes with the configured allow list.
- A trial unwrapped error return at an adapter boundary fails the
  gate with a clear message.
- Production incidents have actionable error chains: every error
  reaching the runtime carries source-package context.

## Related

- Original plan: `FINAL_DDD_HEX_CLEAN_FIX_PLAN.md` § FIX-005.
- Sibling carve-outs: `FIX-TODO-clock-injection.md`,
  `FIX-TODO-return-types.md`, `FIX-TODO-test-quality.md`.
- Domain error machinery: `domain/errors.go` (`BridgeError`,
  `ErrorClass`, `ErrorCode`).
