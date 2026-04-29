# Architecture Lint Remediation Plan

## Purpose

This plan tracks the work needed to make `go-arch-lint` a useful architecture
guardrail for gobridge.

The current architecture lint integration is intentionally useful but not yet
strict enough. It reports real boundary issues, and it also exposes a problem in
the lint model itself: the `adapters` component is too broad. A broad component
can be acceptable while adopting a tool, but it is not acceptable as the final
policy because it hides cross-adapter coupling.

This document has no line limit. Keep it detailed enough that implementation
agents can pick up a task without reconstructing the full design discussion.

## Inputs Reviewed

- `reports/go-arch-lint.log`
- `.go-arch-lint.yml`
- `ARCHITECTURE.md`
- `DEVELOPMENT.md`
- `PLUGIN.md`
- `bridge/factories.go`
- `ports/factories.go`
- `ports/config.go`
- `config/interfaces.go`
- Adapter factory/config packages called out by the lint report

Four expert reviews were incorporated:

| Reviewer | Focus | Main conclusion |
|---|---|---|
| Go expert | Package/interface design | `adapters` is too coarse, `bridge_factory.go` files contaminate adapter implementation packages, and config loader interfaces have split ownership. |
| AWS expert | AWS adapter boundaries | AWS concrete implementations are mostly clean; noisy findings are primarily factory/config edges. AWS store aggregators need explicit modeling. |
| API/plugin expert | Extension API and migration | Public factory APIs force plugins to import `bridge` and `config`; plugin documentation contradicts the intended port-first boundary. |
| Architecture expert | Hexagonal rules and lint sequencing | `ports -> config` is a core boundary leak, adapter component granularity must be fixed, vendor checks should later protect the zero-dependency core. |

## Current State

`make lint-arch-report` succeeds and writes a report. `make lint-arch` fails.

Current remaining notices after the first coarse false-positive cleanup: 21.

Grouped by issue:

| Group | Current finding count | Files |
|---|---:|---|
| Transport adapter packages import `bridge` and `config` | 12 | `adapters/amqp/transport/amqp091/bridge_factory.go`, `adapters/amqp/transport/amqp10/bridge_factory.go`, `adapters/aws/transport/sqs/bridge_factory.go`, `adapters/azure/transport/servicebus/factory.go`, `adapters/http/transport/factory.go`, `adapters/mqtt/transport/paho/bridge_factory.go` |
| Config adapters import `config` | 4 | `adapters/aws/config/dynamodb/loader.go`, `adapters/aws/config/dynamodb/streams.go`, `adapters/native/config/file/source.go`, `adapters/native/config/file/watcher.go` |
| Store factory aggregators import `bridge` and `config` | 4 | `adapters/aws/store/factory.go`, `adapters/native/store/factory.go` |
| `ports` imports `config` | 1 | `ports/config.go` |

Hidden policy gaps:

| Gap | Why it matters |
|---|---|
| `adapters -> adapters` is currently allowed wholesale | This avoids some false positives today, but it would not catch an incorrect import such as MQTT depending on SQS. |
| Vendor dependency lint is disabled globally | This conflicts with the claim that core packages have zero external dependencies. |
| Plugin docs say adapters should follow ports, but examples use `bridge.TransportFactory` and `config.*` | The public API nudges plugin authors toward the boundary violation. |

## Non-Negotiable Design Constraints

### No Backward Compatibility

This is a hard, project-wide constraint that supersedes any task or
sub-section in this plan that talks about deprecation, aliases, or
migration shims:

- Do **not** keep deprecated aliases for renamed types or interfaces.
- Do **not** add wrapper/adapter types whose only purpose is to bridge
  the old API shape to the new one.
- Do **not** keep old package paths alive via re-exports or stub files.
- Do **not** preserve old method signatures alongside new ones.
- Do **not** add `// Deprecated:` comments hoping future cleanup will
  remove the symbol; remove it now.

When an API shape changes:

- Delete the old symbol in the same change that introduces the new one.
- Update every in-tree caller (built-in adapters, `cmd`, tests, docs,
  examples) to the new shape.
- If a built-in adapter cannot move to the new shape immediately, the
  task is not done — finish the migration before closing the task.

Implications for tasks below:

- `ARCH-LINT-006` (compatibility aliases/wrappers) is repurposed: it
  must instead enforce a **clean break** — verify no deprecated alias,
  re-export, or wrapper survives after the ports-first APIs land.
- Transport and store adapter migration tasks must remove the old
  `bridge_factory.go`/`factory.go` shapes outright, not leave them as
  thin wrappers.
- Documentation updates must remove old examples instead of marking
  them deprecated.

### Pluggable Configuration Must Stay Pluggable

The config model must remain plugin-friendly. Do not solve architecture lint by
centralizing every plugin-specific option shape into the core `config` package.

Target behavior:

- Core configuration keeps generic extension points such as `options:
  map[string]any`.
- Each component/plugin owns its typed option shape.
- Each component/plugin parses and validates its own options at its boundary.
- The bridge converts generic gobridge config definitions into port-level specs.
- Plugins receive port-level specs, not `config.*Def` values.
- Config source adapters may depend on `config` because their job is to load a
  canonical `*config.BridgeConfig`.

Example target pattern for a plugin:

