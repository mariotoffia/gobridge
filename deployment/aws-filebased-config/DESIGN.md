# AWS Deployment Profile — Generalization Design

## Overview

> Planning document. Deleted when [TASKS.md](./TASKS.md) is fully implemented.
> Durable content is promoted to [ARCHITECTURE.md](./ARCHITECTURE.md),
> [README.md](./README.md), [UBIQUITOUS.md](./UBIQUITOUS.md), the root docs and
> `docs/aws-deployment/` before deletion. Nothing in code, tests, or shipped
> docs may reference this file. The reference binary's implemented composition
> contract is in [PLUGIN.md](../../PLUGIN.md#binary-composition-build-tags).

## Scope — the four problems

The approved initialization direction supersedes the earlier seeder design.
Implementation is in progress. Public-module publication and optional profile
families remain future work; this document does not mark their gates complete.
The current implementation keeps `deployment/aws-filebased-config` paths.

1. **The profile is misnamed and file-bound.** A complete DynamoDB config
   source exists (`adapters/aws/config/dynamodb`) but no production
   composition root uses it; `lib/bootstrap` hardcodes the file source and
   `infra.BootstrapConfig` can only describe a file path.
2. **Binary support is fixed.** The profile binary always links
   aws+mqtt+native+http; nothing else can be added without forking.
   (The reference binary's compile-time selection contract is in
   [PLUGIN.md](../../PLUGIN.md#binary-composition-build-tags); this profile
   reuses its tag convention.)
3. **The CDK cannot produce the container image.** `Image` is a required
   prop; consumers must clone the repo and `docker build` from the root
   (forced by relative `replace` directives).
4. **External consumption is unsupported.** HEAD `cdk/go.mod` carries 22
   replaces + `v0.0.0` requires; the release manifest
   (`scripts/release/modules.json`) has no deployment entries;
   `RELEASE.md:44-49` declares `deployment/` never tagged. Tags
   `deployment/aws-filebased-config/{cdk,infra}/v0.3.4-v0.3.6` exist on
   origin but are orphaned (cut from an unmerged branch) and ~5 weeks behind
   the documented API.

## Current state (verified facts)

| Fact | Where |
|---|---|
| Config ports are source-agnostic | `ports/blueprint_loader.go:17,36,43` (`Loader`/`Watcher`/`Reloader`), `ports/blueprint_validation.go:84,111` (`ConfigStore`/`ConditionalConfigStore`); assembly unit `config.Layer{Name, Loader, Watcher}` (`config/manager.go:23`) |
| DynamoDB loader is Loader+Reloader with poll **and** streams watch, CAS `Save`, `EnsureTable` | `adapters/aws/config/dynamodb/loader.go:100,211,250,506,546` |
| DynamoDB loader is **not** a `ConfigStore` | missing `Validate`/`Merge`; missing `SaveIfVersion` (CAS machinery already inside `Save`) |
| File source hardcoded at three sites | `lib/bootstrap/startup.go:99` (loader), `:102` + `config.go:117-138` (poll watcher), `:211` (`cfgparser.FileStore`); layer name literal `"file"` at `:104` |
| Start-empty keyed on filesystem error | `lib/bootstrap/config.go:99` checks `os.ErrNotExist`; the DynamoDB loader returns `shared.ErrNotFound` — fatal today |
| Bootstrap config-source surface is two fields | `infra/bootstrap.go:142-143` (`config_file_path`, `poll_interval`); `Validate` (`:364`) requires `config_file_path` unconditionally |
| Single-writer stands in for CAS | `startup.go:319` `configSingleWriter() = NodeRole==control`; comment `:298-318` says a multi-writer deployment MUST wire `ports.ConditionalConfigStore` |
| Rollout/barrier already source-agnostic | `bridge/rollout_barrier.go:27-30`, `rollout_joiner.go:241` ("this node's own validated config-source load path"); fingerprints computed on the **parsed** config (`config_watch.go:150`) |
| HA topology can run without EFS | `gobridgedynamodbha/ha.go` omits EFS when config and data stores use DynamoDB |
| Image is consumer-supplied, no default | `internal/gobridgebase/base.go:136,430,514`; no `DockerImageAsset` anywhere in constructs |
| Earlier seeder image was published | Historical artifact remains on Docker Hub; the approved design removes its maintained runtime dependency and publication job |
| Docker build context must be repo root | root `Dockerfile:12-15,41` — because `lib/go.mod` uses relative replaces |
| Consumer go.mod bootstrap undocumented | zero `go get`/`go.mod` guidance in README quickstarts and `docs/scenarios/cdk/` |
| CDK is Go-only | no jsii packaging; TS/Python CDK apps cannot consume (stays a non-goal) |

## Decisions

### D1 — Rename the module tree to `deployment/aws`

`aws-filebased-config` describes a mechanism the profile is outgrowing. Rename
directory and module paths:

| Old | New |
|---|---|
| `deployment/aws-filebased-config/{infra,cdk,lib}` | `deployment/aws/{infra,cdk,lib}` |
| module `…/deployment/aws-filebased-config/<m>` | `…/deployment/aws/<m>` |
| binary `lib/cmd/gobridge-filebased` | `lib/cmd/gobridge-aws` |
| env `GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` / `…_FILE` | `GOBRIDGE_BOOTSTRAP_JSON` / `GOBRIDGE_BOOTSTRAP_FILE` |
| image name `gobridge-filebased:local` | `gobridge-aws:local` |

**Why now:** the module is not yet on the release train, the orphaned v0.3.x
tags carry no obligations, and the repo-wide invariant "No backward
compatibility — GoBridge has never been deployed" (`PROD_READY_PLAN.md`
Global invariants) makes this the last cheap moment. Renaming after
publication (Decision D7) would be a breaking module-path change forever.

Cascades (mechanical, grep-driven): `go.work`, root `Dockerfile`
(`ARG BINARY_MODULE`, `BINARY_PKG`), CI docker job, release-image targets,
`.go-arch-lint.yml` `deployment` component (already `deployment/**` — no
change), all docs. Old tags are left in place and ignored.

*Alternative — keep the path, reframe docs only:* zero churn now, permanent
misnomer and a `filebased` string in every consumer's go.mod. Rejected;
revisit only if the user vetoes the rename. **Every path below is written
against the new name; if D1 is vetoed, substitute the old path.**

### D2 — Bootstrap gains a config-source discriminator

The bootstrap file/env stays the anchor (static per task revision); it learns
*where the bridge config lives*. `infra/bootstrap.go` additions (stdlib-only,
preserving the zero-dep rule):

```go
// ConfigSource selects where the hot-reloadable bridge config lives.
const (
    ConfigSourceFile     = "file"     // default when empty
    ConfigSourceDynamoDB = "dynamodb"
)

type ConfigDynamoDBSettings struct {
    // TableName is stamped by the CDK facade (consumer-set only outside CDK).
    TableName string `json:"table_name"`
    // WatchMode: "poll" (default) or "streams".
    WatchMode string `json:"watch_mode,omitempty"`
    // StreamPollInterval: GetRecords cadence when WatchMode is "streams".
    StreamPollInterval string `json:"stream_poll_interval,omitempty"`
}

type BootstrapConfig struct {
    // …existing fields…
    ConfigSource   string                  `json:"config_source,omitempty"`
    ConfigDynamoDB *ConfigDynamoDBSettings `json:"config_dynamodb,omitempty"`
}
```

Validation matrix (in `Validate()`; `Normalized()` maps empty → `file`):

| `config_source` | Rules |
|---|---|
| `file` | `config_file_path` required (today's rule); `config_dynamodb` must be nil |
| `dynamodb` | `config_dynamodb.table_name` required; `config_file_path` must be empty; `bridge_id` required (already); topology `filesystem_replicated` rejected (workers boot from the shared filesystem by definition) |

`poll_interval` is reused per source: default stays `DefaultPollInterval`
(1s) for `file`; new `DefaultDynamoDBPollInterval = 30 * time.Second` for
`dynamodb` (matches the adapter default; one strongly consistent read per
interval). The field-reference table in `docs/aws-deployment/configuration.md`
is pinned by `lib/bootstrap/bootstrap_field_reference_test.go` and changes in
the same commit.

### D3 — `lib/bootstrap` selects the source behind one seam

New `lib/bootstrap/config_source.go`:

```go
type configSource struct {
    layer        config.Layer      // Loader + Watcher; Name = cfg.ConfigSource
    store        ports.ConfigStore // handed to httpapi
    singleWriter bool              // true only for file+control
}

func (a *App) newConfigSource(ctx context.Context) (configSource, error)
```

- **file branch:** today's three pieces move here verbatim
  (`optionalFileSource`, `newPollWatcher`, `cfgparser.FileStore`);
  `singleWriter = cfg.NodeRole == NodeRoleControl` (unchanged semantics).
- **dynamodb branch:** one `ddbconfig.NewLoader(a.dynamoDBClient,
  WithTableName, WithBridgeID(cfg.BridgeID), WithRegistry(a.pluginRegistry),
  WithPollInterval(cfg.EffectivePollInterval()), WithWatchMode, WithLogger,
  WithClock)` serves as Loader, Watcher, and (after D4) ConfigStore.
  `singleWriter = false` — the store is CAS-capable, which `httpapi`'s
  config transaction already treats as always safe
  (`httpapi/config_txn.go:473-490`). Dev convenience: when `DevMode` is set,
  call `EnsureTable` at startup; in production the table must pre-exist
  (CDK-owned, D5).
- **Missing config is idle, not an active empty runtime.** With valid bootstrap,
  start the control plane live but not ready. Use the existing source watcher
  for explicit present, missing, and error observations. A backend-table error
  is not document absence.
- **Initial startup only:** an optional `ports.Loader` supplies the initial
  logical config. Only control can use `ports.ConfigInitializer.CreateIfAbsent`
  after definitive document absence. First creation assigns version 1,
  ignores the source version, and rereads the winner. Existing invalid or
  versionless documents are never overwritten; CAS at zero cannot substitute
  for strict creation.
- **After activation:** confirmed absence stops intake, drains safely, releases
  resources, and returns standalone operation to idle for a later rebuild.
  Clustered deletion or uncertain teardown requires process exit and replacement,
  not same-process idle. Read errors retain last-success operation as degraded.
  The first-activation latch is not readiness and does not reset on idle;
  a fresh process may initialize an absent target again. No durable tombstone.
- **Admin creation:** authenticated `POST /api/v1/admin/config` accepts complete
  YAML or JSON through the registry-aware decoder and strict `CreateIfAbsent`.
  It reports existing-document conflict, committed-not-applied, and ambiguous
  commit outcomes after rereading the target; it never compensates with deletion.
- **Reference control startup:** explicit `-admin-addr` plus
  `GOBRIDGE_ADMIN_API_KEY` makes startup independent of the repository.
  `-monitor-addr`, `-http-tls-cert`, and `-http-tls-key` configure the process
  listeners; the monitor key may use `GOBRIDGE_MONITOR_API_KEY`.
  Legacy boot-file HTTP settings remain supported; `-start-empty` is deprecated.
- **Shared core API:** `config.Initialize(ctx, target, source, admit)` validates
  an existing target without reading the source, or creates an absent target
  and rereads the winner. Admission receives an isolated snapshot; mutable
  custom plugin configs require `ports.FreezableConfig`, while deeply immutable
  scalar value configs need not implement it.
- **Observation ownership:** `Manager.Observe` supports one authoritative
  `config.Layer` and rejects overlays; the old `Watch` API remains intact.
  The host calls `Manager.NotifyIdle` after completed quiescence, preserving any
  newer desired snapshot. Local-file tests do not establish EFS crash durability.
- `startup.go` shrinks to `src, err := a.newConfigSource(ctx)` + wiring
  `src.layer`, `src.store`, `src.singleWriter`.

The single-layer rule stays: this profile runs exactly one `config.Layer`
(base, no overlays) — the admin config API and the rollout candidate digest
both require a single writer identity (`docs/configuration-overview.md`
"Overlays and the admin config API do not compose").

### D4 — Complete the DynamoDB adapter as a config store

Add to `adapters/aws/config/dynamodb` (arch-lint envelope already permits the
`config` import):

```go
func (l *Loader) Validate(ctx context.Context, cfg *ports.BridgeConfig) ([]string, error) // config.ValidateWithWarnings
func (l *Loader) Merge(ctx context.Context, base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) // config.DefaultMerge
func (l *Loader) SaveIfVersion(ctx context.Context, cfg *ports.BridgeConfig, expectedVersion int) error // conditional put, ErrVersionMismatch on conflict

var (
    _ ports.ConfigStore            = (*Loader)(nil)
    _ ports.ConditionalConfigStore = (*Loader)(nil)
)
```

A new conformance suite `ports/configstoretest` (mirroring `ports/storetest`)
pins the ConfigStore/ConditionalConfigStore contract once and runs against
both `parser.FileStore` (ConfigStore subset) and the DynamoDB loader
(ddblocal-backed). Out of scope: `FileStore.SaveIfVersion` — the file path
keeps the single-writer guard (tracked separately as the filestore CAS gap).

### D5 — CDK: DynamoDB-sourced config, EFS becomes conditional

The facade reads the consumer's `Bootstrap.ConfigSource` (no new prop):

- **`config_source: dynamodb`** → the facade provisions one config table
  (on-demand, PITR, retained — same posture as `DynamoDBHAData` tables),
  stamps `Bootstrap.ConfigDynamoDB.TableName` (same overwrite pattern as
  `ContainerMemoryBytes`). Initial document creation belongs to the process.
- **EFS is provisioned only when something needs a filesystem**: config
  source is `file`, or the parsed yaml declares sqlite store paths (tier B
  already walks the yaml). `GoBridgeDynamoDBHA` with `dynamodb` config source
  and DynamoDB stores therefore deploys **EFS-free** — the topology's only
  remaining EFS use is the config yaml.
- **Topology × source matrix:** Single: both sources. DynamoDBHA: both
  (default stays `file` in v1; flip to `dynamodb` only after a deployed
  rollout proof). Cluster (`filesystem_replicated`): `file` only, enforced at
  synth (and by D2's `Validate`).
- **No seeder container or S3 config asset:** the facade's parsed config is
  embedded by the Go-build image source. Registry/ECR images are unchanged.
  Only the control process can create an absent document; worker runtime
  wiring remains read-only, independently of its data-store permissions.
- **Grants** (`internal/grants/configsource.go`): control role
  `table.GrantReadWriteData`; worker role `table.GrantReadData`; both roles
  `table.GrantStreamRead` when `watch_mode: streams` (facade enables the
  stream on the table only then). No config-download grant.
- The HA admission fingerprint check
  (`lib/bootstrap/config.go:268,303`) is already computed on the parsed
  config — only its prose ("EFS-loaded") changes.

### D6 — Sealed image source; the CDK can build the image

Mirror of `BridgeConfigSource`, in `cdk/internal/imgsource` re-exported by
`gobridgecdk`:

```go
type BridgeImageSource = imgsource.Source // sealed

func ImageFromRegistry(ref string) BridgeImageSource                       // digest-pinned ref
func ImageFromEcrRepository(repo awsecr.IRepository, tag string) BridgeImageSource
func ImageFromGoBuild(props ImageGoBuildProps) BridgeImageSource

type ImageGoBuildProps struct {
    Version   string   // REQUIRED: published lib-module tag, e.g. "v0.4.0"
    Package   string   // default "github.com/mariotoffia/gobridge/deployment/aws/lib/cmd/gobridge-aws"
    BuildTags []string // nil → facade injects DeriveBuildTags(parsed config)
    GoImage   string   // digest-pinned golang builder default
    BaseImage string   // digest-pinned distroless static:nonroot default
    Platform  string   // default "linux/amd64"
}

func DeriveBuildTags(cfg *ports.BridgeConfig) []string // config kinds → family tags
```

`ImageFromGoBuild.Materialize` renders an embedded two-stage Dockerfile
template (`go:embed`) into a temp build context and returns
`ContainerImage_FromDockerImageAsset`. The builder stage runs
`go install -trimpath -tags=<tags> <Package>@<Version>`
— **no repo checkout**, which is exactly what D7's replace-free publication
makes possible. Facades change `Image awsecs.ContainerImage` →
`Image gobridgecdk.BridgeImageSource` (breaking prop change; no compat
needed). `DeriveBuildTags` maps kinds beyond the profile base
(aws+mqtt+native+http) to family tags: `amqp091→gobridge_amqp091`,
`amqp10→gobridge_amqp10`, `servicebus→gobridge_azure`; unknown kinds are a
synth error (they would fail at runtime anyway).

**Embedded initialization replaces seeder publication.** Both command entry
points define and decode `main.initialConfigBase64`. CDK automatically embeds
the parsed `BridgeConfig` already passed to the facade, without another prop.
It stages `initial-config-<hash>.goenv`; native `GOENV` file flags avoid
operating-system argument limits. The Go command does not accept
`@responsefile` syntax.

The root build supports `make build-gobridge INITIAL_CONFIG_FILE=...`;
Docker supports `--build-arg INITIAL_CONFIG_FILE=<file in context>` and
`make docker-build INITIAL_CONFIG_FILE=...`. Input may be YAML or JSON.
Literal credentials and `pms://` references are both allowed. Document artifact
visibility; Base64 is not secrecy. Never resolve references into the stored
initial logical copy.

Remove mandatory config sidecars, init containers, scripts, image pins,
publication jobs, drift-mode props, `ConfigAsset`, and S3 download grants.
The previously published Docker Hub artifact remains historical, not deleted.
Managed-subscription baselines and HA generation-zero baselines remain intact.

Registry/ECR images cannot be changed by CDK. Consumers own their image build;
images may contain their own initial document, use an existing target, or wait
for operator creation. `BridgeConfig` still declares validation and IAM, not
automatic overwrite. Public `lib` publication and optional family wiring remain
prerequisites for the versioned build path.

**Embedded SQS references:** use a stable physical `queue_name`, or optional
`queue_tags` with `queue_name_prefix`; never unresolved queue URL tokens.
The existing name path uses `GetQueueUrl`. Tag mode uses native SQS SDK
`ListQueues` pagination and `ListQueueTags`, scoped to account and region:
one match succeeds, none retries without readiness, multiple matches error,
and permission failures are errors rather than no-match results. Retain
`IQueue` handles for exact grants and dependencies; add metadata reads only
for tag mode. No generic token resolver or default two-phase deploy.

The completed CDK API is `QueueRegistry.BindQueueTags(name, tags, prefix)` and
`ResolveQueue`; `QueueRef.PhysicalName`, `QueueTags`, and `QueueNamePrefix`
keep aliases, physical names, and selectors distinct. Tag-selected bindings use
`sqs.QueueAddress` (`sqs:queue`), never a resolved target URL.
The shared base calls `GrantSQSConfig`; the image builder calls
`ValidateEmbeddedSQSConfig` and rejects every embedded queue URL, including
literals. `ScanForPlaintextSecrets` remains explicit, not a default builder or
Phase 1 check.

An S3 config adapter is deferred. No SDK dependency is added to core.

**Identity proof:** both commands expose `-initial-config-digest` before runtime
or network startup. It reports only SHA-256 of the decoded embedded bytes.
The CDK build executes `/gobridge-filebased -initial-config-digest` and compares
the result with the staged bytes. Missing support or a mismatched digest fails
the build, including older custom packages that ignore the linker stamp.

**HA baseline recognition:** both file and DynamoDB sources use
`DeploymentBaselineContentDigest`, normalizing only the top-level version.
Initialization independently assigns target version 1. The actual committed
artifact retains its stored version and full `ConfigArtifactDigest`.

### D7 — Publish the three modules for external consumers

- `scripts/release/modules.json`: add `deployment/aws/infra` (layer 1 — zero
  deps), `deployment/aws/lib` (layer 3), `deployment/aws/cdk` (layer 3); add
  `testutil/testcontent` to `bootstrap_modules` (cdk test dependency needs a
  pseudo-version per release policy 6).
- Strip all replaces from `cdk/go.mod` and `lib/go.mod` at release time via
  the existing `stage-published-module` machinery; requires pinned to the
  train version (release gates already reject replaces in published modules).
- `RELEASE.md:44-49` amended: `deployment/` is internal-only **except** the
  three `deployment/aws/*` modules; `MODULES.md` + `DEVELOPMENT.md:236`
  ("Current state" note) updated.
- README gains "Consuming from your own CDK app": the exact consumer
  `go.mod` (`go get github.com/mariotoffia/gobridge/deployment/aws/cdk@vX.Y.Z`
  + `…/infra@vX.Y.Z`), and `docs/scenarios/cdk/01-quickstart-default-vpc.md`
  drops the repo-clone assumption in favor of `ImageFromGoBuild`.
- `smoke-released-modules` extended to build a scratch consumer module
  importing `gobridgecdk` + `gobridgesingle` with `GOWORK=off`.
- Orphaned `…/aws-filebased-config/{cdk,infra}/v0.3.x` tags: left in place,
  documented as dead in RELEASE.md. Never moved (policy 7).

### D8 — Profile binary gains the optional families

Reuses the tag convention from
[PLUGIN.md](../../PLUGIN.md#binary-composition-build-tags) (same tags, one
convention repo-wide). The profile **base** is aws+mqtt+native+http (that is
what "AWS profile" means); additive: `gobridge_amqp091`, `gobridge_amqp10`,
`gobridge_azure`. Mechanics: stub pairs in `lib/bootstrap`
(`plugins_amqp091.go` + stub, …) that extend the decoder registry
(`config.go:188-199`) and the factory maps (`registry.go`) before the loops.
This root is guarded by `registry_wiring_test.go` (white-box), not pluginsym
— extend that test per family. Root `Dockerfile` gains
`ARG GO_BUILD_TAGS=""`; `ImageFromGoBuild.BuildTags`/`DeriveBuildTags` (D6)
feed it. OTel stays out of the profile (metrics selection is runtime
`MetricsExporter: cloudwatch|noop`).

## Alternatives considered

| Decision | Alternative | Why rejected |
|---|---|---|
| D2/D3 | New `App` option `WithConfigLayer(layer, store)` instead of a bootstrap field | Moves source selection out of the deployment contract into code the CDK cannot see; CDK must stamp table names + derive IAM from the same declaration. Kept as a test seam only. |
| D5 | Separate config seeder container or Lambda | Adds a runtime artifact and startup dependency; strict creation belongs behind the target port in the control process. |
| D5 | Config table owned by consumer (BYO) | The facade owns the config table and grants; importing a table can be a separate future API. |
| D6 | Generic CloudFormation token resolver | Stable physical names and native SQS tag discovery cover embedded queue references without a second deployment phase. |
| D6 | `SaveIfVersion(..., 0)` for initialization | It can adopt a legacy versionless row; strict creation must not overwrite any existing document. |
| D6 | Publish a ready image per release and default `Image` to it | The published image has one plugin set; `ImageFromGoBuild` + tags covers minimal *and* extended binaries with the same construct. Registry pinning stays available via `ImageFromRegistry`. |
| D6 | `DockerImageAsset` over a repo checkout path prop | Reintroduces the clone requirement for external consumers. Local checkout builds remain the root `make docker-build` flow. |
| D7 | Keep modules internal; tell consumers to vendor + replace | That is today's undocumented reality and the main external-reuse blocker. |
| D1 | `deployment/awsprofile` or `deployment/aws-config` | `aws` is the shortest name that stays true as the profile grows; sibling profiles (`deployment/kubernetes`) already use the platform name. |

## Open questions (flagged for the user; recommendations already applied)

1. **D1 rename** — approve `deployment/aws` + binary `gobridge-aws` + env
   `GOBRIDGE_BOOTSTRAP_*`? (Recommended: yes, before any other chunk.)
2. **HA default config source** — stays `file` in v1 (recommended); flip to
   `dynamodb` in a later release after a deployed rollout proof?
3. **Streams watch mode in v1** — designed in (D2/D5 grants) but `poll` is
   the default; ship streams support in v1 or defer the grants/stream
   enablement to a follow-up? (Recommended: ship it — the adapter side
   already exists and is tested.)

## Acceptance

- A bootstrap with `config_source: dynamodb` boots, hot-reloads on table
  writes, serves admin config transactions with CAS semantics, and never
  touches the filesystem for config.
- `GoBridgeDynamoDBHA` + `config_source: dynamodb` + DynamoDB stores synths
  with no EFS resources, no config asset, and no seeder dependency.
- An external Go CDK app with only `go get`-fetched modules synths a
  `GoBridgeSingle` whose image comes from `ImageFromGoBuild` — no repo clone.
- Go-built images embed the parsed config and initialize only absent targets.
  Registry images remain unchanged. Existing invalid targets are protected.
- Deletion, recreation, read errors, creation races, standby activation, and
  process restart obey the lifecycle contract without a second watcher service.
- Name-based and tag-based SQS discovery work with precise grants.
- `make lint` + `make test` green throughout; `make check-all` for the
  Docker-backed integration suites.

## Glossary additions (promoted to UBIQUITOUS.md files on completion)

| Term | Meaning |
|---|---|
| **Config source** | The bootstrap-selected backend for the hot-reloadable bridge config: `file` (EFS yaml) or `dynamodb` (single `current` item, CAS-versioned). |
| **BridgeImageSource** | Sealed CDK type for the bridge container image: `ImageFromRegistry`, `ImageFromEcrRepository`, `ImageFromGoBuild`. |
| **ConfigInitializer** | Strict create-if-absent target capability; first target version is 1 and existing documents remain untouched. |
| **ConfigObserver** | Existing-source observation capability distinguishing present, missing, and read-error outcomes. |
| **Inline initial source** | Logical document exposed through `ports.Loader`, including the decoded embedded document. |
| **Profile base set** | Plugin families always linked into the profile binary: aws, mqtt, native stores, http. |

Append current definitions and explicit supersession notes to both glossaries.
Retain existing glossary rows and historical ADRs.
