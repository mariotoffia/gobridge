# GoBridge — Agent Instructions

## 1. Read these first

| Document | Why an agent reads it |
|---|---|
| [`README.md`](README.md) | Project elevator pitch, install layout, top-level doc map |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Hexagonal layers, ports, message flow, error model — §1–§16 |
| [`DDD.md`](DDD.md) | Bounded-context decomposition of `domain/` |
| [`UBIQUITOUS.md`](UBIQUITOUS.md) | The vocabulary. **Use these names**; do not invent synonyms |
| [`PLUGIN.md`](PLUGIN.md) | How to write transports, stores, credentials, processors, typed `PluginConfig` |
| [`TESTS.md`](TESTS.md) | Test categories, anti-flake rules, conformance suites — read before writing **any** test |
| [`DEVELOPMENT.md`](DEVELOPMENT.md) | Workspace setup, build/test/lint commands, environment variables |
| [`.go-arch-lint.yml`](.go-arch-lint.yml) | Machine-checked architectural contract (see §3 below) |
| [`docs/`](docs/) | Per-feature reference (configuration, transports, processors, scenarios) |

If something an agent is about to do is already documented above, **link
to it** in commit messages or PR descriptions instead of restating it.

---

## 2. Non-negotiable engineering rules

These are repeated here because they are easy to violate and expensive
to undo. Each links to the canonical detail.

- **Ubiquitous language** — names from [`UBIQUITOUS.md`](UBIQUITOUS.md)
  win. Never introduce a synonym for `Envelope`, `Subject`, `Address`,
  `Delivery`, `Route`, `Lease`, `OutboxRecord`, etc.
- **Bounded contexts** — the six contexts in
  [`DDD.md`](DDD.md) (`shared`, `messaging`, `persistence`, `routing`,
  `connectivity`, `clock`) are the contract. Cross-context references
  go through `domain/shared` or are forbidden.
