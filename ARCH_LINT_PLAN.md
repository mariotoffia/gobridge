# Architecture Lint Plan

## Purpose

This plan captures the architecture-lint findings and the clean-break
implementation work needed to keep `gobridge` aligned with Clean
Architecture, Hexagonal Architecture, and DDD.

The current `make lint-arch` report is clean:

```text
OK - No warnings found
```

That result is useful, but it is only as strong as the policy expressed in
`.go-arch-lint.yml`. The main finding from the first review was that a coarse
configuration can hide real architecture leaks. A blanket rule such as
`adapters -> adapters` is itself a defect if it remains the final policy,
because it would allow unrelated adapters to depend on each other. For
example, an MQTT adapter depending on SQS should be impossible.

The target state is a precise, pluggable policy:

- each architectural role is a separate go-arch-lint component;
- each adapter/plugin has the exact dependency shape it needs;
- aggregators/factories may depend on their own implementations only;
- implementations may not depend sideways on sibling implementations;
- all external dependencies are explicitly admitted through `vendors`;
- `make lint` is the single static-health command for the repo.

## Non-negotiable Requirements

- **MUST: No backward compatibility.** Do not add deprecated aliases, shim
  packages, compatibility wrappers, or transitional public APIs. Delete old
  symbols in the same change that introduces replacements, and update every
  in-tree caller atomically.
- **MUST: Make the config pluggable without making it amorphous.** Adapter and
  plugin config may be extensible, but each component must have a concrete
  schema or DTO shape that can be linted, validated, tested, and documented.
  Avoid permanent `map[string]any` or raw JSON/YAML blobs crossing core
  boundaries.
- **MUST: `make lint` and `make test` pass after each task.** This applies per
  task, not only at the end of the plan. If a pre-existing problem blocks the
  command, fix it in the same task.
- **MUST: Package moves use filesystem-safe or Git-safe operations.** Use
  `git mv` for tracked package moves. Update imports with scripted `gofmt` /
  `go list` / shell tooling, not manual one-off edits scattered across files.
- **MUST: Keep the mapping regression test in sync.** When adding, removing,
  or moving a component in `.go-arch-lint.yml`, update
  `scripts/lint-arch-mapping-test.sh`.
- **MUST: Architecture rules are not optional.** If a strict rule conflicts
  with current code, the code is the thing to fix.
- **MUST: Tests use `skill-create-test`.** Every implementation task below
  should include focused regression tests and should be treated as requiring
  the `skill-create-test` workflow.
- **MUST: Documentation uses `skill-asiidoc-documentation`.** Every behavior,
  architecture, or operational change below should include the associated
  AsciiDoc/documentation update using the `skill-asiidoc-documentation`
  workflow.

Note: the two named skills above are recorded because they are required by
this plan. If they are not installed in a local Codex session, install/enable
them before starting implementation, or explicitly document the fallback used.

## Expert Review Summary

### Architecture Expert

The first policy risk was over-broad component grouping. A coarse
`adapters -> adapters` permission is not a harmless adoption detail if it
survives into the target state. It lets unrelated driven adapters share
implementation details and bypass ports. The correct long-term model is the
one now expressed by the current `.go-arch-lint.yml`: role-specific and
technology-specific components, with narrowly scoped aggregator exceptions.

Remaining architecture tightening should focus on:

- removing remaining core leaks into adapters, such as adapter code importing
  a concrete `circuitbreaker` package instead of a port;
- preventing infrastructure types from entering `ports`;
- splitting overly broad inner-ring components only when the split maps to a
  real bounded context or architectural role;
- preserving the distinction between adapter factories and adapter
  implementations.

### Go Expert

The static-health command should be boring and repeatable. `make lint` now
needs to remain the default static gate and should include:

- strict architecture lint;
- component mapping regression;
- `gofmt` check;
- `go vet` across every module that has default-tag packages.

For future tasks, import rewrites should be scripted and followed by
`gofmt`. If package moves are needed, use `git mv` and then run a single
mechanical import update. The multi-module workspace means each task must
test all affected modules, not only the root module.

### AWS Expert

AWS-specific adapters must remain isolated by role:

- SQS transport is a transport adapter, not a general AWS utility package;
- DynamoDB outbox, DLQ, lease, and config are separate adapter roles;
- CloudWatch metrics is observability export, not runtime orchestration;
- AWS SDK imports must stay in AWS adapter components or deployment code.

