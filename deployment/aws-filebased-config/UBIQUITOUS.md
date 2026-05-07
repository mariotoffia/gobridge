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
| **Runtime plan** | `runtimePlan` struct: bundles logical + resolved configs, swap mode, registry, and either a `PreparedBuild` or a built `*runtime.Runtime`. |
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
| **Config mount path** | Path *inside the container* where the EFS access point is mounted. Default `/mnt/gobridge`. |
| **Config file path** | Absolute path the bootstrap polls for the bridge config. Combines mount path + filename, e.g. `/mnt/gobridge/bridge.yaml`. |

## CDK

| Term | Meaning |
|---|---|
| **L2 construct** | `GoBridgeEfsConfig`, `GoBridgeService`. Composable; consumers wire their own VPC / cluster / ALB. |
| **L3 stack** | `GoBridgeStack`. Opinionated single-call deployment for default-VPC use. |
| **Exposure** | `infra.Exposure` flags (`Admin`, `Monitor`, `TransportHTTP`) selecting which container ports get mapped. Admin :8080 is always mapped (health check requirement) regardless of `Admin`. |
