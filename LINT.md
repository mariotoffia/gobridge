# GoBridge Lint

Single entry point. `make lint` runs every static check on the workspace, writes one report per checker under `reports/`, and fails fast on the first blocking violation. Advisory stages run last and never fail the build.

`make test` is the test-side gate (unit tests plus the production and test timing audits).

```bash
make lint   # all static checks + advisory reports
make test   # unit tests + timing audits
```

Wrappers: `make check` = `build + lint + test`. `make check-all` = `build + lint + test-integration` (Docker-backed).

## What `make lint` runs

Stages execute in order. The first blocking failure stops the build; advisory stages run only if every blocking stage passes.

| # | Stage | Tool | Blocking | Catches |
|---|---|---|---|---|
| 1 | Architecture | `go-arch-lint check` + `graph` + `scripts/lint-arch-mapping-test.sh` | yes | Outward dependency edges, component vendor-opt-in violations, sentinel-package drift. |
| 2 | Format | `gofmt -l` | yes | Files not run through `gofmt`. Auto-fix: `make lint-fix`. |
| 3 | Vet | `go vet` per workspace module | yes | Stdlib correctness issues (printf, shadow, unreachable, …). |
| 4 | Lint | `golangci-lint run` per workspace module | yes | Full ruleset from `.golangci.yml` (`depguard`, `forbidigo`, `wrapcheck`, `interfacebloat`, `gochecknoglobals`, `gochecknoinits`, …). |
| 5 | Aggregate | `aggcheck` on `domain/` | yes | Aggregate-shaped types not in `*_aggregate.go` or missing `Validate()`. Pure value objects / read-only snapshots (e.g. `shared.Secret`, `LeaseInfo`) carry a `// value-object` marker to opt out of the heuristic track; aggregate roots carry `// aggregate-root` to opt into the strict no-exported-mutable-state / guarded-transition checks. |
| 6 | ACL | `aclcheck` per adapter module | yes | Vendor SDK imports outside `acl_*.go` or `acl/`, **and** export-confinement: SDK-originated types must not appear in exported signatures (type-origin check, not just import location). |
| 7 | Config shape | `cfgshape` per workspace module | yes | Plugin config decoded as `map[string]any` instead of typed `ports.PluginConfig`. Enforces the non-empty-`Validate()`-body rule only; the Validate test-reference check is intentionally **not** enforced. |
| 8 | Registry coverage | `registrychk` | yes | AWS-deployable kind missing CDK `With<Kind>*` builder or grants helper. |
| 9 | Registry symmetry | `pluginsym` | yes | Decoder ↔ wired factory mismatch in `cmd/gobridge/main.go`. |
| 10 | Module graph | `go mod graph` → `reports/arch-graph.txt` | no | Workspace module-level edges. Diff across PRs for new vendor deps. |
| 11 | Duplicate scan | `dupl -threshold 75` → `reports/dupl.log` | no | Repeated logic blocks ≥75 tokens. Prompt for a missing aggregate, value object, or domain service. |
| 12 | Repeated literals | `goconst -min-occurrences 4 -min-length 5` → `reports/goconst.log` | no | Strings or numbers repeated ≥4 times. Prompt for a missing domain constant. |

The five custom analyzers (`aggcheck`, `aclcheck`, `cfgshape`, `registrychk`, `pluginsym`) build automatically as `lint` prerequisites; no separate `make build-*` invocation is needed.

## Reading the output

Two channels carry the same information at different granularity. Use both.

### Stdout — live signal

Each stage prints `=== <stage name> ===` before its tool output. Per-module loops add `--- <module> ---` headers so the failing module is locatable without re-running. The last `===` line on a failed run names the failing stage.

Use stdout to identify which stage failed, see the immediate error, and decide whether to fix from the message alone or open the full report.

### `reports/<tool>.log` — post-mortem

Every stage writes its full output to a per-tool file under `reports/`. The file persists across runs. On the next `make lint`, only the stages that actually executed rewrite their logs; later-stage logs after a mid-run failure are stale from the previous successful run. Always check the modification time before trusting a downstream report.

Use the reports to read full output without scrolling stdout, to diff across branches and runs for regressions, to share with reviewers, and to download from CI as a workflow artifact (uploaded on every run by `.github/workflows/ci.yml`).

### Auxiliary artifacts

- `reports/go-arch-lint-graph.svg` — component-level domain-architecture diagram. Open in a browser. Regenerated on every `make lint`.
- `reports/arch-graph.txt` — module-level `go mod graph` dump (one edge per line). Use `grep '^github.com/mariotoffia/gobridge ' reports/arch-graph.txt` for direct deps. Diff across branches to spot a new vendor edge.
- `reports/dupl.log`, `reports/goconst.log` — advisory only. Not every entry needs a fix. Read at release cuts or when investigating a smell that blocking lint cannot pinpoint. See the prompts in the stage table.

## Report inventory