The architecture policy should keep AWS SDK access out of `domain`, `ports`,
`runtime`, `bridge`, and generic processors. Any shared AWS helper that would
be imported by multiple adapter categories should be treated with suspicion;
prefer small local helpers inside the specific adapter component unless a
proper port or composition-root abstraction exists.

### API Expert

The API boundary must not leak HTTP framework types into the core. `httpapi`
is a driving adapter; `ports` should describe application contracts, not
`net/http` contracts. If core code needs mountable endpoints, define a
transport-neutral port and keep `http.Handler`, request parsing, response
headers, and status-code rendering inside the HTTP adapter.

For configuration APIs, separate parsed core configuration from the delivery
mechanism. HTTP handlers should translate requests into application commands
or DTOs, and runtime/bridge code should depend on those DTOs or ports rather
than on the HTTP adapter.

## Current Policy Baseline

The current `.go-arch-lint.yml` already has the important baseline choices:

- `allow.depOnAnyVendor: false` means every external dependency must be
  explicitly whitelisted in `vendors`.
- `allow.deepScan: true` means method-call and structural dependency leaks are
  checked, not only direct imports.
- `ignoreNotFoundComponents: false` means unmapped packages are treated as a
  policy failure.
- tests, test utilities, reports, docs, scripts, and generated audit material
  are excluded from production architecture lint.
- adapters are split by technology and role, including transport, store,
  config, credentials, metrics, tracing, and cluster components.
- store factories are modeled separately from store implementations so the
  allowed dependency is factory-to-implementation, not
  implementation-to-factory or implementation-to-implementation.

The current `make lint` static-health gate should include:

```text
make lint
  -> make lint-arch-check
       -> make lint-arch
       -> make lint-arch-mapping-test
  -> make lint-gofmt
  -> make lint-go-vet
```

## Findings And Required Fixes

### Finding F-001: Coarse Adapter Rules Are A Real Architecture Risk

**Status:** Mostly addressed by the current component split, but must remain
guarded.

**Problem:** A blanket `adapters -> adapters` permission would hide bad
coupling such as MQTT depending on SQS, SQS depending on Azure Service Bus, or
SQLite store code depending on DynamoDB store code.

**Target:** No broad adapter-to-adapter rule. Each adapter implementation has
its own component. Only factory/aggregator packages may depend on their own
implementation packages.

**How to solve:**

1. Keep transport components split by concrete technology.
2. Keep store components split by backing store and responsibility.
3. Keep factory components separate from implementation components.
4. Add sentinel package checks to `scripts/lint-arch-mapping-test.sh`.
5. Run `make lint && make test`.

### Finding F-002: Adapter Factories Need Narrow, Explicit Exceptions

**Status:** Present in current policy; must be maintained when new adapters are
added.

**Problem:** Factory packages such as `adapters/native/store` need to assemble
their own implementations. If modeled too coarsely, that exception becomes a
general adapter dependency rule.

**Target:** Factory components may depend on their own implementation
components only.

**How to solve:**

1. Keep `adapter_store_native_factory` and `adapter_store_aws_factory`
   separate from implementation components.
2. Do not let implementation packages import the factory package.
3. When adding a new store plugin, add both the implementation component and
   the factory dependency edge explicitly.
4. Update mapping tests for the new implementation and factory if relevant.
5. Run `make lint && make test`.

### Finding F-003: Pluggable Config Must Still Be Strongly Shaped

**Status:** Needs ongoing enforcement.

**Problem:** Plugin-style config can easily degrade into raw maps or ad-hoc
JSON/YAML blobs passed through the core. That makes linting and validation
weak, and it hides adapter-specific requirements.

**Target:** Config remains pluggable, but every adapter/plugin has an actual
typed shape:

- the core config package owns shared DTOs and stable cross-boundary shapes;
- each adapter/plugin owns or registers its concrete config shape;
- validation runs before runtime assembly;
- runtime and domain code do not parse adapter-specific raw config;
- no permanent compatibility aliases are kept.

**How to solve:**

1. Inventory current config structs and raw extension points.
2. For each plugin category, define the required typed config object.
3. Keep parser/decoder details inside config-source adapters or the config
   shared kernel, not in runtime.
4. Use discriminated config blocks or explicit registration instead of raw
   maps where possible.
5. Add validation tests for each plugin config shape.
6. Document each plugin config shape in the public docs.
7. Run `make lint && make test`.

### Finding F-004: `circuitbreaker` Is A Concrete Dependency From An Adapter

**Status:** Open, tracked as a final tightening task.

**Problem:** `adapters/mqtt/transport/paho` imports the concrete
`circuitbreaker` package. That forces an adapter to know a project-internal
implementation detail.

