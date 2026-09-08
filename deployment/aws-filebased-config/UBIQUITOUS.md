# aws-filebased-config — Ubiquitous Language

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

## CDK

| Term | Meaning |
|---|---|
| **L2 construct** | `GoBridgeSingle`, `GoBridgeCluster`, `GoBridgeDynamoDBHA`, `GoBridgeAlarms`. Composable; consumers wire their own VPC / cluster / ALB. There is no L3 stack — see [ARCHITECTURE.md](ARCHITECTURE.md). |
| **Exposure** | `infra.Exposure` flags (`Admin`, `Monitor`, `TransportHTTP`) selecting which container ports get mapped. Admin :8080 is always mapped (health check requirement) regardless of `Admin`. |
| **BridgeConfigSource** | Sealed type representing the source of bridge YAML supplied to a `GoBridgeSingle`, `GoBridgeCluster`, or `GoBridgeDynamoDBHA`. Two constructors: `BridgeYamlAsset(path)` (file → S3 asset) and `BridgeYamlInline(*ports.BridgeConfig)` (in-memory builder output). Construct unwraps internally. Lives in `cdk/gobridgecdk/`. |

### Registries

Explicit producer→consumer wiring for resources referenced by name from bridge YAML. See ARCHITECTURE.md → "Source resolution & cross-stack lookup" and Tier B (Phase 2).

| Term | Meaning |
|---|---|
| **QueueRegistry** | Explicit `string→awssqs.IQueue` map provided as a construct prop. Tier B resolves YAML SQS-by-name references via `QueueRegistry.Ref(name) → QueueRef`. No auto-import scanning. Missing entry → `addError` with self-healing message: `registry.AddQueue("X", queue)`. Lives in `cdk/registry/`. |
| **SsmParamRegistry** | Explicit `string-URI→awsssm.IParameter` map. Tier B resolves YAML SSM URI references via `SsmParamRegistry.Ref(uri) → ParamRef`. Keyed by full URI / parameter path (e.g. `/bridge/mqtt`). Missing entry → `addError`: `registry.AddParameter("/path", param)`. Lives in `cdk/registry/`. |

### Drift policy

| Term | Meaning |
|---|---|
| **OnConfigDrift** | Drift-handling policy applied by the seeder init container against the current EFS file or DynamoDB config item. `SeedOnce` (control default) seeds iff absent and warns on drift; `Overwrite` uses the CDK source of truth (DynamoDB writes CAS-bump the row and JSON version); `AbortDeploy` is read-only and exits 10 on mismatch; `AdoptValid` (worker default) adopts valid drift without writes. DynamoDB semantic hashes use actual `data` and ignore only top-level `version`. Configured via `SeederMode` / `ControlSeederMode` and `WorkerSeederMode`; DynamoDB worker modes must remain read-only. |

### Cross-stack lookup

| Term | Meaning |
|---|---|
| **BridgeRef** | Consumer-side handle returned by `LookupBridge`. Exposes the same accessor surface (`AdminURL`, `HealthzURL`, optional ARNs) as the producing constructs but resolves values lazily through SSM tokens. |
| **LookupBridge** | Top-level helper `gobridgecdk.LookupBridge(scope, id, ssmPrefix)` returning a `BridgeRef`. Reads `<prefix>/admin-url`, `<prefix>/healthz-url`, `<prefix>/manifest-version` (and optional `<prefix>/alb-arn`, `<prefix>/cluster-arn`, `<prefix>/efs-id` if producer used `IncludeARNs()`). Values use `awsssm.StringParameter_FromStringParameterName` deploy-time tokens. EFS presence uses an optional, context-cached `ValueFromLookup`; `EfsID()` is nil until presence resolves or when the producer has no EFS. Refresh that lookup after changing filesystem use. Manifest-version sentinel allows future schema breaks to fail consumer synth fast. |
