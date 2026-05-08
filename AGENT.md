# GoBridge — Agent Instructions

Entry point for any AI coding agent. edit `AGENT.md` only -> all other will symlink to this one.

GoBridge is a multi-module Go 1.25+ message bridge (MQTT, AWS SQS,
Azure Service Bus, AMQP, HTTP, …) built on Hexagonal + DDD + Clean
Architecture, machine-enforced via `.go-arch-lint.yml` plus custom
analyzers (`aclcheck`, `aggcheck`, `cfgshape`).

---

## 1. Reference docs (read on demand)

Do not preload these. Open one only when the task requires it.

| Doc | Open when you need… |
|---|---|
| [`README.md`](README.md) | Elevator pitch, install matrix, doc index |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Hex layers, ports, message flow, error model, HTTP API, observability, headers — §1–§16 |
| [`DDD.md`](DDD.md) | Six bounded contexts inside `domain/` (`shared`, `messaging`, `persistence`, `routing`, `connectivity`, `clock`) and their boundaries |
| [`UBIQUITOUS.md`](UBIQUITOUS.md) | Canonical names. Look up before naming a type, field, or constant |
| [`PLUGIN.md`](PLUGIN.md) | Writing transports / stores / credentials / processors; typed `PluginConfig`; `register.go` self-registration |
| [`TESTS.md`](TESTS.md) | Unit / integration / long-running categories; anti-flake rules; conformance suites; per-test checklist. Read before writing **any** test |
| [`DEVELOPMENT.md`](DEVELOPMENT.md) | Workspace setup, env vars, `make` commands, CI mapping |
| [`.go-arch-lint.yml`](.go-arch-lint.yml) | Source of truth for component dependency rules. §3 below is the maintenance contract |
| [`.golangci.yml`](.golangci.yml) | Lint rule set (depguard, interfacebloat, revive, gochecknoglobals, …) |
| [`docs/`](docs/) | Configuration reference, transport options, processors/stores, scenarios |

In commits and PRs: **link**, do not restate.

---

## 2. Non-negotiable rules

- **Names** come from `UBIQUITOUS.md`. No synonyms for `Envelope`, `Subject`, `Address`, `Delivery`, `Route`, `Lease`, `OutboxRecord`, etc.
- **Cross-context references** in `domain/` go through `domain/shared` or are forbidden (`DDD.md`).
- **Plugin config is typed.** Implement `ports.PluginConfig` (`Kind() string`, `Validate() error`); register decoder in `register.go` `init()` against `ports.DefaultRegistry`; type-assert in factory. `map[string]any` decoding is banned (`cfgshape`). Detail: `PLUGIN.md#typed-plugin-config`.
- **Sender contract**: `ports.Sender.Send(ctx, OutboundMessage)` carrying `Envelope` + resolved `Address`. `Subject` (logical) ≠ `Address` (transport destination); never overload one with the other. Detail: `ARCHITECTURE.md#subject-vs-address`.
- **No `time.Sleep`** in production or test code. Use injected `clock.Clock`. Audited by `make audit-timings` and `make audit-test-timings`.
- **Hand-rolled fakes only.** `gomock`, `mockery`, etc. are banned (`TESTS.md`).
- **Long-running tests** live under `tests/longrunning/`, every file starts with `//go:build longrunning`, run via `make test-long-running` (10800s timeout).
- **Errors** wrap `domain.BridgeError` with class `Transient` | `Permanent` | `Expired` | `Rejected` (`ARCHITECTURE.md#15-error-classification`).
- **Logging**: `*slog.Logger` with `observability.CorrelationHandler`. Pass `ctx` so `correlation_id`/`trace_id`/`span_id` propagate.
- **Compile-time interface assertions** at the bottom of every adapter file: `var _ ports.Sender = (*Sender)(nil)`.
- **Functional options** (`WithXxx(value)`) for adapter constructors.

---

## 3. `.go-arch-lint.yml` maintenance contract

Encodes Clean (Martin) + Hexagonal (Cockburn) + DDD (Evans). Every
edit MUST preserve these. Project convenience never wins over the
architectural rule.

### 3.1 Layer model