- **Typed `ports.PluginConfig`** — adapters self-register a decoder in
  `register.go` `init()`; factories type-assert to a concrete config
  struct. `map[string]any` decoding is banned (enforced by the
  `cfgshape` analyzer). See [`PLUGIN.md`](PLUGIN.md#typed-plugin-config).
- **Sender contract** — `ports.Sender.Send(ctx, OutboundMessage)` where
  `OutboundMessage` carries the `Envelope` and the resolved `Address`.
  `Subject` (logical) and `Address` (transport destination) are
  separate; never overload one with the other. See
  [`ARCHITECTURE.md` §3 *Subject vs. Address*](ARCHITECTURE.md#subject-vs-address).
- **No `time.Sleep` in production or test code.** Use the injected
  `clock.Clock`. Audited by `make audit-timings` and
  `make audit-test-timings`. See [`TESTS.md` §3](TESTS.md).
- **Hand-rolled fakes only.** `gomock`, `mockery`, and similar mock
  generators are banned. See [`TESTS.md` §2.1](TESTS.md).
- **Long-running tests** live under `tests/longrunning/`, every file
  starts with `//go:build longrunning`, and they run via
  `make test-long-running` (10 800 s timeout). See
  [`TESTS.md` §6](TESTS.md).
- **Error wrapping** uses `domain.BridgeError` (`Transient`,
  `Permanent`, `Expired`, `Rejected`) so the runtime can route
  failures. See [`ARCHITECTURE.md` §15](ARCHITECTURE.md#15-error-classification).
- **Logging** uses `*slog.Logger` with
  `observability.CorrelationHandler`. Pass `ctx` so
  `correlation_id` / `trace_id` / `span_id` propagate.

---

## 3. Architecture Policy — `.go-arch-lint.yml` maintenance contract

`.go-arch-lint.yml` is the machine-checked expression of three
architectural styles this project commits to:

- **Clean Architecture** (Robert C. Martin) — concentric layers; the
  Dependency Rule: source-code dependencies must point inward only.
- **Hexagonal Architecture** (Alistair Cockburn) — application core
  inside the hexagon; ports are the contracts; adapters live outside.
- **Domain-Driven Design** (Eric Evans) — pure domain layer; ubiquitous
  language and invariants live there; application services orchestrate.

These principles are non-negotiable. Every edit to `.go-arch-lint.yml`
MUST preserve them. Project-specific convenience never wins over the
architectural rule.

### 3.1 Layer model (canonical)

| Layer | Components | Allowed inward deps |
|---|---|---|
| 1 — Enterprise / Domain | `domain` (six bounded contexts) | stdlib only |
| 2 — Use Cases / Application | `ports`, `config`, `runtime`, `bridge`, `validate` | only inward layers + cross-cutting utilities |
| 2 — Cross-cutting utilities | `logging`, `observability`, `circuitbreaker` | only `domain`/`ports`; no infrastructure |
| 3 — Interface Adapters | `httpapi`, `processors`, `adapters/*/...` (split by role) | only `ports`, `domain`, allowed utilities, vendor SDK |
| 4 — Frameworks & Drivers | `cmd`, `deployment` | anything (composition root) |

`config` is a **shared kernel** between application services and the
config-source adapter category. It is the only Layer-2 package that
ships a vendor dependency (`yaml`, `mapstructure`). Adapters that
produce `*config.BridgeConfig` (under `adapters/*/config/*`) are the
**only** adapter category permitted to import `config`.

### 3.2 Required invariants

When editing `.go-arch-lint.yml`, the following MUST remain true.
Verify with `make lint-arch-check` before merging.

1. **Inward-only dependencies.** No `mayDependOn` line points outward
   (e.g., `runtime` must never depend on any adapter; `domain` must
   never depend on anything; `ports` only depends on `domain`).
2. **Adapters depend only on ports.** Each adapter component lists
   only `[domain, logging, ports]` plus a single vendor name in
   `canUse:`. Never `bridge`, `config`, `runtime`, `httpapi`, or
   another adapter — except:
   - **Aggregator exception**: store-factory aggregators
     (`adapter_store_*_factory`) MAY depend on their own store
     implementation packages. This is the only "sideways" rule.
   - **Config-source exception**: `adapter_config_*` components MAY
     depend on `config` because their job is to load it.
3. **One adapter component per technology.** Splitting by tech
   prevents a flaw in one driver leaking into another (a Hex/DDD
   bounded-context guarantee). Never collapse adapter components into
   a blanket "adapters" rule.
4. **Vendor allowlist is mandatory.** `allow.depOnAnyVendor` stays
   `false`. Each component lists its vendor needs explicitly via
   `canUse`. Inner-ring components use the `_no_external_deps_`
   sentinel and MUST NOT add real vendor entries.
5. **No deprecation aliases.** Per the project-wide No Backward
   Compatibility rule, do not add compatibility components, deprecated
   re-exports, or wrapper adapters whose only purpose is to bridge old
   APIs.
6. **Mapping regression test stays green.** `make lint-arch-mapping-test`
   asserts sentinel packages map to expected components. Update the
   test alongside the policy when a new component is added.

### 3.3 Forbidden patterns

- Adding `bridge` or `config` to a transport/store/credential/cluster
  adapter's `mayDependOn`. The bridge converts config to ports specs
  at the boundary; adapters never see config.
- Adding `runtime` to any adapter. Adapters never reach into use cases.
- Reintroducing a blanket `adapters: { in: [adapters/**] }` component
  with `adapters → adapters` allowance.
- Adding `anyVendorDeps: true` to anything outside `cmd`/`deployment`.
- Introducing a "common" component that anything else may import. The
  shared-kernel role is reserved for `config`; cross-cutting is for
  `logging`, `observability`, `circuitbreaker` only.

### 3.4 Workflow when adding a new component

1. Decide its **layer** (1–4) and its **role** (entity / port / use case
   / adapter / framework / cross-cutting utility / shared kernel). If
   you cannot place it cleanly, the structure is wrong — push back.
2. Choose a **role-named** identifier (`adapter_transport_<tech>`,
   `adapter_store_<provider>_<role>`, `adapter_config_<source>`, etc.).
   Never use project-jargon names.
3. Write its `mayDependOn` strictly per the layer model above. Add
   `canUse:` only for vendor SDKs the implementation actually imports.
4. Add a sentinel assertion to `scripts/lint-arch-mapping-test.sh`.
5. Run `make lint-arch-check`.

### 3.5 Workflow when an existing rule conflicts with new code

If `make lint-arch-check` fails:

1. **First instinct: the code is wrong.** Architecture lint is the
   policy of record. Refactor the code so it complies (typical fixes:
   convert a direct call into a port, lift a type into `ports`, push
   the offending logic up to a composition root).
2. **Only relax the policy if the violation reveals a missing
   architectural concept** (e.g., a previously-unrecognised
   cross-cutting utility). When relaxing, document the rationale in a
   comment in `.go-arch-lint.yml` AND in this file.
3. Never add a project-specific exception ("paho needs runtime
   because…"). If a real architectural justification exists, name
   the new concept and add a generic rule.

---

## 4. Architecture drift verification

The codebase enforces Clean / Hexagonal / DDD via a stack of
overlapping linters and analyzers. **Drift is caught only if you run
the right tool in the right phase.**

### 4.1 Per-PR (mandatory — gates merge)

```bash
make check        # build + lint + lint-arch-check + unit tests
make check-all    # adds integration tests (Docker required)
```

`make lint` runs gofmt, go vet, golangci-lint (per `.golangci.yml`),
and `lint-aggregate` (the `aggcheck` custom analyzer). A failure here
is **drift, not noise**. Fix the code; do not relax the rule.

### 4.2 Per-release (advisory — review aid)

```bash
make arch-quality
# Produces:
#   reports/arch-graph.txt  -- module dep graph (`go mod graph`, plain text)
#   reports/dupl.log        -- duplicate code blocks
#   reports/goconst.log     -- repeated literals
```

Advisory tools highlight smells that may indicate missing aggregates,
missing value objects, or shifting dependency directions. Nothing
fails CI; a human (or LLM) reviews the diffs.

### 4.3 What each tool catches

| Tool | Status | Layer of enforcement | Example violation |
|---|---|---|---|
| `go-arch-lint` (component imports) | enforcing | Component-to-component direction | `adapters/aws/transport/sqs` imports `bridge` |
| `go-arch-lint` (vendor `canUse`) | enforcing | External-dep allowlist | `domain` imports `gopkg.in/yaml.v3` |
| `go-arch-lint` (deepScan) | enforcing | Structural-typing leaks across components | runtime function parameter is a bridge type |
| `lint-arch-mapping-test` | enforcing | Component taxonomy stays role-based | someone reintroduces a blanket `adapters/**` glob |
| `golangci-lint depguard` | enforcing | File-pattern import bans (finer than go-arch-lint) | `runtime` imports `config`; `ports` imports `net/http` |
| `golangci-lint interfacebloat` | enforcing (max=6) | ISP enforcement | a port grew to 7 methods |
| `golangci-lint revive` (cog/cyc/struct) | enforcing | Cognitive/cyclomatic complexity | a `bridge.wireRoutes`-class function appears |
| `golangci-lint gochecknoglobals/inits` | enforcing | Domain purity | a global or `init()` was added inside `domain/` |
| `aggcheck` (custom) | enforcing | Aggregate-root convention | a domain type with mutable state and a transition method but no `Validate()` |
| `aclcheck` (custom) | enforcing | Anti-corruption layer placement | SDK type appears in a non-`acl_*.go` file |
| `cfgshape` (custom) | enforcing | Typed plugin config | a factory still decodes `map[string]any` instead of a `PluginConfig` struct |
| `dupl` (advisory) | advisory | Missing aggregate / value object | duplicated logic across two packages |
| `goconst` (advisory) | advisory | Missing domain constants | the same magic string appears 5+ times |
| `arch-graph` (text dep diff) | advisory | Module dep direction shift | visible in `arch-graph.txt` diff between PR commits |

### 4.4 Failure → fix translation

When a tool fails, the **first instinct is "the code is wrong."** Do
not relax the rule.

| Failure | First instinct |
|---|---|
| `lint-arch` fails | Refactor the offending package's structure; the architecture lint is the source of truth. |
| `depguard` denies an import | The file is in the wrong location, or the dependency is in the wrong direction. Move the file or invert the dependency. |
| `interfacebloat` flags a port | Split the interface; one of the methods belongs to a different role. |
| `forbidigo` flags `time.Now()` | Inject `clock.Clock` and use `clk.Now()`. |
| `forbidigo` flags `os.Getenv` | Add the value to a config DTO, read it at the composition root. |
| `gochecknoglobals` / `gochecknoinits` flags `domain/` | Remove the global. Domain types must be deterministic. |
| `wrapcheck` flags an error | Wrap with `fmt.Errorf("...: %w", err)` or `domain.ErrXxx.Wrap(err)`. |
| `aclcheck` flags an adapter file | Move the SDK-touching code into `acl_*.go` (or an `acl/` sub-package). |
| `aggcheck` flags a domain type | The type is becoming an aggregate; rename to `*_aggregate.go` and add `Validate()`. |
| `cfgshape` flags a factory | Define a typed `ports.PluginConfig`, register a decoder in `register.go`, and type-assert in the factory. |
| `dupl` highlights repeated logic | Look for a missing aggregate root or domain service before adding a helper function. The duplication is often a missing concept. |
| `goconst` highlights a repeated literal | Define a domain-meaningful constant — pick a name from the ubiquitous language. |
| `arch-graph.txt` diff shows a new edge | Audit the edge: did a new dep direction get introduced? If yes, is it inward (OK) or outward (block). |

### 4.5 Why each tool exists (overlap is intentional)

`go-arch-lint` enforces **component-level** dependency direction.
`golangci-lint depguard` enforces **file-level** import bans (more
fine-grained — e.g., "non-ACL adapter files cannot import the AWS
SDK"). The two overlap deliberately: belt-and-suspenders for the
most architecturally-load-bearing rule (the Dependency Rule).

The custom analyzers (`aclcheck`, `aggcheck`, `cfgshape`) cover
convention patterns — Anti-Corruption Layer naming, aggregate-root
markers, typed plugin config — that are domain-specific and would be
too cumbersome to express via a general-purpose linter.

The advisory tools (`dupl`, `goconst`, `arch-graph`) catch design
smells that no rule can cleanly encode. They are **review aids**, not
gates: false positives are common, and over-zealous deduplication or
premature naming hurts more than it helps. `arch-graph` writes plain
text (`go mod graph` output) so an LLM agent or `git diff` can
inspect it directly.

### 4.6 When to escalate

If you find yourself wanting to disable a check or relax a rule:

1. **Refactor the code** to comply with the existing rule. This is the
   right answer in 95% of cases.
2. **Recognise that the rule encodes the wrong architectural concept**
   and that the failure points at a missing one. Document the new
   concept (e.g., a previously-unrecognised cross-cutting utility),
   add a *generic* rule for it, and update this AGENT.md.
3. Never add a project-specific exception ("paho is special because…").

---

## 5. Conventions cheat sheet

Things easy to forget mid-task. Each item links to the canonical
explanation; do not duplicate it elsewhere.

- **Compile-time interface assertion** at the bottom of every adapter
  file:
  ```go
  var _ ports.Sender = (*Sender)(nil)
  ```
- **Functional options** (`WithXxx(value)`) for adapter constructors —
  see [`PLUGIN.md`](PLUGIN.md).
- **Factory registration** lives in `register.go` `init()` against
  `ports.DefaultRegistry`. See
  [`PLUGIN.md` *Typed Plugin Config*](PLUGIN.md#typed-plugin-config).
- **Multi-module workspace.** From the root, `go build ./...` and
  `make test` traverse every module via `go.work`. To work in a single
  adapter: `cd adapters/<path> && go test -v ./...`.
- **Structured errors** flow as `domain.BridgeError` with a class —
  see [`ARCHITECTURE.md` §15](ARCHITECTURE.md#15-error-classification).

---

## 6. Doing work as an agent

1. Run `make test` and `make lint` before declaring any change done.
   These are the only verification gates; CI runs the same.
2. Architectural changes (new component, new vendor dep, new port)
   require a `.go-arch-lint.yml` edit per §3 above and a sentinel in
   `scripts/lint-arch-mapping-test.sh`.
3. Test changes follow [`TESTS.md`](TESTS.md) — including the
   per-test checklist and the ban on `time.Sleep` and mock generators.
4. Documentation lives in the canonical files listed in §1. If you
   are tempted to drop a runbook into the repo root, prefer a
   `docs/runbooks/` entry or extend an existing doc.

---

*Anything beyond what is documented above is **incidental**, not
contract. When in doubt, prefer the architecture rules over local
convenience.*