```go
type ReceiverConfig struct {
    QueueURL string
    WaitTimeSeconds int
}

func ReceiverConfigFromOptions(options map[string]any) (ReceiverConfig, error) {
    // Parse and validate plugin-owned shape here.
}
```

The bridge should not know that shape. The bridge should only pass a generic
`ports.ReceiverSpec{Options: def.Options}` or equivalent.

### Do Not Hide Real Architecture Debt

Do not fix the report by:

- Excluding `bridge_factory.go` files.
- Allowing all adapters to import `bridge`.
- Allowing all adapters to import `config`.
- Keeping `adapters -> adapters` as a final rule.
- Moving plugin option structs into the central `config` package.

### Keep Adoption Non-Blocking Until The Policy Is Correct

`make lint-arch-report` should remain non-blocking while the taxonomy and code
boundaries are being fixed. `make lint-arch` should become blocking only when it
is precise and the report is clean.

### Every Task Must Leave Lint And Tests Passing

After each implementation task in this plan, `make lint` and `make test` must
pass before the task is considered complete.

If either command fails, the implementer must fix the failures before closing
the task, even when the failure appears to be pre-existing. This is intentional:
architecture cleanup changes package boundaries and public APIs, so carrying
known lint or test failures forward makes it too easy to hide regressions.

Minimum verification after each task:

```bash
make lint
make test
```

Additional task-specific checks such as `make lint-arch-report`, targeted
package tests, or integration tests still apply where listed below.

### Package Moves Must Be Mechanical And Reviewable

Some tasks may extract package responsibilities into new directories. For
example, a task might split a bridge-facing adapter factory away from a transport
implementation package. Per the No Backward Compatibility rule, package moves
must not introduce new wrapper or shim packages whose only purpose is to
preserve the old import path or API shape.

When moving packages or files:

- Use `git mv` when moving tracked files.
- Use normal filesystem directory creation for new packages.
- Do not copy-paste moved files by hand when a move preserves file history.
- Keep package moves separate from behavior changes when practical.
- Use one or more shell commands or scripts for broad import-path updates.
- Prefer deterministic bulk edits for import rewrites, followed by `gofmt`.
- Verify every broad rewrite with `rg` before and after.
- Do not leave stale imports, duplicate packages, or compatibility files.
  Old files must be deleted in the same change that moves their contents.

Recommended workflow for package extraction:

```bash
git mv old/path/file.go new/path/file.go
rg "github.com/mariotoffia/gobridge/old/path"
# run scripted import rewrite
gofmt -w new/path old/path
make lint
make test
```

If a task updates many import references, do it with an explicit script or a
small set of bash commands so the change is reproducible and easy to review.
After the rewrite, run `rg` for the old import path and inspect any remaining
matches.

## Target Architecture

### Package Rules

| Component | May depend on | Must not depend on |
|---|---|---|
| `domain` | Standard library only | Any gobridge package |
| `ports` | `domain` only | `config`, `bridge`, `runtime`, adapters |
| `config` | `domain`, standard library, YAML/JSON parsing dependencies | `ports`, `bridge`, `runtime`, adapters |
| `logging`, `observability` | Standard library only, unless a specific rule is added | `domain`, `ports`, `bridge`, adapters |
| `runtime` | `domain`, `ports`, `logging`, `observability` | `config`, `bridge`, adapters |
| `bridge` | `config`, `domain`, `logging`, `ports`, `runtime` | adapters |
| transport implementation adapters | `domain`, `ports`, `logging`, optional approved helper packages, vendor SDKs | `bridge`, `config`, other unrelated adapters |
| store implementation adapters | `domain`, `ports`, `logging`, vendor SDKs | `bridge`, `config`, unrelated adapters |
| store factory/aggregator adapters | `ports`, own implementation packages, vendor SDKs | `bridge`, `config` after the port-level store factory migration |
| config source adapters | `config`, `domain`, optional clock/logging, vendor SDKs | `bridge`, `runtime`, transport/store adapters |
| observability adapters | `domain`, `ports`, vendor SDKs | `bridge`, `config`, transport/store adapters |
| `cmd` and deployment modules | Any project dependency needed for composition | None, because they are composition roots |

### Factory API Direction

The current public factory interfaces live in `bridge` and use `config.*Def`.
That forces adapters/plugins to import both `bridge` and `config`.

Target:

- Port-level factory contracts live in `ports`.
- Factory methods accept `ports.*Spec` values.
- The bridge owns conversion from `config.*Def` to `ports.*Spec`.
- Existing adapter implementation packages implement `ports` contracts directly.
- Old `ports.TransportFactory` / `ports.StoreFactory` interfaces are
  **deleted**, not aliased. Per the No Backward Compatibility rule, no
  shims remain in `bridge`.

Candidate target interfaces:

```go
package ports

type TransportFactory interface {
    NewSession(ctx context.Context, spec SessionSpec) (Session, error)
    NewReceiver(ctx context.Context, spec ReceiverSpec, session Session) (Receiver, error)
    NewSender(ctx context.Context, spec SenderSpec, session Session) (Sender, error)
    Capabilities() []Capability
}

type StoreSpec struct {
    Type    string
    Options map[string]any
}

type StoreFactory interface {
    NewLeaseStore(ctx context.Context, spec StoreSpec) (LeaseStore, error)
    NewOutboxStore(ctx context.Context, spec StoreSpec) (OutboxStore, error)
    NewDLQStore(ctx context.Context, spec StoreSpec) (DLQStore, error)
}
```