**Target:** Paho depends on a resilience port. The concrete circuit breaker is
wired at the composition root.

**How to solve:**

1. Add a small `ports.CircuitBreaker` or equivalent resilience port with the
   exact methods needed by the adapter.
2. Make the concrete `circuitbreaker` implementation satisfy that port.
3. Change Paho code to depend on the port, not the implementation package.
4. Wire the concrete implementation in `cmd`/deployment/composition code.
5. Remove `circuitbreaker` from the Paho component's `mayDependOn`.
6. Run `make lint && make test`.

### Finding F-005: Infrastructure Types Must Not Leak Into `ports`

**Status:** Open, tracked as a final tightening task.

**Problem:** A port such as `ports.HTTPMountable` that exposes `http.Handler`
pulls a delivery mechanism into the port layer. The architectural concept is a
core endpoint or capability, not necessarily HTTP.

**Target:** `ports` remains transport-neutral. HTTP types stay in HTTP
adapters.

**How to solve:**

1. Inventory all `ports` imports of infrastructure packages such as
   `net/http` and `database/sql`.
2. Replace HTTP-specific ports with transport-neutral request/response or
   endpoint abstractions.
3. Translate between `http.Handler` and the neutral port inside `httpapi` or
   `adapters/http/transport`.
4. Add vendor entries for visible stdlib infrastructure imports and deny them
   from `ports` by omission from `canUse`.
5. Run `make lint && make test`.

### Finding F-006: `bridge` / `httpapi` Should Not Over-Couple To Config

**Status:** Open, tracked as a final tightening task.

**Problem:** If `bridge` and `httpapi` depend directly on broad parsed config
structures, config can become an all-purpose shared object instead of a
boundary DTO.

**Target:** Config is parsed once into typed DTOs; application services depend
on narrow command/config ports or small DTOs specific to the use case.

**How to solve:**

1. Inventory `bridge -> config` and `httpapi -> config` usage.
2. Split broad config access into smaller use-case DTOs where needed.
3. Keep parsing and source-specific config handling out of runtime.
4. Update call sites atomically with no compatibility aliases.
5. Run `make lint && make test`.

### Finding F-008: Vendor Admission Control Must Stay Explicit

**Status:** Present in current policy; must be maintained.

**Problem:** External dependencies can silently enter inner layers if vendor
lint is disabled or too permissive.

**Target:** Every non-stdlib dependency is declared under `vendors` and
explicitly admitted only to the components that should use it.

**How to solve:**

1. Keep `allow.depOnAnyVendor: false`.
2. Add new vendor entries only with a matching component policy change.
3. Keep AWS SDK, Azure SDK, Paho, AMQP, SQLite, OTel, and processor libraries
   out of core components unless explicitly justified.
4. Run `make lint && make test`.

### Finding F-009: Tests Are Excluded From Architecture Lint

**Status:** Accepted for production lint baseline; future optional hardening.

**Problem:** Tests are excluded to avoid noisy adoption, but tests can still
teach bad coupling patterns or import internal implementation details.

**Target:** Production lint remains strict. Test linting can be added later as
an advisory or separate target once production rules are stable.

**How to solve:**

1. Keep production `make lint` strict.
2. Add a future `lint-arch-tests` advisory target if test coupling becomes a
   problem.
3. Start with high-value bans only, such as core tests importing adapters where
   a fake port would be better.
4. Run `make lint && make test`.

### Finding F-010: Static Health Needs One Obvious Command

**Status:** Addressed by `make lint`.

**Problem:** If architecture lint, formatting, and vetting are separate manual
commands, contributors will miss at least one of them.

**Target:** `make lint` is the single static-health command.

**How to solve:**

1. Keep `make lint` depending on `lint-arch-check`, `lint-gofmt`, and
   `lint-go-vet`.
2. Keep `lint-arch-check` depending on strict architecture lint and mapping
   regression.
3. Keep `lint-go-vet` multi-module aware.
4. Run `make lint && make test` after edits to the Makefile.

## Implementation Task Table