| Layer | Components | Allowed inward deps |
|---|---|---|
| 1 — Enterprise / Domain | `domain` (six contexts) | stdlib only |
| 2 — Use Cases / Application | `ports`, `config`, `runtime`, `bridge`, `validate` | inward layers + cross-cutting |
| 2 — Cross-cutting | `logging`, `observability`, `circuitbreaker` | only `domain`/`ports`; no infra |
| 3 — Interface Adapters | `httpapi`, `processors`, `adapters/*/...` (split by role) | only `ports`, `domain`, allowed utilities, vendor SDK |
| 4 — Frameworks & Drivers | `cmd`, `deployment` | anything (composition root) |

`config` is a **shared kernel**. It is the only Layer-2 package that
ships vendor deps (`yaml`, `mapstructure`). Only `adapters/*/config/*`
components may import `config`.

### 3.2 Invariants (verify with `make lint-arch-check`)

1. **Inward-only.** No `mayDependOn` points outward. `domain` depends on nothing; `ports` depends only on `domain`; `runtime` never on adapters.
2. **Adapters depend only on ports.** Each adapter component lists `[domain, logging, ports]` plus one vendor in `canUse`. Exceptions:
   - **Aggregator**: `adapter_store_*_factory` MAY depend on its own store impl packages. Only "sideways" rule.
   - **Config-source**: `adapter_config_*` MAY depend on `config`.
3. **One component per technology.** No blanket `adapters: { in: [adapters/**] }`.
4. **Vendor allowlist mandatory.** `allow.depOnAnyVendor: false`. Inner-ring uses `_no_external_deps_` sentinel and lists no real vendor.
5. **No deprecation aliases / compatibility components / wrapper adapters.**
6. **Mapping regression test stays green.** `make lint-arch-mapping-test` asserts sentinel package→component pairs. Update it when adding a component.

### 3.3 Forbidden

- `bridge` or `config` in any transport/store/credential/cluster adapter `mayDependOn`.
- `runtime` in any adapter.
- Reintroducing the blanket `adapters/**` glob with `adapters → adapters` allowance.
- `anyVendorDeps: true` outside `cmd`/`deployment`.
- A "common" component anything else may import. Shared-kernel role is reserved for `config`; cross-cutting for `logging`, `observability`, `circuitbreaker`.

### 3.4 Adding a component

1. Decide layer (1–4) and role (entity / port / use case / adapter / framework / cross-cutting / shared kernel). If it doesn't fit, the structure is wrong — push back.
2. Use a role-named identifier: `adapter_transport_<tech>`, `adapter_store_<provider>_<role>`, `adapter_config_<source>`. No project jargon.
3. Write `mayDependOn` per §3.1. Add `canUse` only for SDKs actually imported.
4. Add a sentinel to `scripts/lint-arch-mapping-test.sh`.
5. Run `make lint-arch-check`.

### 3.5 When lint conflicts with new code

1. **First instinct: the code is wrong.** Refactor (lift type to `ports`, convert direct call to a port, push logic to composition root).
2. Relax the policy only if the violation reveals a missing architectural concept. Document it in `.go-arch-lint.yml` AND here.
3. Never add a project-specific exception ("paho needs runtime because…"). Name a generic concept or refactor.

---

## 4. Drift verification

### 4.1 Per-PR (mandatory)

```bash
make check        # build + lint + lint-arch-check + unit tests
make check-all    # adds integration tests (Docker required)
```

`make lint` = gofmt + go vet + golangci-lint + `lint-aggregate` (aggcheck). Failure = drift, not noise.

### 4.2 Per-release (advisory)

```bash
make arch-quality
# reports/arch-graph.txt   (go mod graph, plain text)
# reports/dupl.log         (duplicate blocks)
# reports/goconst.log      (repeated literals)
```

### 4.3 Tool matrix

