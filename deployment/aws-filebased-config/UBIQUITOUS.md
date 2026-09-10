# aws-filebased-config — Ubiquitous Language

## Overview

Terms specific to this deployment profile. Additive to the project-wide [UBIQUITOUS.md](../../UBIQUITOUS.md). If a term appears both here and in the project glossary, the project glossary wins.

## Configuration

There are exactly **two** configuration artifacts. The `bootstrap` package tracks two *states* of the bridge config (`logicalRef`, `appliedRef`) which only differ when a reload was rejected.

| Term | Meaning |
|---|---|
| **Bootstrap config** | Deployment-owned runtime parameters (`infra.BootstrapConfig`). Static per task revision. Delivered via env var `GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` or file `GOBRIDGE_FILEBASED_BOOTSTRAP_FILE`. Distinct from `ports.BridgeConfig`. |
| **Bridge config** | The application's `ports.BridgeConfig` (YAML on EFS or JSON in a DynamoDB config item). Hot-reloadable. The same artifact whether in the selected source, in `logicalRef`, or in `appliedRef`. |
| **Config table** | Facade-owned DynamoDB table for the `dynamodb` config source: string `PK`/`SK`, one item at `config#<bridge_id>` / `current`, on-demand billing, PITR and retention. Shared by every HA task definition and separate from the HA data and rollout tables. CDK overwrites `ConfigDynamoDB.TableName` in an owned settings copy. |
| **Config source** | Bootstrap selector `ConfigSource`: `file` (default) or `dynamodb`. Each supplies one base `config.Layer` and the matching admin `ports.ConfigStore`; the DynamoDB loader also implements `ports.ConditionalConfigStore`. |
| **Logical state** | The bridge config last *seen* in the selected config source and parsed successfully (`logicalRef`). Updated even when the subsequent runtime swap is rejected. |
| **Applied state** | The bridge config the *currently running* runtime was built from (`appliedRef`). On the happy path equals logical state. Diverges only after a failed reload — then logical = rejected new config, applied = last good. Used by `Stop` for `DrainTimeout` and by `recoverPrevious`. |
| **ContainerMemoryBytes** | Bootstrap container hard limit. Defaults to 1 GiB outside CDK; the CDK base always overwrites it from the effective Fargate task `MemoryMiB`, preventing runtime accounting from diverging from the deployed limit. |
| **ReservedMemoryBytes** | Bootstrap reservation for non-MQTT runtime memory. Together with the AWS MQTT memory profile's 25% ingress reservation, it must leave at least 20% of `ContainerMemoryBytes` as headroom. |
| **AWS MQTT memory profile** | Runtime bootstrap policy applied to initial config and every reload: divide 25% of container memory across unique built MQTT sessions that can ingest and derive each default Receive Maximum with the Paho ingress byte model. Every Persistent/Exclusive session referenced by a declared sender consumes one deduplicated share with route concurrency zero even when no route references that sender, because resumed durable state may deliver stale backlog; an Ephemeral sender-only session consumes no share. |
| **ConfigInitializer** | Optional strict target-creation capability; uses the [project definition](../../UBIQUITOUS.md#blueprint--configuration-portsblueprintgo--config), creates version 1 only when the document is absent, and is wired only for control. |
| **ConfigObserver** | Source capability reporting ordered present, missing, and read-error outcomes through the existing watch path; see the [project glossary](../../UBIQUITOUS.md#blueprint--configuration-portsblueprintgo--config). |
| **ConfigObservation** | One source result categorized by `ConfigObservationKind`, carrying the config or error. |
| **ConfigObservationKind** | `ConfigPresent` (document observed), `ConfigMissing` (definitive document absence), or `ConfigReadError` (read failed); backend-table absence is a read error. |
| **Inline initial source** | Logical YAML or JSON exposed through `ports.Loader`, including an embedded document decoded by the entry point, for optional initial-start creation. |
| **First activation latch** | Process-lifetime activation record, independent of readiness; an HA standby can activate without Full readiness, and idle does not reset the latch. |
| **ConfigPresent** | Logical document observed through the selected source; normal validation and rollout rules still govern activation. |
| **ConfigMissing** | Definitive document absence; after activation it requires safe idle or instance termination, never stale processing. |
| **ConfigReadError** | Source read failure; before activation remain idle with the error, after activation retain last successful config as degraded. |
| **ConfigObservation.Sequence** | Observation order independent of the stored version; a recreated version-1 document must not disappear behind an earlier higher version. |
| **Initialize** | Shared `config.Initialize` orchestration for strict creation and winner admission, following the [project contract](../../UBIQUITOUS.md#blueprint--configuration-portsblueprintgo--config); control authorization and process lifecycle remain profile responsibilities. |
| **Manager.Observe** | Observation path for the profile's single authoritative `config.Layer`; overlays are unsupported and the existing `Watch` API remains available. |
| **Manager.NotifyIdle** | Host acknowledgement after data-plane quiescence that clears confirmed running config state without removing a newer desired snapshot or rearming initialization. |
| **Initialization snapshot** | Admission copy preserving logical references; mutable custom plugins must implement existing `ports.FreezableConfig`, while deeply immutable scalar value configs need not. |
| **Clustered config absence** | Confirmed absence after clustered activation signals process exit and replacement; only safely quiesced standalone operation can return to idle in the same process. |
| **Initial config creation** | Authenticated control-only `POST /api/v1/admin/config`, using a complete typed YAML or JSON document and strict `CreateIfAbsent`; see [request and outcomes](../../docs/aws-deployment/config-initialization.md#operator-creation-and-rollout). |

> **Not a layer:** the `resolvedCfg` produced inside `resolveInputs` is a transient working copy with SSM secrets patched into HTTP `Config.APIKey`. It is consumed by the builder and discarded — no ref, no name to learn.

## Topology & Roles

| Term | Meaning |
|---|---|
| **Topology** | Deployment shape. `single` = one replica owns config writes. `filesystem_replicated` = N independent replicas read the same EFS and cross-instance coordination features are rejected. `dynamodb_coordinated_ha` (`TopologyDynamoDBCoordinatedHA`) = coordinated active/warm-standby ECS tasks using DynamoDB lease, shared outbox, and managed-subscription history stores. |
| **NodeRole** | Per-replica identity. `control` (default) or `worker`. Declared in bootstrap; only control asserts single-writer authority for a file ConfigStore. A DynamoDB config store uses CAS regardless of role. |
| **Filesystem profile guard** | `validateFilesystemProfile` — rejects `shared_outbox` and `route.session` when topology is `filesystem_replicated`. |
| **GoBridgeDynamoDBHA** | Separate coordinated active/warm-standby CDK facade for topology `dynamodb_coordinated_ha`; it reuses `internal/gobridgebase.New`, provisions one control task and at least two worker tasks, and does not change `GoBridgeCluster`. |
| **DynamoDBHAProps** | Input contract for `GoBridgeDynamoDBHA`, including the shared bridge config, ECS/VPC placement, and registries. |
| **DynamoDB HA config expectation** | Deployment-owned exact lease/outbox/managed-subscription table names plus canonical bridge-config SHA-256 fingerprint stamped into bootstrap by `GoBridgeDynamoDBHA`; every HA process checks the selected-source logical config against it before planning stores or transports. |
| **DynamoDBHAData** | Data output owned by `GoBridgeDynamoDBHA`; the sole profile API exposing the lease, shared-outbox, and managed-subscription table objects, names, and ARNs. |
| **FailureToFullDuration** | External failover-probe CloudWatch metric in the deployment metrics namespace. One sample is the milliseconds from the conservative pre-`StopTask` timestamp through exact-holder `STOPPED`, owner plus fencing-version change, and the different successor reaching `ServiceLevelFull`. It has no runtime dimensions; warm/cold percentiles are reported separately by the credentialed harness. Missing samples are non-breaching for the alarm, while release proof must query and find its exact sample. |
| **NodeRole (current configuration authority)** | Supersedes the earlier write-authority description: workers remain read-only for file and DynamoDB config; CAS capability does not authorize worker writes or initialization. |
| **Deployment baseline content digest** | Content-only HA baseline identity for both file and DynamoDB sources, using the [project definition](../../UBIQUITOUS.md#deployment--seeding-deploymentaws-filebased-config); committed artifacts retain their actual stored version and full digest. |

## Reload Mechanics

| Term | Meaning |
|---|---|
| **Swap mode** | Strategy used by `applyLogicalConfig` to replace the running runtime. |
| **Overlap swap** (`swapModeOverlap`) | Build + Start new runtime, install, then Stop old. Default. |
| **Prepare/commit swap** (`swapModePrepareCommit`) | Used when any transport advertises `ports.CapExclusiveIdentity` (e.g. exclusive MQTT client ID). Stop old → `Complete` → Start new → install. |
| **Runtime plan** | `runtimePlan` struct: bundles logical + resolved configs, swap mode, registry, and either a `bridge.BuildPlan` or a built `*runtime.Runtime`. |
| **Recover previous** | `recoverPrevious(ctx, oldApplied)` — best-effort rebuild from last-good applied config when a prepare/commit swap fails mid-flight. |

## Secrets & Parameters

| Term | Meaning |
|---|---|
| **Parameter reference** | A bootstrap field value identifying an SSM parameter. Either `pms://name/path` (authority form), `pms:///name/path` (absolute-path form), or an absolute SSM name (`/foo/bar`). Normalized by `normalizeParameterRef`. |
| **Parameter resolver** | The `parameterResolver` interface used by `resolveInputs`. Default is SSM-backed; tests inject custom implementations via `WithParameterResolver`. |
| **DevMode** | Bootstrap flag that authorizes use of `SSMEndpoint` overrides (e.g. LocalStack) and startup creation of the selected DynamoDB config table. Production safety guard: `SSMEndpoint` without `DevMode` fails `Validate()`. |
| **Admin/Monitor key param** | SSM references for the admin (required) and monitor (optional) HTTP API `X-API-Key` values. Re-resolved on every reload. |

## Paths

| Term | Meaning |
|---|---|
| **Access point path** | POSIX path *inside* EFS exposed by the access point. Default `/gobridge`. Set on the access point at creation; immutable thereafter. |
| **Config mount path** | Path *inside the container* where the EFS access point is mounted when the config source or SQLite store paths require EFS. Default `/var/lib/gobridge` (single canonical constant `infra.DefaultMountPath`; the Phase-1 store-path validator, the ECS mount, and the seeder all derive from it). |
| **Config file path** | Absolute path the bootstrap polls for the bridge config. Combines mount path + filename, e.g. `/var/lib/gobridge/bridge.yaml`. |
| **Config mount path (current users)** | The runtime config path and SQLite store paths derive from the mount; the earlier seeder reference is historical, since initialization now runs in the control process. |

## CDK

The original **Embedded initial config** row describes the superseded
linker/Go-environment-file build path. The native-file definition in this table
is current; earlier rows remain as glossary history.

| Term | Meaning |
|---|---|
| **L2 construct** | `GoBridgeSingle`, `GoBridgeCluster`, `GoBridgeDynamoDBHA`, `GoBridgeAlarms`. Composable; consumers wire their own VPC / cluster / ALB. There is no L3 stack — see [ARCHITECTURE.md](ARCHITECTURE.md). |
| **Exposure** | `infra.Exposure` flags (`Admin`, `Monitor`, `TransportHTTP`) selecting which container ports get mapped. Admin :8080 is always mapped (health check requirement) regardless of `Admin`. |
| **BridgeConfigSource** | Sealed type representing the source of bridge YAML supplied to a `GoBridgeSingle`, `GoBridgeCluster`, or `GoBridgeDynamoDBHA`. Two constructors: `BridgeYamlAsset(path)` (file → S3 asset) and `BridgeYamlInline(*ports.BridgeConfig)` (in-memory builder output). Construct unwraps internally. Lives in `cdk/gobridgecdk/`. |
| **BridgeImageSource** | Sealed image input to all three facades, re-exported from `cdk/internal/imgsource.Source`. `ImageFromRegistry(ref)` preserves a digest-pinned reference; `ImageFromEcrRepository(repo, tag)` uses a consumer-managed ECR tag or digest; `ImageFromGoBuild(props)` stages an embedded Dockerfile for a published command. The internal constructors are `NewRegistry`, `NewEcrRepository`, and `NewGoBuild`; `Materialize` creates the CDK image and `RuntimePlatform` aligns the Fargate task with it. |
| **ImageGoBuildProps** | Build settings, re-exported from `imgsource.GoBuildProps`: required published lib-module `Version`, optional command `Package`, `BuildTags`, digest-pinned `GoImage` and `BaseImage`, and `Platform` (`linux/amd64` by default, or `linux/arm64`). The command must support this profile's bootstrap and health check. |
| **DeriveBuildTags** | Sorted, deduplicated optional family tags derived from bridge-config transport and store discriminators. Base AWS, MQTT, native stores and HTTP require no extra tags. Unknown kinds and processor registrations without a profile mapping fail synth. Nil `BuildTags` invokes derivation; an explicit empty slice adds no tags. Aliases follow root `PLUGIN.md`. |
| **BridgeConfigSource (current materialization)** | Supersedes the earlier S3-asset description: `BridgeYamlAsset` reads a local document and `BridgeYamlInline` supplies typed config; both drive validation and grants, and `ImageFromGoBuild` embeds the parsed result without a separate S3 config asset. |
| **Embedded initial config** | Logical document carried in linker string `main.initialConfigBase64`; CDK stages it in `initial-config-<hash>.goenv`, while registry images remain consumer-owned and unchanged. |
| **Embedded initial config digest** | SHA-256 of decoded embedded bytes, inspected with `-initial-config-digest` before runtime or network startup and checked by the Go-build image path without printing the document. |
| **Embedded initial config (native file)** | Supersedes the linker/Go-environment-file path: both commands use `go:embed` on fixed `initial-config.base64`, empty by default; CDK stages pure data in `initial-config-<rawSHA>.base64`, hashes the unencoded serialized document, and fills the command's file in a writable module copy before `go build`. |
| **Local embed overlay** | `scripts/buildconfig` output that maps the fixed command embed file to a generated Base64 payload for Make and Docker builds without changing original source files or passing payload bytes in flags or environment variables. |

### Registries

Explicit producer→consumer wiring for resources referenced by name from bridge YAML. See ARCHITECTURE.md → "Source resolution & cross-stack lookup" and Tier B (Phase 2).

| Term | Meaning |
|---|---|
| **QueueRegistry** | Explicit `string→awssqs.IQueue` map provided as a construct prop. Tier B resolves YAML SQS-by-name references via `QueueRegistry.Ref(name) → QueueRef`. No auto-import scanning. Missing entry → `addError` with self-healing message: `registry.AddQueue("X", queue)`. Lives in `cdk/registry/`. |
| **SsmParamRegistry** | Explicit `string-URI→awsssm.IParameter` map. Tier B resolves YAML SSM URI references via `SsmParamRegistry.Ref(uri) → ParamRef`. Keyed by full URI / parameter path (e.g. `/bridge/mqtt`). Missing entry → `addError`: `registry.AddParameter("/path", param)`. Lives in `cdk/registry/`. |
| **QueueRegistry (embedded references)** | Keeps exact queue handles for grants and dependencies while the document carries a physical name or explicit tag selector; imported-queue tags remain the producer's responsibility. |
| **BindQueueTags** | `QueueRegistry.BindQueueTags(name, tags, prefix)` binds one literal selector to a previously registered queue, applies tags to owned queues, and asserts producer-managed tags for imported queues. |
| **ResolveQueue** | `QueueRegistry.ResolveQueue(cfg sqs.Config) (QueueRef, error)` matches config to registered queue handles without AWS calls, rejecting ambiguous or missing name/tag mappings. |
| **QueueRef.PhysicalName** | Known physical queue name rather than its registry alias or a CDK token; empty when the physical name is unknown at synth. |
| **QueueRef.QueueTags** | Isolated copy of the explicit selector bound through `BindQueueTags`. |
| **QueueRef.QueueNamePrefix** | Optional physical-name prefix bound to the tag selector. |
| **QueueAddress** | `sqs.QueueAddress = "sqs:queue"`, the [project-defined binding marker](../../UBIQUITOUS.md#transport-adapters-adapterstransport) that uses the sender's configured queue while keeping its resolved URL runtime-only. |
| **ValidateEmbeddedSQSConfig** | `bridgecfg` check used by the image builder to require decoded name/tag references, reject embedded queue URLs, and restrict SQS binding addresses to physical names or `sqs:queue`. |
| **GrantSQSConfig** | Shared-base grant derivation that resolves SQS receiver, sender, and binding references through `ResolveQueue`, retaining exact handles for message permissions and dependencies, with metadata reads added only for tag mode. |
| **ScanForPlaintextSecrets** | Explicit `bridgecfg` utility for a consumer-selected reference-only credential policy; neither `Builder.Build` nor construct Phase 1 calls it by default. |

### Drift policy

Historical vocabulary follows. These modes and props are removed; use the
strict [initialization contract](../../docs/aws-deployment/config-initialization.md).
The row is retained as glossary history, not as a supported API.

| Term | Meaning |
|---|---|
| **OnConfigDrift** | Drift-handling policy applied by the seeder init container against the current EFS file or DynamoDB config item. `SeedOnce` (control default) seeds iff absent and warns on drift; `Overwrite` uses the CDK source of truth (DynamoDB writes CAS-bump the row and JSON version); `AbortDeploy` is read-only and exits 10 on mismatch; `AdoptValid` (worker default) adopts valid drift without writes. DynamoDB semantic hashes use actual `data` and ignore only top-level `version`. Configured via `SeederMode` / `ControlSeederMode` and `WorkerSeederMode`; DynamoDB worker modes must remain read-only. |
| **OnConfigDrift (superseded)** | No maintained drift-mode API or seeder container remains; initialization creates only an absent document, existing config wins, and workers never initialize. |

### Cross-stack lookup

| Term | Meaning |
|---|---|
| **BridgeRef** | Consumer-side handle returned by `LookupBridge`. Exposes the same accessor surface (`AdminURL`, `HealthzURL`, optional ARNs) as the producing constructs but resolves values lazily through SSM tokens. |
| **LookupBridge** | Top-level helper `gobridgecdk.LookupBridge(scope, id, ssmPrefix)` returning a `BridgeRef`. Reads `<prefix>/admin-url`, `<prefix>/healthz-url`, `<prefix>/manifest-version` (and optional `<prefix>/alb-arn`, `<prefix>/cluster-arn`, `<prefix>/efs-id` if producer used `IncludeARNs()`). Values use `awsssm.StringParameter_FromStringParameterName` deploy-time tokens. EFS presence uses an optional, context-cached `ValueFromLookup`; `EfsID()` is nil until presence resolves or when the producer has no EFS. Refresh that lookup after changing filesystem use. Manifest-version sentinel allows future schema breaks to fail consumer synth fast. |
