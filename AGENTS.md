# GoBridge Agent Rules

Canonical file. `.claude/CLAUDE.md` is a symlink to this. If the two ever disagree, preserve user edits and report it.

GoBridge is a Go 1.25+ multi-module message bridge (MQTT, AWS SQS, Azure Service Bus, AMQP 0-9-1, AMQP 1.0, HTTP, …) built on Hexagonal + DDD + Clean Architecture. Naming, layering, and config shape are machine-enforced — this file is navigation, not a rule restatement.

## First read

- `LANGUAGE.md` — communication style. Read once, apply always. Terse for chat/status/findings/handoffs. Full clarity for destructive, security, production, migration, legal, compliance, privacy, or ambiguous work.

## MUST: Where to look — task → doc

| Doing this | Read |
|---|---|
| Naming a type, field, constant, config key, header, or concept | `UBIQUITOUS.md` (authoritative glossary) |
| Adding / changing a transport, store, credential, processor, or plugin config | `PLUGIN.md` (typed `ports.PluginConfig`, `register.go`, factories) |
| Writing or modifying any test | `TESTS.md` (categories, anti-flake, build tags) |
| Changing layering, dependencies, or adding a new component | `ARCHITECTURE.md §2` + `.go-arch-lint.yml` (source of truth) |
| Understanding the domain shape or bounded contexts | `DDD.md` |
| Lint failed; need to know which checker, which report, how to fix | `LINT.md` |
| Local setup, env vars, CI mapping | `DEVELOPMENT.md` |
| Local dev workspace setup, adding a module, or cutting a release (the simple front door) | `MODULES.md` (`make dev` / `make release`) |
| Tagging a release, bumping inter-module requires, adding/removing a `replace`, versioning a module | `RELEASE.md` (tag names, dependency-ordered release, no replaces in published modules) + `DEVELOPMENT.md` §Module versioning & references |
| Operating against the running bridge | `docs/` (configuration, transports, processors, stores, scenarios, runbooks) |

Markdown is the default doc format. New runbooks go under `docs/runbooks/`. Do not add root-level docs unless instructed.

## Conventions not enforced by lint

Treat these as hard rules — they are grep-able conventions, not machine-checked.

- Adapter files end with a compile-time interface assertion: `var _ ports.Sender = (*Sender)(nil)`.
- Adapter constructors use functional options: `WithXxx(value)`.
- Domain context references go through `domain/shared` or are forbidden.

Everything else (naming, layering, plugin-config shape, ACL boundary, aggregate convention, timing rules, registry symmetry, gofmt / go vet / golangci-lint rules) is enforced by `make lint` and `make test`. Do not restate those rules in PR feedback — point at the failing checker.

## MUST: Never reference a planning document

Comments, test names, file names and docs MUST NOT carry review or task
identifiers — `HIGH-3`, `CRITICAL 1`, `LOW-6`, `FIX 3 (XCUT-A)`, `finding F2`,
`T14`, `chunk-07`, `round-2`, "see the design doc". Planning documents get
deleted; the reference outlives them and points a reader at nothing.

Reference only what is durable:

| Reference | Example |
|---|---|
| An ADR | `see ADR-0006 — DLQ redrive is at-most-once` |
| A canonical root doc + section | `ARCHITECTURE.md §2`, `DDD.md`, `PLUGIN.md`, `TESTS.md` |
| A live page under `docs/` | `docs/cluster/spec/cluster-config-rollout-protocol.md §7` |
| A `UBIQUITOUS.md` term | say `Subject`, `Address`, `plan`, `lease` — the glossary word, not a ticket |

Otherwise write the rule in plain English: what must hold, and why.

```
// ✗  covers the HIGH-4 rule:
// ✓  a clustered exclusive session may not also hold an HTTP direct-hold
//    binding — both claim the same delivery slot, so the validator rejects
//    the pair at load time.
```

Name test files and functions after the behaviour they pin, never after the
batch that produced them (`numeric_bounds_test.go`, not `prodready_c15_test.go`).

If a plan's decision is worth keeping, promote it to an ADR or a `docs/` page
**before** the plan is deleted, then point at that.

## How to know you're done

Two commands. That's it.

```bash
make lint   # every static check; writes one log per checker under reports/
make test   # unit tests + timing audits (production + tests)
```

Both green on your branch. Failures that pre-date your work are still your problem on the branch — fix or revert before declaring done.

For the full list of checkers, the report each writes, and the failure → fix recipe table, read `LINT.md`. Read `reports/<tool>.log` first when something is red.

Keep test output context-efficient: write full logs to `reports/`, report only command/status/count/duration for passing runs, and read failure sections only unless full output is required for diagnosis.

Convenience wrappers (same gates, different scopes):

```bash
make check       # build + lint + test
make check-all   # build + lint + test-integration (Docker-backed)
```

`make lint` ends with three advisory stages (module graph, duplicate scan, repeated literals). They never fail the build. Their reports (`reports/arch-graph.txt`, `reports/dupl.log`, `reports/goconst.log`) are review aids — see `LINT.md` for what each one prompts.

## Adding a new arch component

1. Place it in a real layer and role (Layer 1 domain / Layer 2 application / Layer 3 adapter / Layer 4 composition).
2. Name by role: `adapter_transport_<tech>`, `adapter_store_<provider>_<role>`, `adapter_config_<source>`.
3. Add precise `mayDependOn` and only imported SDKs in `canUse` in `.go-arch-lint.yml`.
4. Add a sentinel in `scripts/lint-arch-mapping-test.sh`.
5. `make lint` must stay green.

If `make lint` conflicts with the code, the code is wrong. Refactor by moving types inward, introducing a port, or pushing wiring to the composition root. Relax `.go-arch-lint.yml` only when a generic architectural concept is genuinely missing — and document the reason inline in the yaml.
