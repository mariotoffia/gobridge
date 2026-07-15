# aws-filebased-config — Ubiquitous Language

Terms specific to this deployment profile. Additive to the project-wide [UBIQUITOUS.md](../../UBIQUITOUS.md). If a term appears both here and in the project glossary, the project glossary wins.

## Configuration

There are exactly **two** configuration artifacts. The `bootstrap` package tracks two *states* of the bridge config (`logicalRef`, `appliedRef`) which only differ when a reload was rejected.

| Term | Meaning |
|---|---|
| **Bootstrap config** | Deployment-owned runtime parameters (`infra.BootstrapConfig`). Static per task revision. Delivered via env var `GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` or file `GOBRIDGE_FILEBASED_BOOTSTRAP_FILE`. Distinct from `ports.BridgeConfig`. |
| **Bridge config** | The application's `ports.BridgeConfig` (YAML on EFS). Hot-reloadable. The same artifact whether on disk, in `logicalRef`, or in `appliedRef`. |
| **Logical state** | The bridge config last *seen* on disk and parsed successfully (`logicalRef`). Updated even when the subsequent runtime swap is rejected. |
| **Applied state** | The bridge config the *currently running* runtime was built from (`appliedRef`). On the happy path equals logical state. Diverges only after a failed reload — then logical = rejected new config, applied = last good. Used by `Stop` for `DrainTimeout` and by `recoverPrevious`. |
| **ContainerMemoryBytes** | Bootstrap container hard limit. Defaults to 1 GiB outside CDK; the CDK base always overwrites it from the effective Fargate task `MemoryMiB`, preventing runtime accounting from diverging from the deployed limit. |
| **ReservedMemoryBytes** | Bootstrap reservation for non-MQTT runtime memory. Together with the AWS MQTT memory profile's 25% ingress reservation, it must leave at least 20% of `ContainerMemoryBytes` as headroom. |
| **AWS MQTT memory profile** | Runtime bootstrap policy applied to initial config and every reload: divide 25% of container memory across unique built MQTT sessions that can ingest and derive each default Receive Maximum with the Paho ingress byte model. Every Persistent/Exclusive session referenced by a declared sender consumes one deduplicated share with route concurrency zero even when no route references that sender, because resumed durable state may deliver stale backlog; an Ephemeral sender-only session consumes no share. |

> **Not a layer:** the `resolvedCfg` produced inside `resolveInputs` is a transient working copy with SSM secrets patched into HTTP `Config.APIKey`. It is consumed by the builder and discarded — no ref, no name to learn.

## Topology & Roles

| Term | Meaning |
|---|---|
| **Topology** | Deployment shape. `single` = one replica owns config writes. `filesystem_replicated` = N replicas read the same EFS; cross-instance coordination features are rejected. |
| **NodeRole** | Per-replica identity. `control` (default) or `worker`. Declared in bootstrap; reserved for future coordination. |
| **Filesystem profile guard** | `validateFilesystemProfile` — rejects `shared_outbox` and `route.session` when topology is `filesystem_replicated`. |

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
| **Parameter reference** | A bootstrap field value identifying an SSM parameter. Either a `pms://name/path` URI or an absolute SSM name (`/foo/bar`). Normalized by `normalizeParameterRef`. |
| **Parameter resolver** | The `parameterResolver` interface used by `resolveInputs`. Default is SSM-backed; tests inject custom implementations via `WithParameterResolver`. |
| **DevMode** | Bootstrap flag that authorizes use of `SSMEndpoint` overrides (e.g. LocalStack). Production safety guard: `SSMEndpoint` without `DevMode` fails `Validate()`. |
| **Admin/Monitor key param** | SSM references for the admin (required) and monitor (optional) HTTP API `X-API-Key` values. Re-resolved on every reload. |

## Paths

| Term | Meaning |
|---|---|
| **Access point path** | POSIX path *inside* EFS exposed by the access point. Default `/gobridge`. Set on the access point at creation; immutable thereafter. |
| **Config mount path** | Path *inside the container* where the EFS access point is mounted. Default `/var/lib/gobridge` (single canonical constant `infra.DefaultMountPath`; the Phase-1 store-path validator, the ECS mount, and the seeder all derive from it). |
| **Config file path** | Absolute path the bootstrap polls for the bridge config. Combines mount path + filename, e.g. `/var/lib/gobridge/bridge.yaml`. |

## CDK

| Term | Meaning |
|---|---|
| **L2 construct** | `GoBridgeSingle`, `GoBridgeCluster`, `GoBridgeAlarms`. Composable; consumers wire their own VPC / cluster / ALB. There is no L3 stack — see [ARCHITECTURE.md](ARCHITECTURE.md). |
| **Exposure** | `infra.Exposure` flags (`Admin`, `Monitor`, `TransportHTTP`) selecting which container ports get mapped. Admin :8080 is always mapped (health check requirement) regardless of `Admin`. |
| **BridgeConfigSource** | Sealed type representing the source of bridge YAML supplied to a `GoBridgeSingle`/`GoBridgeCluster`. Two constructors: `BridgeYamlAsset(path)` (file → S3 asset) and `BridgeYamlInline(*ports.BridgeConfig)` (in-memory builder output). Construct unwraps internally. Lives in `cdk/gobridgecdk/`. |

### Registries

Explicit producer→consumer wiring for resources referenced by name from bridge YAML. See ARCHITECTURE.md → "Source resolution & cross-stack lookup" and Tier B (Phase 2).

| Term | Meaning |
|---|---|
| **QueueRegistry** | Explicit `string→awssqs.IQueue` map provided as a construct prop. Tier B resolves YAML SQS-by-name references via `QueueRegistry.Ref(name) → QueueRef`. No auto-import scanning. Missing entry → `addError` with self-healing message: `registry.AddQueue("X", queue)`. Lives in `cdk/registry/`. |
| **SsmParamRegistry** | Explicit `string-URI→awsssm.IParameter` map. Tier B resolves YAML SSM URI references via `SsmParamRegistry.Ref(uri) → ParamRef`. Keyed by full URI / parameter path (e.g. `/bridge/mqtt`). Missing entry → `addError`: `registry.AddParameter("/path", param)`. Lives in `cdk/registry/`. |

### Drift policy

| Term | Meaning |
|---|---|
| **OnConfigDrift** | Drift-handling policy applied by the seeder init container when comparing the bundled asset against the existing EFS file (canonical SHA-256). Three modes: `SeedOnce` (default — seed iff absent, warn if drifted), `Overwrite` (CDK source of truth, GitOps), `AbortDeploy` (strict — exit 10 on hash mismatch). On the cluster, the worker seeder is fixed to `AbortDeploy`. Configured per-construct via `SeederMode` / `ControlSeederMode` props. |

### Cross-stack lookup

| Term | Meaning |
|---|---|
| **BridgeRef** | Consumer-side handle returned by `LookupBridge`. Exposes the same accessor surface (`AdminURL`, `HealthzURL`, optional ARNs) as the producing constructs but resolves values lazily through SSM tokens. |
| **LookupBridge** | Top-level helper `gobridgecdk.LookupBridge(scope, id, ssmPrefix)` returning a `BridgeRef`. Reads `<prefix>/admin-url`, `<prefix>/healthz-url`, `<prefix>/manifest-version` (and optional `<prefix>/alb-arn`, `<prefix>/cluster-arn`, `<prefix>/efs-id` if producer used `IncludeARNs()`). Backed by `awsssm.StringParameter_FromStringParameterName` — soft coupling, deploy-time token, producer rotates freely. Manifest-version sentinel allows future schema breaks to fail consumer synth fast. |
