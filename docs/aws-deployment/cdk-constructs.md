# CDK Construct Library

The constructs that wire a deployment together, the props each one takes, and
a complete worked example.

Part of the [AWS Deployment Overview](overview.md).

---

The GoBridge CDK constructs are written in Go and live at:

```text
github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs
```

The shared infrastructure types (zero external dependencies) live at:

```text
github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra
```

## Construct Overview

The library provides an EFS config construct plus three façade constructs, each
deploying a complete profile:

| Construct | Package | Purpose |
|-----------|---------|---------|
| `GoBridgeEfsConfig` | `cdk/constructs` | EFS filesystem + access point for config mounting. |
| `GoBridgeSingle` | `cdk/constructs/gobridgesingle` | One control Fargate task, RW EFS mount, no worker, no clustering. |
| `GoBridgeCluster` | `cdk/constructs/gobridgecluster` | Independent filesystem scale-out: one control task plus workers. |
| `GoBridgeDynamoDBHA` | `cdk/constructs/gobridgedynamodbha` | DynamoDB-coordinated active/warm-standby: one control plus at least two workers and three owned tables. |

The constructors are `NewGoBridgeSingle(scope, id, *SingleProps)`,
`NewGoBridgeCluster(scope, id, *ClusterProps)`, and
`NewGoBridgeDynamoDBHA(scope, id, *DynamoDBHAProps)`. There is no `GoBridgeService` or
`GoBridgeStack` construct and no `GoBridgeServiceProps` type.

## GoBridgeEfsConfigProps

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Vpc` | `awsec2.IVpc` | *required* | VPC for EFS mount targets. |
| `FileSystem` | `awsefs.IFileSystem` | new filesystem | Existing EFS filesystem to reuse. |
| `AccessPointPath` | `*string` | `/gobridge` | POSIX path inside EFS. |
| `PosixUID` | `*string` | `"1000"` | POSIX user ID for the access points. |
| `PosixGID` | `*string` | `"1000"` | POSIX group ID for the access points. |
| `RemovalPolicy` | `interface{}` | `RETAIN` | What happens to the filesystem on stack deletion. |

## SingleProps (selected)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Vpc` | `awsec2.IVpc` | *required* | VPC for the task and EFS mount targets. |
| `Image` | `awsecs.ContainerImage` | *required* | The `gobridge-filebased` runtime image. |
| `Bootstrap` | `infra.BootstrapConfig` | *required* | Runtime config; `NodeRole` forced to `control`. |
| `BridgeConfig` | `source.Source` | *required* | Sealed config from `gobridgecdk.BridgeYamlAsset`/`BridgeYamlInline`. |
| `QueueRegistry` | `*registry.QueueRegistry` | conditionally required | Resolves SQS queue names in the config. |
| `SsmParamRegistry` | `*registry.SsmParamRegistry` | conditionally required | Resolves SSM parameter URIs in the config. |
| `CPU` | `*float64` | `512` | Fargate CPU units. |
| `MemoryMiB` | `*float64` | `1024` | Fargate memory (MiB). |
| `MountPath` | `*string` | `/var/lib/gobridge` | Container EFS mount path. |
| `SeederMode` | `*string` | `"SeedOnce"` | Control seeder mode. |

The single profile runs exactly one task (`DesiredCount` is not a prop) and has
no auto-scaling.

## ClusterProps (selected)

`ClusterProps` shares `Vpc`, `Image`, `Bootstrap`, `BridgeConfig`, the
registries, `CPU`, `MemoryMiB`, and `MountPath` with `SingleProps` (applied to
both services), plus:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `WorkerDesiredCount` | `*float64` | `2` | Worker task count (must be ≥ 1). |
| `ControlSeederMode` | `*string` | `"SeedOnce"` | Control seeder mode. |
| `WorkerSeederMode` | `*string` | `"AdoptValid"` | Worker seeder mode (see below). |
| `AutoScaling` | `*AutoScalingProps` | `nil` (off) | Opt-in worker CPU target-tracking (`{Min, Max, TargetCPU}`, `TargetCPU` `0` → 70). |

The control task always runs a single copy (`DesiredCount` is hard-coded to 1).
Auto-scaling applies to the worker service only and is off unless `AutoScaling`
is set.

## DynamoDBHAProps (selected)

`DynamoDBHAProps` shares the common VPC, image, bootstrap, config, registry,
sizing, and EFS fields. Its `WorkerDesiredCount` defaults to `2` and must be a
resolved finite integer greater than or equal to `2`; unresolved numeric tokens
are rejected. It has no worker auto-scaling surface. Table names and the
deployment-profile fingerprint are derived from the admitted bridge config and
injected into bootstrap by the facade, not supplied independently by callers, as
is the baseline config digest of the seeded document.

### Worker seeder: AdoptValid vs AbortDeploy

Workers mount EFS read-only and cannot write config, so their default seeder
mode is **`AdoptValid`**: on startup a worker adopts whatever valid `bridge.yaml`
the EFS filesystem currently holds — whether written by the CDK seed or by an
Admin-API config-txn commit — and never fails on hash drift from the synth-time
asset. This lets the two reconfiguration paths coexist: a CDK redeploy and a
live admin edit both leave a valid file that scale-out and crash-replacement
workers pick up without a redeploy. Set `WorkerSeederMode` to **`AbortDeploy`**
for strict lock-step deployments where every worker must match the synth-time
asset exactly (an absent or mismatched file aborts the task).

## Usage Example

```go
import (
    gobridgesingle "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
    "github.com/aws/jsii-runtime-go"
)

qr := registry.NewQueueRegistry()
qr.AddQueue("inbound", inboundQueue)

// cfg is a *ports.BridgeConfig (build it with the bridgecfg builder).
single := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
    Vpc:   vpc,
    Image: awsecs.ContainerImage_FromRegistry(jsii.String("123456789.dkr.ecr.eu-west-1.amazonaws.com/gobridge:latest"), nil),
    Bootstrap: infra.BootstrapConfig{
        BridgeID:         "my-bridge",
        ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
        AdminAPIKeyParam: "/myapp/admin-key",
    },
    BridgeConfig:  gobridgecdk.BridgeYamlInline(cfg),
    QueueRegistry: qr,
})
_ = single
```

For a control + worker pair, use `gobridgecluster.NewGoBridgeCluster` with
`ClusterProps` (add `WorkerDesiredCount` and, optionally, `AutoScaling`).

See [CDK Scenarios](../scenarios/cdk/) for complete, runnable examples.

---