Existing `ports.SessionSpec`, `ports.ReceiverSpec`, and `ports.SenderSpec`
already point in this direction.

### Config Source Ownership

`ports/config.go` currently imports `config`, while `config/interfaces.go`
defines similar interfaces. This is muddled ownership.

Target:

- `config.Loader`, `config.Watcher`, and `config.Reloader` are canonical for
  config sources.
- `ports` stops importing `config`.
- Config source adapters implement `config.Loader` / `config.Watcher`.
- Runtime and transport ports stay free of config model dependencies.

### Adapter Component Granularity

Final lint components should be role-specific and non-overlapping. Exact names
can change during implementation, but the final policy should model these roles:

| Role | Examples |
|---|---|
| `adapter_transport_impl_*` | MQTT Paho, SQS, Azure Service Bus, AMQP 0-9-1, AMQP 1.0, HTTP transport |
| `adapter_store_impl_*` | `memorylease`, `memoryoutbox`, `memorydlq`, `sqliteoutbox`, `sqlitedlq`, DynamoDB lease/outbox/DLQ |
| `adapter_store_factory_*` | `adapters/native/store`, `adapters/aws/store` |
| `adapter_config_*` | `adapters/native/config/file`, `adapters/aws/config/dynamodb` |
| `adapter_credentials_*` | file credentials, AWS SSM credentials |
| `adapter_observability_*` | OTel metrics/tracing, CloudWatch metrics |
| `adapter_cluster_*` | native cluster resolver, AWS ECS resolver |

The final policy should avoid a blanket `adapters -> adapters` rule. Instead,
only named factory/aggregator components should be allowed to import the
implementation packages they aggregate.

## All Current Lint Findings And Disposition

| Finding | Current file | Classification | Target resolution |
|---|---|---|---|
| AMQP 0-9-1 adapter imports `bridge` | `adapters/amqp/transport/amqp091/bridge_factory.go` | Real package boundary debt | Move bridge/config adaptation out of the implementation package or change the public registration API to accept port-level factory specs. |
| AMQP 0-9-1 adapter imports `config` | `adapters/amqp/transport/amqp091/bridge_factory.go` | Real package boundary debt | Bridge converts config to `ports.*Spec`; adapter parses plugin options from specs. |
| AMQP 1.0 adapter imports `bridge` | `adapters/amqp/transport/amqp10/bridge_factory.go` | Real package boundary debt | Same as AMQP 0-9-1. |
| AMQP 1.0 adapter imports `config` | `adapters/amqp/transport/amqp10/bridge_factory.go` | Real package boundary debt | Same as AMQP 0-9-1. |
| SQS adapter imports `bridge` | `adapters/aws/transport/sqs/bridge_factory.go` | Real package boundary debt | Prefer registering the lower-level SQS factory directly through a port-level interface. |
| SQS adapter imports `config` | `adapters/aws/transport/sqs/bridge_factory.go` | Real package boundary debt | Bridge owns config-to-spec conversion. |
| Azure Service Bus adapter imports `bridge` | `adapters/azure/transport/servicebus/factory.go` | Real package boundary debt | Split bridge-facing adapter from implementation or migrate the implementation to port-level factory APIs. |
| Azure Service Bus adapter imports `config` | `adapters/azure/transport/servicebus/factory.go` | Real package boundary debt | Bridge owns config-to-spec conversion. |
| HTTP transport adapter imports `bridge` | `adapters/http/transport/factory.go` | Real package boundary debt | Split HTTP composition factory from HTTP implementation or migrate to port-level factory APIs. |
| HTTP transport adapter imports `config` | `adapters/http/transport/factory.go` | Real package boundary debt | Bridge owns config-to-spec conversion. |
| MQTT Paho adapter imports `bridge` | `adapters/mqtt/transport/paho/bridge_factory.go` | Real package boundary debt | Register a ports-first `Factory` directly or move bridge adapter to a composition package. |
| MQTT Paho adapter imports `config` | `adapters/mqtt/transport/paho/bridge_factory.go` | Real package boundary debt | Bridge owns config-to-spec conversion. |
| AWS DynamoDB config adapter imports `config` | `adapters/aws/config/dynamodb/loader.go` | Acceptable policy, currently mis-modeled | Classify as `adapter_config_aws_dynamodb`; it may depend on `config`. |
| AWS DynamoDB config stream support imports `config` | `adapters/aws/config/dynamodb/streams.go` | Acceptable policy, currently mis-modeled | Same config-adapter rule. |
| Native file config adapter imports `config` | `adapters/native/config/file/source.go` | Acceptable policy, currently mis-modeled | Classify as `adapter_config_native_file`; it may depend on `config`. |
| Native file config watcher imports `config` | `adapters/native/config/file/watcher.go` | Acceptable policy, currently mis-modeled | Same config-adapter rule. |
| AWS store aggregator imports `bridge` | `adapters/aws/store/factory.go` | Transitional debt | Move store factory contract to `ports`; remove `bridge` import. |
| AWS store aggregator imports `config` | `adapters/aws/store/factory.go` | Transitional debt | Use `ports.StoreSpec` instead of `config.StoreConfig`. |
| Native store aggregator imports `bridge` | `adapters/native/store/factory.go` | Transitional debt | Move store factory contract to `ports`; remove `bridge` import. |
| Native store aggregator imports `config` | `adapters/native/store/factory.go` | Transitional debt | Use `ports.StoreSpec` instead of `config.StoreConfig`. |
| `ports` imports `config` | `ports/config.go` | Real core boundary leak | Move config source ports to `config` and remove the import from `ports`. |