| Report | Source | Blocking |
|---|---|---|
| `reports/go-arch-lint.log` | `go-arch-lint check` | yes |
| `reports/go-arch-lint-graph.svg` | `go-arch-lint graph` | yes (graph regenerated even if check passes) |
| `reports/arch-mapping.log` | `scripts/lint-arch-mapping-test.sh` | yes |
| `reports/gofmt.log` | `gofmt -l` | yes |
| `reports/go-vet.log` | `go vet` per module | yes |
| `reports/golangci.log` | `golangci-lint run` per module | yes |
| `reports/aggcheck.log` | `aggcheck` analyzer | yes |
| `reports/aclcheck.log` | `aclcheck` analyzer | yes |
| `reports/cfgshape.log` | `cfgshape` analyzer | yes |
| `reports/registrychk.log` | `registrychk` tool | yes |
| `reports/pluginsym.log` | `pluginsym` tool | yes |
| `reports/arch-graph.txt` | `go mod graph` | no |
| `reports/dupl.log` | `dupl` | no |
| `reports/goconst.log` | `goconst` | no |

## Failure → fix recipes

Locate the offending file:line in the named report. Apply the fix.

| Failing check | Report | Fix |
|---|---|---|
| `go-arch-lint` outward edge | `reports/go-arch-lint.log` | Move the type inward, introduce a port, or wire at the composition root. |
| `arch-mapping` sentinel drift | `reports/arch-mapping.log` | Restore the mapping. If the rename is deliberate, update `scripts/lint-arch-mapping-test.sh`. |
| `depguard` | `reports/golangci.log` | Same as `go-arch-lint` — `depguard` is the per-file mirror. |
| `interfacebloat` | `reports/golangci.log` | Split the port interface; plugins implement the subset they need. |
| `forbidigo time.Now` | `reports/golangci.log` | Inject `clock.Clock`; call `clk.Now()`. |
| `forbidigo os.Getenv` | `reports/golangci.log` | Add a config DTO; read env at the composition root only. |
| `gochecknoglobals` / `gochecknoinits` in `domain/` | `reports/golangci.log` | Remove the global or init; pass the value through. |
| `wrapcheck` | `reports/golangci.log` | Wrap with `%w` or a domain error wrapper. |
| `aclcheck` | `reports/aclcheck.log` | Move the SDK-touching code into an `acl_*.go` file. Also fires on export-confinement: an SDK-originated type in an exported signature — return/accept a domain type instead. |
| `aggcheck` | `reports/aggcheck.log` | Move the type into `*_aggregate.go` and add `Validate() error`. |
| `cfgshape` | `reports/cfgshape.log` | Define a typed `ports.PluginConfig`, register the decoder in `register.go`, type-assert at the adapter boundary. |
| `registrychk` | `reports/registrychk.log` | Add the `With<Kind>*` builder under `deployment/aws-filebased-config/cdk/bridgecfg/` and `cdk/constructs/internal/grants/<kind>.go`. |
| `pluginsym` | `reports/pluginsym.log` | Wire the missing side (decoder or factory) in `cmd/gobridge/main.go`. |
| `gofmt` | `reports/gofmt.log` | Run `make lint-fix`. |
| `go vet` | `reports/go-vet.log` | Fix the reported issue at file:line. |
| `audit-timings` (in `make test`) | stdout + `audit/timing-allowlist.txt` | Replace `time.Sleep` / `time.After` / `NewTimer` / `NewTicker` / `Tick` with an injected `Clock` and `select { case <-ctx.Done(): case <-clk.After(d): }`. |
| `audit-test-timings` (in `make test`) | stdout + `audit/test-timing-allowlist.txt` | Replace `time.Sleep` with `require.Eventually`, a started-signal channel, or `clocktest.FakeClock`. |

## Escape hatch

```bash
make lint-fix   # gofmt -w on every tracked Go file
```

Only auto-fix. Every other failure requires a code change driven by the table above.

## Sanctioned architecture exceptions

A few inward-ring rules carry deliberate, reviewed exceptions. They are recorded here and inline at the source so a reviewer never mistakes one for drift.

- **`domain/messaging` owns the Envelope JSON schema (M-10).** `domain/messaging/envelope.go` imports `encoding/json` and defines the stable on-disk `MarshalJSON` / `UnmarshalJSON` for `Envelope`. This is the single source of truth for the durable wire format every store adapter (`sqlitedlq`, `dynamodbdlq`, `sqliteoutbox`, `dynamodboutbox`, …) serialises through. Keeping one domain-owned marshaller is intentional: scattering the schema across adapters would invite silent cross-backend drift. The `encoding/json` edge is therefore an accepted exception to the otherwise stdlib-only inner ring, mirrored by a comment in `.go-arch-lint.yml` next to the inner-ring no-json note. go-arch-lint cannot deny stdlib imports, so there is nothing to suppress — this entry is the audit trail.

## Authoritative sources

- `.golangci.yml` — golangci-lint ruleset.
- `.go-arch-lint.yml` — component dependency map; single source of truth for architecture rules.
- `scripts/<analyzer>/` — implementation of each custom analyzer (`aclcheck`, `aggcheck`, `cfgshape`, `registrychk`, `pluginsym`).
- `scripts/lint-arch-mapping-test.sh` — sentinel mapping regression.
- `.github/workflows/ci.yml` — CI invocation and `reports/` artifact upload.