| ID | Task | Main owner agent/expert | Required skills/workflows | Package moves? | Implementation notes | Verification |
|---|---|---|---|---|---|---|
| ARCH-001 | Preserve precise adapter/plugin component split | Architecture expert + Go expert | `skill-create-test`, `skill-asiidoc-documentation` | No | Keep every adapter implementation mapped separately. No blanket `adapters/**` component. | `make lint && make test` |
| ARCH-002 | Maintain factory-to-implementation exceptions only | Architecture expert + Go expert | `skill-create-test`, `skill-asiidoc-documentation` | No, unless adding a new package | Factories may import their own implementations. Implementations may not import factories or sibling implementations. | `make lint && make test` |
| ARCH-003 | Define typed pluggable config shapes for every plugin category | API expert + Architecture expert + Go expert | `skill-create-test`, `skill-asiidoc-documentation` | Possibly | Replace raw unbounded extension maps with typed DTOs or explicit registration. No compatibility aliases. Use `git mv` and scripted import rewrites if packages move. | `make lint && make test` |
| ARCH-004 | Remove adapter dependency on concrete `circuitbreaker` | Go expert + Architecture expert | `skill-create-test`, `skill-asiidoc-documentation` | No expected move | Introduce a resilience port, make concrete breaker satisfy it, wire in composition root, remove adapter `mayDependOn: circuitbreaker`. | `make lint && make test` |
| ARCH-005 | Remove infrastructure types from `ports` | API expert + Go expert + Architecture expert | `skill-create-test`, `skill-asiidoc-documentation` | Possibly | Replace HTTP-specific port shapes with transport-neutral ports. Keep `net/http` in HTTP adapters. No backwards-compatible `HTTPMountable` shim. | `make lint && make test` |
| ARCH-006 | Narrow `bridge`/`httpapi` config coupling | API expert + Architecture expert + Go expert | `skill-create-test`, `skill-asiidoc-documentation` | Possibly | Split broad parsed config access into narrow use-case DTOs or ports. Keep parsing out of runtime. | `make lint && make test` |
| ARCH-008 | Keep vendor admission explicit | Go expert + AWS expert + API expert | `skill-create-test`, `skill-asiidoc-documentation` | No | Add vendor entries only with matching component `canUse`. AWS SDK stays in AWS adapters/deployment. HTTP dependencies stay in HTTP adapters. | `make lint && make test` |
| ARCH-009 | Add optional architecture linting for tests | Architecture expert + Go expert | `skill-create-test`, `skill-asiidoc-documentation` | No expected move | Add as advisory first, then promote only when signal is high. Do not weaken production lint. | `make lint && make test` |
| ARCH-010 | Keep `make lint` as the single static-health gate | Go expert | `skill-create-test`, `skill-asiidoc-documentation` | No | Include architecture lint, mapping regression, gofmt, and multi-module `go vet`. Add future static checks here, not as hidden side commands. | `make lint && make test` |
| ARCH-011 | Update mapping regression whenever components change | Architecture expert + Go expert | `skill-create-test`, `skill-asiidoc-documentation` | No | Add sentinel packages for every new component category. Prevent accidental broad globs. | `make lint && make test` |
| ARCH-012 | Document component/plugin rules | Architecture expert + API expert + AWS expert | `skill-asiidoc-documentation`, `skill-create-test` | No | Document allowed dependency direction, plugin config shape, and where SDK/framework types may live. | `make lint && make test` |

## Package Move Protocol

Use this protocol for any task that moves Go packages:

1. Decide whether a physical move is required. Prefer a lint component split
   without a directory move when the current layout already expresses the
   domain language clearly.
2. Move tracked files with `git mv`.
3. Rewrite imports mechanically. Use `go list`, `gofmt`, and a small shell or
   Go script so the rewrite is repeatable.
4. Run `gofmt` on all touched Go files.
5. Run `make lint`.
6. Run `make test`.
7. Update docs in the same change.

Do not leave forwarding packages behind for compatibility.

## Documentation Requirements

For each task, update the relevant documentation using
`skill-asiidoc-documentation`:

- describe the architectural role of any new component;
- describe the config shape for any new plugin/adapter;
- document any removed public symbols as removed, not deprecated;
- update examples in the same change as the code;
- keep architecture docs aligned with `.go-arch-lint.yml`.

## Test Requirements

For each task, use `skill-create-test` to add or update tests:

- focused unit tests for new ports or config DTOs;
- adapter tests proving translation stays inside the adapter;
- mapping regression updates for new go-arch-lint components;
- static verification through `make lint`;
- behavioral verification through `make test`.

## Done Criteria

This plan is complete when:

- `make lint` passes and includes architecture lint, mapping regression,
  gofmt, and multi-module `go vet`;
- `make test` passes;
- `.go-arch-lint.yml` has no broad adapter-to-adapter dependency rule;
- every adapter/plugin component has an explicit dependency shape;
- every plugin config has a typed, documented shape;
- no core component imports infrastructure SDK/framework packages;
- no compatibility shims remain from the migration;
- documentation and mapping tests match the implemented architecture.