## Hidden Issues Not Fully Exposed By The Current Report

| Issue | Why current lint does not catch it | Fix |
|---|---|---|
| A transport adapter could import another transport adapter | `adapters -> adapters` is allowed wholesale | Split adapter components and remove blanket self-dependency. |
| Store implementation packages could accidentally depend on store aggregators | Same broad `adapters` component | Add directed `store_factory -> store_impl` rules only. |
| Core packages could gain external dependencies | `depOnAnyVendor: true` disables vendor lint globally | Enable vendor checks and explicitly allow vendor dependencies only where intended. |
| Plugin docs direct users to boundary-crossing APIs | Lint does not check documentation | Update plugin docs after APIs are corrected. |

## Task Plan

Status legend:

- `Planned`: not started.
- `Blocked`: depends on a prior task.
- `Review`: requires human architecture/API decision before coding.
- `Done`: complete.

The user explicitly requested `skill-create-test` and
`skill-asiidoc-documentation`. The repository contains the documentation skill
as `skill-asciidoc-documentation`; use that spelling in execution plans.

| ID | Task | Status | Primary agents | Skills to use | Main files | Outcome |
|---|---|---|---|---|---|---|
| ARCH-LINT-001 | Decide final dependency rule for config sources | Review | Architecture expert, API expert, Go expert | `skill-create-test` for tests; `skill-asciidoc-documentation` for docs | `ports/config.go`, `config/interfaces.go`, `config/manager.go`, `ARCHITECTURE.md`, `DEVELOPMENT.md`, `PLUGIN.md` | A written decision: config source interfaces live in `config`; `ports` no longer imports `config`. |
| ARCH-LINT-002 | Consolidate config loader/watcher interfaces in `config` | Planned | Go expert, API expert | `skill-create-test` | `ports/config.go`, `config/interfaces.go`, adapters under `adapters/*/config/*`, bridge/supervisor config loading call sites | `ports -> config` lint finding removed. Config source adapters implement `config.Loader` and `config.Watcher`. |
| ARCH-LINT-003 | Preserve pluggable typed plugin options | Planned | API expert, Architecture expert, Go expert | `skill-create-test`; `skill-asciidoc-documentation` | `ports/factories.go`, adapter config parsing files, `PLUGIN.md` | Core config keeps generic `Options`; plugins own typed config parsing and validation. |
| ARCH-LINT-004 | Introduce port-level transport factory interface | Planned | API expert, Go expert, Architecture expert | `skill-create-test` | `ports/factories.go`, `bridge/factories.go`, `bridge/builder.go`, `bridge/supervisor.go` | New plugin-facing transport factory contract accepts `ports.SessionSpec`, `ports.ReceiverSpec`, and `ports.SenderSpec`; adapters no longer need `config.*Def`. |
| ARCH-LINT-005 | Introduce port-level store factory interface and `ports.StoreSpec` | Planned | API expert, Go expert, AWS expert | `skill-create-test` | `ports/factories.go`, `bridge/factories.go`, `bridge/builder*.go`, `adapters/aws/store/factory.go`, `adapters/native/store/factory.go` | Store factories no longer need `bridge` or `config`. |
| ARCH-LINT-006 | Verify clean break: no compatibility aliases or wrappers survive | Planned | API expert, Go expert | `skill-create-test`; `skill-asciidoc-documentation` | `bridge/factories.go`, `bridge/doc.go`, `PLUGIN.md` | All in-tree callers use ports-first APIs; deprecated bridge factory interfaces and config-shaped wrappers are deleted, not aliased. |
| ARCH-LINT-007 | Migrate MQTT Paho factory off `bridge` and `config` | Blocked by ARCH-LINT-004 | Go expert, API expert | `skill-create-test` | `adapters/mqtt/transport/paho/bridge_factory.go`, `adapters/mqtt/transport/paho/factory.go`, package tests | MQTT implementation package imports only allowed inner packages and vendor deps. |
| ARCH-LINT-008 | Migrate SQS factory off `bridge` and `config` | Blocked by ARCH-LINT-004 | AWS expert, Go expert, API expert | `skill-create-test` | `adapters/aws/transport/sqs/bridge_factory.go`, `adapters/aws/transport/sqs/factory.go`, package tests | SQS implementation package imports only allowed inner packages and AWS SDK deps. |
| ARCH-LINT-009 | Migrate AMQP 0-9-1 factory off `bridge` and `config` | Blocked by ARCH-LINT-004 | Go expert, API expert | `skill-create-test` | `adapters/amqp/transport/amqp091/bridge_factory.go`, package tests | AMQP 0-9-1 implementation package has no `bridge` or `config` import. |
| ARCH-LINT-010 | Migrate AMQP 1.0 factory off `bridge` and `config` | Blocked by ARCH-LINT-004 | Go expert, API expert | `skill-create-test` | `adapters/amqp/transport/amqp10/bridge_factory.go`, package tests | AMQP 1.0 implementation package has no `bridge` or `config` import. |
| ARCH-LINT-011 | Migrate Azure Service Bus factory off `bridge` and `config` | Blocked by ARCH-LINT-004 | Go expert, API expert | `skill-create-test` | `adapters/azure/transport/servicebus/factory.go`, package tests | Service Bus implementation package has no `bridge` or `config` import. |
| ARCH-LINT-012 | Migrate HTTP transport factory off `bridge` and `config` | Blocked by ARCH-LINT-004 | API expert, Go expert | `skill-create-test` | `adapters/http/transport/factory.go`, package tests | HTTP implementation package has no `bridge` or `config` import. |
| ARCH-LINT-013 | Migrate AWS store aggregator off `bridge` and `config` | Blocked by ARCH-LINT-005 | AWS expert, Go expert, API expert | `skill-create-test` | `adapters/aws/store/factory.go`, `adapters/aws/store/factory_test.go` | AWS store factory depends on `ports.StoreSpec` and concrete DynamoDB store packages only. |
| ARCH-LINT-014 | Migrate native store aggregator off `bridge` and `config` | Blocked by ARCH-LINT-005 | Go expert, API expert | `skill-create-test` | `adapters/native/store/factory.go`, `adapters/native/store/factory_test.go` | Native store factory depends on `ports.StoreSpec` and concrete native store packages only. |
| ARCH-LINT-015 | Model config adapters explicitly in lint config | Blocked by ARCH-LINT-002 | Architecture expert, AWS expert, Go expert | `skill-create-test` for lint tests | `.go-arch-lint.yml`, `adapters/aws/config/dynamodb`, `adapters/native/config/file` | Config adapters may import `config`; other adapters may not. |
| ARCH-LINT-016 | Split adapter lint components by role | Planned | Architecture expert, Go expert, AWS expert | `skill-create-test` | `.go-arch-lint.yml` | Adapter components become precise enough to catch bad cross-adapter imports. |
| ARCH-LINT-017 | Remove blanket `adapters -> adapters` dependency | Blocked by ARCH-LINT-016 | Architecture expert, Go expert | `skill-create-test` | `.go-arch-lint.yml` | Only named factory/aggregator components may depend on named implementation packages. |
| ARCH-LINT-018 | Add directed store factory to store implementation lint rules | Blocked by ARCH-LINT-016 | Architecture expert, AWS expert, Go expert | `skill-create-test` | `.go-arch-lint.yml` | `adapters/aws/store` and `adapters/native/store` can aggregate their own implementations, but implementations cannot depend back. |
| ARCH-LINT-019 | Add directed transport implementation lint rules | Blocked by ARCH-LINT-016 | Architecture expert, Go expert, API expert | `skill-create-test` | `.go-arch-lint.yml` | MQTT, SQS, AMQP, Service Bus, and HTTP transport adapters cannot depend on each other. |
| ARCH-LINT-020 | Enable vendor dependency guardrails for core packages | Blocked by ARCH-LINT-016 | Architecture expert, Go expert | `skill-create-test` | `.go-arch-lint.yml`, `go.mod`, package imports | Core zero-external-dependency claim is enforced by lint. |
| ARCH-LINT-021 | Update plugin guide to ports-first factory model | Blocked by ARCH-LINT-004 and ARCH-LINT-005 | API expert, Architecture expert | `skill-asciidoc-documentation`; `skill-create-test` for examples | `PLUGIN.md`, `README.md`, `DEVELOPMENT.md`, maybe AsciiDoc docs if introduced | Documentation stops instructing plugin authors to import `bridge` from adapter packages. |
| ARCH-LINT-022 | Update architecture and development docs with final lint policy | Blocked by ARCH-LINT-016 through ARCH-LINT-020 | Architecture expert | `skill-asciidoc-documentation` | `ARCHITECTURE.md`, `DEVELOPMENT.md`, `.go-arch-lint.yml` comments if added | Docs and lint rules agree. |
| ARCH-LINT-023 | Add architecture lint regression tests or scripted checks | Blocked by ARCH-LINT-016 | Go expert, Architecture expert | `skill-create-test` | `Makefile`, `scripts/`, possibly test fixture packages under `tests` or `scripts` | Bad cross-adapter imports are caught by `make lint-arch`. |
| ARCH-LINT-024 | Ratchet `lint-arch` into CI/check | Blocked by zero lint findings | Architecture expert, API expert | `skill-create-test`; `skill-asciidoc-documentation` | `Makefile`, `.github/workflows/*`, `DEVELOPMENT.md` | `make check` or CI runs strict `make lint-arch`. |

