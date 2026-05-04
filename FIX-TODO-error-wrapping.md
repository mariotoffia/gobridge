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

### Phase 1 — Inventory the violation surface - DONE

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

#### Phase 1 — Findings (2026-05-04)

**Method:** Temporarily uncommented `- wrapcheck` in `.golangci.yml`
(line 41), kept all other settings unchanged. Ran the `Makefile`
`lint-go` per-module loop with golangci-lint v2.11.4. Filtered output
for `(wrapcheck)`, grouped by package directory. Reverted
`.golangci.yml` from a local backup; worktree clean.

**Total violations: 162** across 39 packages (152 production, 10 in
`_test.go`). Two categories reported by wrapcheck:

- `error returned from external package is unwrapped` — 82
- `error returned from interface method should be wrapped` — 80

##### Per-package counts (sorted desc)

| # | Package | Count |
|---|---|---|
| 1 | `runtime` | 36 |
| 2 | `deployment/aws-filebased-config/lib/bootstrap` | 17 |
| 3 | `adapters/azure/transport/servicebus` | 12 |
| 4 | `adapters/aws/store/dynamodblease` | 8 |
| 5 | `adapters/amqp/transport/amqp091` | 7 |
| 6 | `adapters/amqp/transport/amqp10` | 6 |
| 7 | `adapters/otel/metrics` | 5 |
| 8 | `adapters/aws/config/dynamodb` | 5 |
| 9 | `httpapi` | 4 |
| 10 | `adapters/aws/transport/sqs` | 4 |
| 11 | `processors/transform` | 3 |
| 12 | `config` | 3 |
| 13 | `adapters/otel/tracing` | 3 |
| 14 | `adapters/native/store/sqliteoutbox` | 3 |
| 15 | `adapters/native/config/file` | 3 |
| 16 | `adapters/http/transport` | 3 |
| 17–24 | `testutil/{sqslocal,s3local,rabbitmqlocal,mqttlocal,localstack,ddblocal,asblocal,artemislocal}` | 2 each (16) |
| 25 | `tests/integration` | 2 |
| 26 | `processors/filter` | 2 |
| 27 | `deployment/aws-filebased-config/lib/cmd/gobridge-filebased` | 2 |
| 28 | `bridge` | 2 |
| 29 | `adapters/native/store/sqlitedlq` | 2 |
| 30 | `adapters/native/store` | 2 |
| 31 | `adapters/mqtt/transport/paho` | 2 |
| 32 | `adapters/aws/store/dynamodboutbox` | 2 |
| 33–39 | `processors/circuitbreaker`, `observability`, `deployment/aws-filebased-config/lib/infra`, `adapters/native/credentials/file`, `adapters/native/cluster`, `adapters/aws/store/dynamodbdlq`, `adapters/aws/metrics/cloudwatch`, `adapters/aws/credentials/ssm` | 1 each (8) |

##### Hot-spot summary

- **`runtime` (36)** dominates — overwhelmingly forwards `ports.*Store` /
  `ports.CredentialRepository` errors verbatim.
- **`deployment/aws-filebased-config/lib/bootstrap` (17)** — pure
  composition-root forwarding of `bridge.Builder.Prepare`,
  `config.Manager.Load`, infra `Validate()`.
- **`azure/transport/servicebus` (12)** + **`aws/store/dynamodblease`
  (8)** — direct cloud SDK error returns.
- Top external-pkg sources: `ports.*` (46), `context.Context.Err` (20),
  `aws-sdk-go-v2/service/dynamodb` (11), `net.Listen` (9),
  `azservicebus` (6), `net.DialTimeout` (4),
  `aws-sdk-go-v2/config` (4), `bridge`/`config` own pkgs treated as
  external by wrapcheck (4 each).

##### Representative samples (one per category)

- **Interface-method (`ports`):** `runtime/dlq_router.go:235:9` —
  `ports.DLQStore.Write` returned bare.
- **Interface-method (context):**
  `adapters/amqp/transport/amqp091/receiver.go:93:11` — `ctx.Err()`
  returned bare.
- **Interface-method (vendor SDK):**
  `adapters/aws/config/dynamodb/loader.go:343:13` — `ddbAPI.GetItem`
  returned bare.
- **External-pkg (cloud SDK):**
  `adapters/aws/store/dynamodblease/store.go:130:31` —
  `dynamodb.Client.PutItem`.
- **External-pkg (stdlib):**
  `adapters/aws/config/dynamodb/loader.go:353:9` — `strconv.ParseInt`.
- **External-pkg (net):** `testutil/*` — `net.Listen`,
  `net.DialTimeout` bare in test harnesses.
- **External-pkg (intra-repo):**
  `deployment/.../bootstrap/app.go:118:10` — `config.Manager.Load`
  returned bare from composition root.
- **Test file:**
  `adapters/amqp/transport/amqp091/integration_consumer_test.go:89:12`
  — `ports.Delivery.Ack` bare.

##### Cross-cutting patterns

1. **Pass-through of internal `ports.*` errors (~46 sites).** Runtime
   and instrumented decorators forward store errors without wrapping.
   Wrapcheck flags because it considers any other module path external.
2. **`ctx.Err()` returned bare (~20 sites).** Idiomatic Go cancellation
   propagation; cheap to wrap (`fmt.Errorf("...: %w", ctx.Err())`) but
   arguably noise.
3. **Cloud SDK calls (AWS DynamoDB, Azure ServiceBus, AMQP).** Bare
   `return err` from adapter methods that already provide context via
   the function name.