| Tool | Status | Catches | Example violation |
|---|---|---|---|
| `go-arch-lint` component imports | enforcing | Component-to-component direction | `adapters/aws/transport/sqs` imports `bridge` |
| `go-arch-lint` `canUse` | enforcing | Vendor allowlist | `domain` imports `gopkg.in/yaml.v3` |
| `go-arch-lint` deepScan | enforcing | Structural-typing leaks | runtime fn param is a bridge type |
| `lint-arch-mapping-test` | enforcing | Component taxonomy | blanket `adapters/**` reintroduced |
| `golangci-lint depguard` | enforcing | File-level import bans | `runtime` imports `config`; `ports` imports `net/http` |
| `golangci-lint interfacebloat` (max 6) | enforcing | ISP | port grew to 7 methods |
| `golangci-lint revive` | enforcing | Cog/cyc/struct complexity | `bridge.wireRoutes`-class function |
| `golangci-lint gochecknoglobals/inits` | enforcing | Domain purity | global or `init()` in `domain/` |
| `aggcheck` | enforcing | Aggregate-root convention | mutable domain type with transition method but no `Validate()` |
| `aclcheck` | enforcing | ACL placement | SDK type in non-`acl_*.go` file |
| `cfgshape` | enforcing | Typed plugin config | factory decodes `map[string]any` |
| `dupl` | advisory | Missing aggregate / value object | duplicated logic across packages |
| `goconst` | advisory | Missing domain constant | same magic string ≥5 times |
| `arch-graph` | advisory | Module dep direction shift | new edge in `arch-graph.txt` diff |

### 4.4 Failure → fix

| Failure | Fix |
|---|---|
| `lint-arch` | Refactor the offending package; lint is the source of truth. |
| `depguard` denied import | File is in the wrong location, or dep direction is wrong. Move file or invert dep. |
| `interfacebloat` | Split the interface; one method belongs to a different role. |
| `forbidigo time.Now()` | Inject `clock.Clock`; use `clk.Now()`. |
| `forbidigo os.Getenv` | Add to a config DTO; read at composition root. |
| `gochecknoglobals` / `gochecknoinits` in `domain/` | Remove the global. Domain types must be deterministic. |
| `wrapcheck` | Wrap with `fmt.Errorf("...: %w", err)` or `domain.ErrXxx.Wrap(err)`. |
| `aclcheck` on adapter file | Move SDK-touching code into `acl_*.go` (or `acl/` sub-package). |
| `aggcheck` on domain type | Type is becoming an aggregate; rename `*_aggregate.go` and add `Validate()`. |
| `cfgshape` on factory | Define typed `ports.PluginConfig`, register decoder in `register.go`, type-assert in factory. |
| `dupl` | Look for missing aggregate root or domain service before adding a helper. |
| `goconst` | Define a domain-meaningful constant from the ubiquitous language. |
| `arch-graph` new edge | Inward = OK; outward = block. |

### 4.5 Why overlap

`go-arch-lint` enforces component-level direction; `depguard` enforces
file-level import bans (e.g. "non-ACL adapter files cannot import the
AWS SDK"). Belt-and-suspenders for the Dependency Rule.

Custom analyzers (`aclcheck`, `aggcheck`, `cfgshape`) cover
domain-specific conventions (ACL naming, aggregate markers, typed
plugin config) too narrow for general-purpose linters.

Advisory tools (`dupl`, `goconst`, `arch-graph`) catch design smells
no rule can cleanly encode. Review aids, not gates.

### 4.6 Escalation

1. Refactor to comply (95% of cases).
2. If the rule encodes the wrong concept, name the missing concept, add a generic rule, update this file.
3. Never add a project-specific exception.

---

## 5. Working as an agent

1. `make test` and `make lint` before declaring done. Same gates as CI. **Both must be green on the working branch.** If either fails — including issues that pre-date your changes but landed on the branch — fix them. Do not declare "pre-existing" or "unrelated" and move on; you own the branch state when you commit.
2. Architectural changes (new component, new vendor dep, new port) require `.go-arch-lint.yml` edit per §3 + sentinel in `scripts/lint-arch-mapping-test.sh`.
3. Test changes follow `TESTS.md`, including the per-test checklist and bans on `time.Sleep` and mock generators.
4. Documentation belongs in the canonical files in §1. New runbooks go under `docs/runbooks/` or extend an existing doc; do not drop new MDs in the repo root.