## Detailed Task Notes

### ARCH-LINT-001: Decide Final Dependency Rule For Config Sources

Problem:

- `ports/config.go` imports `config`, which violates the documented rule that
  `ports` imports only `domain`.
- `config/interfaces.go` already defines similar loader/watcher interfaces.
- Config source adapters are real adapters, but their purpose is to return
  `*config.BridgeConfig`.

Decision to make:

- Make `config.Loader`, `config.Watcher`, and `config.Reloader` canonical.
- Delete `ports.ConfigLoader`, `ports.ConfigWatcher`, and
  `ports.ConfigReloader`. Per the No Backward Compatibility rule, no
  deprecated aliases are kept in `ports`.

Decision (recorded):

- Use `config` ownership. `ports/config.go` is deleted outright; every
  in-tree user of the old `ports.Config*` types is migrated in the same
  change.

Acceptance criteria:

- `ports` has no import of `config`.
- `go-arch-lint` no longer reports `ports -> config`.
- Config source adapters still support pluggable source implementations.

### ARCH-LINT-002: Consolidate Config Loader/Watcher Interfaces In `config`

Implementation steps:

1. Add `type Reloader interface { Loader; Watcher }` to `config/interfaces.go`
   if it is not already present.
2. Replace uses of `ports.ConfigLoader`, `ports.ConfigWatcher`, and
   `ports.ConfigReloader` with `config.Loader`, `config.Watcher`, and
   `config.Reloader`.