4. **Composition root (bootstrap, cmd).** Forwards domain-internal
   errors (`bridge.*`, `config.*`); wrapcheck treats sister modules in
   the workspace as external because of the multi-module layout.
5. **Stdlib leaks.** `strconv.Parse*`, `net.Listen`, `net.DialTimeout`
   (mostly testutil + 1 dynamodb loader).
6. **Test files (10).** Integration tests reuse production helpers and
   trip wrapcheck on `ports.Delivery.Ack` etc.

##### Phase 3 sub-task scope check (mapping inventory → sub-tasks)

- **T012 / `runtime` (36):** likely largely absorbed by allow-list
  decisions in Phase 4 (ports glob + ctx.Err) → reduces to a handful.
- **T003 AWS SQS (4)**, **T008 DynamoDB stores (lease 8 + outbox 2 +
  dlq 1 = 11)** — distinct sweeps; SDK wrap pattern.
- **T004 Azure ServiceBus (12)** — distinct sweep.
- **T005 AMQP 0.9.1 (7)** + **T006 AMQP 1.0 (6)** — distinct sweeps,
  similar shape.
- **T007 MQTT Paho (2)** — small.
- **T009 Native SQLite stores (sqliteoutbox 3 + sqlitedlq 2 + native
  store 2 = 7)** — bundle.
- **T010 Credentials adapters (ssm 1 + native/credentials/file 1 = 2)**
  — small.
- **T011 Metrics/Tracing adapters (otel/metrics 5 + otel/tracing 3 +
  cloudwatch 1 = 9)** — distinct sweep.
- **Out-of-scope-of-current-Phase-3 packages** (~30 hits):
  `deployment/.../bootstrap` (17), `httpapi` (4), `processors/*` (6),
  `config` (3), `bridge` (2), `observability` (1), `tests/integration`
  (2), `testutil/*` (16), `adapters/http/transport` (3),
  `adapters/native/{config/file,cluster}` (4),
  `adapters/aws/config/dynamodb` (5),
  `deployment/.../{cmd/gobridge-filebased,infra}` (3). Phase 4
  allow-list + Phase 3.10 (T012 runtime) likely catches the bulk; the
  bootstrap/cmd composition root may need a deliberate decision in
  Phase 2 (wrap or allow-list internal modules).

Scope appears tractable: ~6–8 real sweep sub-tasks; nothing exceeds
~25 wraps after the Phase-4 allow-list lands.

##### Open questions for Phase 2 (T002)

1. **`ports.*` policy — wrap or allow-list?** Decorators
   (`runtime/instrumented.go`) currently rely on transparent
   passthrough so observability can `errors.Is` against domain
   sentinels. Wrapping with a fresh `fmt.Errorf("…: %w", err)` may
   break sentinel-matching downstream consumers if any rely on the
   exact error type.
2. **`ctx.Err()` (~20 sites).** Allow-list (recommended) vs wrap.
   Affects sample counts above.
3. **Cross-module workspace boundaries.** Wrapcheck flags `bridge`,
   `config`, `deployment/.../infra` as external because of the
   `go.work` multi-module layout. Phase 2 should decide whether to
   widen the internal-module glob or to actually wrap at composition
   roots.
4. **Test files (10).** Project convention not stated here; an
   `_test.go` exclude-rule is the obvious low-cost answer but worth
   confirming in Phase 2.

##### Suggested wrapcheck allow-list seed (for Phase 4 — T013)

```yaml
settings:
  wrapcheck:
    ignoreSigs:
      - .Err()                     # context.Context.Err — idiomatic propagation
    ignoreSigRegexps:
      - \.Err\(\) error$
    ignorePackageGlobs:
      - github.com/mariotoffia/gobridge/ports          # internal port contracts (decorator forwarding)
      - github.com/mariotoffia/gobridge/domain
    ignoreInterfaceRegexps:
      - ^(?i)ports\.                                   # already-wrapped at outer adapter boundary
issues:
  exclude-rules:
    - path: _test\.go
      linters: [wrapcheck]
    - path: testutil/
      linters: [wrapcheck]
```

This seed alone would drop ~70+ of the 162 violations (ports
forwarding + `ctx.Err` + tests + testutil), narrowing the genuine
SDK-boundary fix surface to ~80–90 wraps. Phase 2 (T002) should
confirm or adjust this seed before Phase 3 sweeps begin.

### Phase 2 — Decide per-call wrap vs classify - DONE

**Status:** Resolved 2026-05-04. Authoritative policy authored at
[`_design/error-wrapping-policy.adoc`](_design/error-wrapping-policy.adoc)
(703 lines). Codifies the decision tree, `domain.ErrXxx` sentinel
mapping per adapter family, the four open-question resolutions
(allow-list `ports.*` decorator forwarding; allow-list `ctx.Err()`;
single repo-wide `github.com/mariotoffia/gobridge/...` glob per
the no-backcompat constraint; exempt `_test.go` and `testutil/`,
keep `tests/integration` and `tests/longrunning` enforced),
the binding wrapcheck allow-list seed for T013 (golangci-lint
v2 schema), and a deterministic 11-item sweep checklist for
T003–T012 reviewers. `make lint` passes. Reviewed by `code-reviewer`
across two rounds (initial v1→v2 YAML schema correction landed in
round 1).

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

1. AWS SQS transport (most error paths) - DONE
2. Azure Service Bus transport - DONE
3. AMQP 0.9.1 transport - DONE
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