3. Update compile-time checks in:
   - `adapters/aws/config/dynamodb/loader.go`
   - `adapters/native/config/file/source.go`
   - `adapters/native/config/file/watcher.go`
   - any tests or config manager code that uses the ports variants.
4. Remove `ports/config.go` or leave deprecated aliases only if a migration
   period is required.

Testing:

- Use `skill-create-test`.
- Add/adjust unit tests for `config.Manager`.
- Add compile-time interface checks against `config.Loader` and
  `config.Watcher`.
- Run `make lint-arch-report`.
- Run targeted Go tests for config source adapters.

### ARCH-LINT-003: Preserve Pluggable Typed Plugin Options

Problem:

- Architecture lint should not push plugin-specific schemas into core
  `config`.
- The core config should provide generic option maps.
- Plugins must parse the actual shape they require.

Implementation guidance:

- Keep `Options map[string]any` on generic config definitions.
- Keep or introduce typed plugin config structs inside plugin packages.
- Require each plugin package to expose option parsing helpers such as:
  - `ReceiverConfigFromOptions`
  - `SenderConfigFromOptions`
  - `SessionConfigFromOptions`
  - `StoreConfigFromOptions`
- The bridge passes options through; plugins validate them.

Acceptance criteria:

- No new plugin-specific option struct is added to core `config` unless the
  option is genuinely cross-plugin.
- Plugin docs show typed option parsing inside the adapter package.
- Tests cover invalid option shapes for migrated plugins.

### ARCH-LINT-004: Introduce Port-Level Transport Factory Interface

Problem:

- `ports.TransportFactory` currently requires `config.SessionDef`,
  `config.ReceiverDef`, and `config.SenderDef`.
- Adapter implementation packages import `bridge` and `config` to satisfy this
  interface.

Implementation direction:

- Add `ports.TransportFactory` accepting `ports.*Spec`.
- Make bridge builder/supervisor registration accept the port-level interface.
- Delete the old `ports.TransportFactory` interface in the same change. Per
  the No Backward Compatibility rule, no compatibility functions or
  deprecated aliases remain.

Bridge conversion responsibility:

- Convert `config.SessionDef` to `ports.SessionSpec`.
- Convert `config.ReceiverDef` to `ports.ReceiverSpec`.
- Convert `config.SenderDef` to `ports.SenderSpec`.
- Translate generic fields such as topic/subscription plans where needed.
- Preserve `Options` without interpreting plugin-specific shape.

Acceptance criteria:

- Transport implementation packages can register without importing `bridge`.
- Transport implementation packages can create sessions/receivers/senders
  without importing `config`.
- Existing builder tests cover the conversion layer.

### ARCH-LINT-005: Introduce Port-Level Store Factory Interface

Problem:

- `ports.StoreFactory` accepts `config.StoreConfig`.
- Store aggregators import `bridge` and `config`.

Implementation direction:

- Add `ports.StoreSpec`.
- Add `ports.StoreFactory`.
- Add optional `ports.DistributedStoreFactory` or move the existing optional
  distributed capability out of `bridge`.
- Bridge converts `config.StoreConfig` to `ports.StoreSpec`.
- Store factories parse `spec.Options` into typed store-specific config.

Acceptance criteria:

- `adapters/aws/store/factory.go` no longer imports `bridge` or `config`.
- `adapters/native/store/factory.go` no longer imports `bridge` or `config`.
- Store implementation packages remain independent of store aggregators.

### ARCH-LINT-006: Verify Clean Break (No Compatibility Aliases Or Wrappers)

This task enforces the project-wide **No Backward Compatibility** rule
declared in the Non-Negotiable Design Constraints. There is no migration
window: the breaking changes from ARCH-LINT-004 and ARCH-LINT-005 must
land as a single clean cutover.

Forbidden:

- Type aliases such as `type TransportFactory = ports.TransportFactory`
  in the `bridge` package as a "compatibility shim".
- Adapter wrappers like `bridgeFactoryAdapter` that translate old
  `config.*Def` calls to the new `ports.*Spec` calls.
- Stub packages that re-export deleted symbols.
- `// Deprecated:` comments — remove the symbol instead.

Required:

- Old `ports.TransportFactory` and `ports.StoreFactory` interfaces are
  deleted in the same change that introduces the ports-first contracts.
- Every in-tree caller (cmd, deployment modules, integration tests,
  built-in adapters, examples in docs) is rewritten in the same change.
- Old configuration-keyed registration helpers, if any, are deleted.

Acceptance criteria:

- `git grep` for the old type names returns zero non-history matches.
- No `// Deprecated:` markers added by this remediation effort remain.
- No file named `compat*.go`, `legacy*.go`, or similar exists for this
  remediation.
- Examples in `PLUGIN.md`, `README.md`, and `docs/` use the new
  ports-first APIs only.

### ARCH-LINT-007 through ARCH-LINT-012: Transport Adapter Migrations

Applies to:

- MQTT Paho
- SQS
- AMQP 0-9-1
- AMQP 1.0
- Azure Service Bus
- HTTP transport

Common implementation steps:

1. Identify the low-level factory that already accepts `ports.*Spec`.
2. If missing, add low-level factory methods that accept `ports.*Spec`.
3. Remove `BridgeFactory` or move bridge-specific adaptation into a separate
   composition package.
4. If files are moved to a separate package, use `git mv` for tracked files and
   scripted import rewrites for all changed package references.
5. Update `cmd`, deployment modules, and tests to register ports-first factories.
6. Keep plugin option parsing inside the adapter package.

Acceptance criteria:

- No transport implementation package imports `bridge`.
- No transport implementation package imports `config`.
- `make lint-arch-report` no longer reports these transport packages.
- Targeted package tests pass.

### ARCH-LINT-013 and ARCH-LINT-014: Store Aggregator Migrations

Applies to:

- `adapters/aws/store`
- `adapters/native/store`

Common implementation steps:

1. Replace `ports.StoreFactory` compile-time checks with `ports.StoreFactory`.
2. Replace `config.StoreConfig` method parameters with `ports.StoreSpec`.
3. Parse typed store options locally.
4. Keep factory packages as aggregators that depend on their own store
   implementation packages.
5. Add lint rules that allow only those directed dependencies.

Acceptance criteria:

- Store factory packages no longer import `bridge`.
- Store factory packages no longer import `config`.
- Store implementation packages do not import aggregators.

### ARCH-LINT-015: Model Config Adapters Explicitly

Problem:

- Config adapters importing `config` are currently reported because all adapters
  are treated the same.

Implementation direction:

- Add component(s) for:
  - `adapters/native/config/file`
  - `adapters/aws/config/dynamodb`
- Allow those components to depend on `config`.
- Do not allow transport/store implementation adapters to depend on `config`.

Acceptance criteria:

- `adapters/aws/config/dynamodb -> config` is allowed.
- `adapters/native/config/file -> config` is allowed.
- `adapters/mqtt/transport/paho -> config` would still fail if reintroduced.

### ARCH-LINT-016 through ARCH-LINT-019: Precise Adapter Component Rules

Implementation direction:

- Replace:

```yaml
adapters: { in: [adapters, adapters/**] }
```

with non-overlapping components by role and technology.

Candidate structure:

```yaml
components:
  adapter_transport_mqtt_paho:
    in: [adapters/mqtt/transport/paho]
  adapter_transport_sqs:
    in: [adapters/aws/transport/sqs]
  adapter_transport_servicebus:
    in: [adapters/azure/transport/servicebus]
  adapter_transport_amqp091:
    in: [adapters/amqp/transport/amqp091]
  adapter_transport_amqp10:
    in: [adapters/amqp/transport/amqp10]
  adapter_transport_http:
    in: [adapters/http/transport]

  adapter_store_native_factory:
    in: [adapters/native/store]
  adapter_store_native_memorylease:
    in: [adapters/native/store/memorylease]
  adapter_store_native_memoryoutbox:
    in: [adapters/native/store/memoryoutbox]
  adapter_store_native_memorydlq:
    in: [adapters/native/store/memorydlq]
  adapter_store_native_sqliteoutbox:
    in: [adapters/native/store/sqliteoutbox]
  adapter_store_native_sqlitedlq:
    in: [adapters/native/store/sqlitedlq]

  adapter_store_aws_factory:
    in: [adapters/aws/store]
  adapter_store_aws_dynamodblease:
    in: [adapters/aws/store/dynamodblease]
  adapter_store_aws_dynamodboutbox:
    in: [adapters/aws/store/dynamodboutbox]
  adapter_store_aws_dynamodbdlq:
    in: [adapters/aws/store/dynamodbdlq]

  adapter_config_native_file:
    in: [adapters/native/config/file]
  adapter_config_aws_dynamodb:
    in: [adapters/aws/config/dynamodb]
```

Notes:

- The exact syntax must be validated with `go-arch-lint self-inspect`.
- Patterns should be non-overlapping. If a parent package and child packages
  both need separate treatment, test the mapping with `go-arch-lint mapping`.
- Add more components for credentials, cluster, metrics, and tracing adapters.

Acceptance criteria:

- `go-arch-lint mapping --project-path . -s grouped` shows each package in the
  intended component.
- The broad `adapters` component is removed.
- The broad `adapters -> adapters` dependency is removed.
- A bad cross-transport import would fail lint.

### ARCH-LINT-020: Enable Vendor Dependency Guardrails

Problem:

- `.go-arch-lint.yml` currently has `depOnAnyVendor: true`.
- The report says vendor import lint is off.
- The architecture claims the core module has zero external dependencies.

Implementation direction:

- Enable vendor checks after component granularity is stable.
- Allow vendor dependencies only for components that need them.
- Keep core layers constrained.

Likely policy:

| Component | Vendor dependency policy |
|---|---|
| `domain` | No vendor dependencies |
| `ports` | No vendor dependencies |
| `config` | Only approved config parser dependencies, if any |
| `runtime` | No vendor dependencies unless explicitly justified |
| `bridge` | No vendor dependencies unless explicitly justified |
| adapters | Vendor SDKs allowed as needed |
| processors | Vendor deps allowed only when processor needs them |
| `httpapi` | Vendor deps allowed only if documented |

Acceptance criteria:

- Vendor lint is enabled.
- Core zero-dependency claim is enforceable.
- Any vendor exception is explicit in `.go-arch-lint.yml`.

### ARCH-LINT-021 and ARCH-LINT-022: Documentation

Use `skill-asciidoc-documentation`.

Docs to update:

- `PLUGIN.md`
- `ARCHITECTURE.md`
- `DEVELOPMENT.md`
- `README.md` if examples change

Required documentation changes:

- Plugin authors implement port-level factories.
- Plugin-specific option shapes live in plugin packages.
- The bridge owns generic config-to-spec conversion.
- Config source adapters are a special adapter category that may depend on
  `config`.
- Store factory aggregators are a special adapter category with directed
  dependencies on their own store implementations.
- `make lint-arch-report` is for migration; `make lint-arch` is the strict gate.

Acceptance criteria:

- Docs no longer tell adapter authors to import `bridge` from implementation
  packages.
- Docs explain typed plugin option parsing.
- Architecture docs and `.go-arch-lint.yml` match.

### ARCH-LINT-023: Regression Checks

Use `skill-create-test`.

Possible checks:

- A script target that verifies `go-arch-lint self-inspect` has no notices.
- A target that verifies `go-arch-lint mapping` maps key packages to expected
  components.
- A small fixture or scripted import check, if maintainable.

Acceptance criteria:

- A future broadening of `adapters` rules is visible in review.
- Component mapping mistakes are easy to catch.

### ARCH-LINT-024: Ratchet Into CI

Implementation direction:

1. Keep `make lint-arch-report` while findings remain.
2. Once `make lint-arch` is clean, add it to `make check`.
3. Add it to GitHub Actions if CI exists.
4. Keep report output as an artifact if useful.

Acceptance criteria:

- `make check` fails on architecture violations.
- CI fails on architecture violations.
- `reports/go-arch-lint.log` remains available for local diagnosis.

## Suggested Implementation Sequence

1. `ARCH-LINT-001`: Decide final config-source dependency rule.
2. `ARCH-LINT-002`: Move config source interfaces out of `ports`.
3. `ARCH-LINT-003`: Document and test plugin-owned typed options.
4. `ARCH-LINT-004`: Add port-level transport factory interface.
5. `ARCH-LINT-005`: Add port-level store factory interface.
6. `ARCH-LINT-006`: Verify clean break (no compatibility aliases or wrappers survive).
7. `ARCH-LINT-007` through `ARCH-LINT-012`: Migrate transport adapters.
8. `ARCH-LINT-013` and `ARCH-LINT-014`: Migrate store aggregators.
9. `ARCH-LINT-015`: Model config adapters explicitly.
10. `ARCH-LINT-016`: Split adapter components.
11. `ARCH-LINT-017`: Remove blanket `adapters -> adapters`.
12. `ARCH-LINT-018`: Add directed store factory rules.
13. `ARCH-LINT-019`: Add directed transport adapter rules.
14. `ARCH-LINT-020`: Enable vendor dependency guardrails.
15. `ARCH-LINT-021` and `ARCH-LINT-022`: Update docs.
16. `ARCH-LINT-023`: Add regression checks.
17. `ARCH-LINT-024`: Ratchet strict lint into `make check` and CI.

## Risk Register

| Risk | Impact | Mitigation |
|---|---|---|
| Moving factory APIs breaks existing users | Accepted by the No Backward Compatibility rule | Migrate every in-tree caller in the same change as the API rename; communicate the breaking change in release notes. |
| Config pluggability regresses | High | Keep plugin-specific options in adapter packages; bridge only passes generic option maps. |
| go-arch-lint component patterns overlap unexpectedly | Medium | Use `go-arch-lint mapping` after each config change. |
| Too many tiny lint components become hard to maintain | Medium | Use role-based grouping where safe; split by technology only where cross-adapter coupling matters. |
| Vendor lint creates noisy findings | Medium | Enable vendor lint after package taxonomy is stable; add explicit exceptions. |
| Documentation drifts from code | Medium | Update docs as part of the API migration and mark documentation tasks as required before CI ratchet. |

## Definition Of Done

Architecture lint remediation is done when:

- Every completed implementation task has left `make lint` and `make test`
  passing.
- `go-arch-lint self-inspect --project-path . --json` has no notices.
- `go-arch-lint mapping --project-path . -s grouped` maps packages to precise,
  intentional components.
- `make lint-arch` passes.
- `make lint-arch-report` still produces a useful report when failures occur.
- `make check` or CI includes strict architecture lint.
- `ports` no longer imports `config`.
- Transport implementation adapter packages no longer import `bridge` or
  `config`.
- Store implementation adapter packages do not import aggregators or unrelated
  adapters.
- Config adapters are explicitly allowed to import `config`.
- Plugin-specific typed option shapes remain inside plugin packages.
- `PLUGIN.md`, `ARCHITECTURE.md`, and `DEVELOPMENT.md` describe the same
  dependency rules enforced by `.go-arch-lint.yml`.
