# aws-filebased-config

AWS deployment profile for GoBridge. Runs the bridge on **ECS Fargate** with **EFS** for hot-reloadable bridge config and **SSM Parameter Store** for secrets.

## Module Layout

| Path                | Go module | Purpose                                                                     |
|---------------------|-----------|-----------------------------------------------------------------------------|
| `infra/`            | yes       | Zero-dep types (`BootstrapConfig`, `ServiceProps`, `Exposure`, `AppSpec`).  |
| `cdk/`              | yes       | CDK v2 (Go) constructs + opinionated L3 stack + synth `main.go`.            |
| `cdk/constructs/`   | —         | `GoBridgeEfsConfig` (L2), `GoBridgeService` (L2).                           |
| `lib/`              | yes       | Runtime bootstrap library (`bootstrap.App`) + binary `gobridge-filebased`.  |
| `lib/model/`        | —         | Internal mirror of `BootstrapConfig` (kept in sync with `infra/`).          |

`infra/` is intentionally dependency-free so CDK consumers never pull the runtime tree.

## What It Provisions

- ECS Fargate **service** (CPU/mem configurable; CPU-target auto-scaling).
- **EFS** filesystem + **access point** (POSIX uid/gid 1000, IAM-auth, transit encryption); read-only mount in container.
- **CloudWatch Logs** group with configurable retention.
- **Security groups**: task → EFS:2049 wired automatically.
- **IAM**: task role granted `elasticfilesystem:ClientMount/ClientRead` and `ssm:GetParameter` on supplied parameter ARNs.
- Container **health check** (`wget /healthz` on `:8080`).
- **Port mappings** driven by `infra.Exposure` (admin `:8080` always; monitor `:8081`; transport HTTP `:8082`).
- Bootstrap config injected via env var `GOBRIDGE_FILEBASED_BOOTSTRAP_JSON`.

> No ALB / target group is created. Consumers attach `GoBridgeService.Service()` to their own load balancer.

## Configuration Surface

### `cdk/constructs.GoBridgeServiceProps`

| Field                | Required | Default              | Purpose                                              |
|----------------------|---------|-----------------------|------------------------------------------------------|
| `Vpc`                | yes     | —                     | Target VPC.                                          |
| `Cluster`            | no      | new cluster           | Reuse existing ECS cluster.                          |
| `ServiceName`        | yes     | —                     | ECS service name.                                    |
| `Image`              | yes     | —                     | `awsecs.ContainerImage`.                             |
| `Bootstrap`          | yes     | —                     | `infra.BootstrapConfig` (serialized to env).         |
| `CPU`                | no      | `512`                 | Fargate CPU units.                                   |
| `MemoryMiB`          | no      | `1024`                | Task memory MiB.                                     |
| `DesiredCount`       | no      | `1`                   | Initial replicas.                                    |
| `EfsConfig`          | no      | auto-created          | Inject `GoBridgeEfsConfig`.                          |
| `ConfigMountPath`    | no      | `/mnt/gobridge`       | In-container mount point.                            |
| `SsmParameterArns`   | no      | none                  | Extra SSM params task may read.                      |
| `Exposure`           | no      | admin only            | Toggle monitor / transport HTTP port mappings.       |
| `LogRetention`       | no      | `ONE_WEEK`            | CloudWatch log retention.                            |
| `ScalingMaxCapacity` | no      | `4` (`0` disables)    | Auto-scaling max.                                    |
| `CpuTargetPercent`   | no      | `70`                  | Auto-scaling CPU target.                             |

### `cdk/constructs.GoBridgeEfsConfigProps`

| Field             | Required | Default     | Purpose                                  |
|-------------------|----------|-------------|------------------------------------------|
| `Vpc`             | yes      | —           | VPC for mount targets.                   |
| `FileSystem`      | no       | new EFS     | Reuse existing `IFileSystem`.            |
| `AccessPointPath` | no       | `/gobridge` | POSIX path inside EFS.                   |
| `PosixUID`/`GID`  | no       | `1000`      | Access point identity.                   |
| `RemovalPolicy`   | no       | `RETAIN`    | Declared; not currently applied in code. |

### `cdk.GoBridgeStackProps` (L3)

`StackProps`, `ServiceName`, `ImageURI`, `Bootstrap`, `Exposure`, `VpcID` (lookup vs create), `MaxAZs` (default `2`).

> L3 uses `ContainerImage_FromRegistry` only. For ECR/asset images, drop to L2 `GoBridgeService`.

### `infra.BootstrapConfig` (env-serialized to runtime)

Identity (`BridgeID`, `NodeRole`, `Topology`), file watch (`ConfigFilePath`, `PollInterval`), listener addresses (`AdminAddr`, `MonitorAddr`, `TransportHTTPAddr`, `CORSOrigins`), SSM-backed secrets (`AdminAPIKeyParam` required; `MonitorAPIKeyParam` + per-receiver/sender HTTP key param maps), AWS overrides (`AWSRegion`, `SSMEndpoint` requires `DevMode`).

Full field reference: [docs/aws-deployment/configuration.md](../../docs/aws-deployment/configuration.md).

## Quickstart (L3 stack)

```go
NewGoBridgeStack(app, "MyBridgeStack", &GoBridgeStackProps{
    ServiceName: "my-bridge",
    ImageURI:    "123456789.dkr.ecr.eu-west-1.amazonaws.com/gobridge:latest",
    Bootstrap: infra.BootstrapConfig{
        BridgeID:         "my-bridge",
        ConfigFilePath:   "/mnt/gobridge/bridge.yaml",
        AdminAPIKeyParam: "/myapp/admin-key",
    },
    Exposure: infra.Exposure{Admin: true, Monitor: true},
})
```

L2 composition (existing VPC/cluster, ECR/asset image, custom ALB):

```go
gobridgecdk.NewGoBridgeService(stack, jsii.String("Bridge"), &gobridgecdk.GoBridgeServiceProps{
    Vpc:         vpc,
    Cluster:     cluster,
    ServiceName: "my-bridge",
    Image:       awsecs.ContainerImage_FromEcrRepository(repo, jsii.String("v1.2.3")),
    Bootstrap:   bootstrap,
    Exposure:    infra.Exposure{Monitor: true, TransportHTTP: true},
})
```

## Runtime Library

`lib/bootstrap.NewApp(cfg, opts...)` returns an `App`:

- Loads `BootstrapConfig` from env (`GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` or `GOBRIDGE_FILEBASED_BOOTSTRAP_FILE`, max 1 MiB).
- Polls `ConfigFilePath` on EFS, reloads bridge config without restart.
- Resolves SSM secrets (`pms://` URIs supported).
- Starts admin/monitor HTTP servers, transport HTTP server, and a `bridge.Runtime` with registered transports (`mqtt`, `sqs`, `http`) and stores (`memory`, `sqlite`).
- On reload uses `swapModeOverlap` by default; `swapModePrepareCommit` when any transport advertises `CapExclusiveIdentity`.

Options: `WithLogger`, `WithParameterResolver`, `WithCredentialStore`, `WithShutdownTimeout`.

Binary entrypoint: `lib/cmd/gobridge-filebased`.

## Related Docs

| Doc | Scope |
|-----|-------|
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Module-internal architecture, DDD mapping, runtime/CDK layering. |
| [UBIQUITOUS.md](./UBIQUITOUS.md) | Terms unique to this deployment profile. |
| [docs/aws-deployment/overview.md](../../docs/aws-deployment/overview.md) | End-to-end AWS architecture, IAM, image build. |
| [docs/aws-deployment/configuration.md](../../docs/aws-deployment/configuration.md) | Full bootstrap + bridge config reference. |
| [docs/aws-deployment/tco.md](../../docs/aws-deployment/tco.md) | Cost analysis with worked use cases. |
| [docs/scenarios/cdk/](../../docs/scenarios/cdk/) | Default-VPC, custom-VPC, API GW, production, multi-bridge cluster. |

## Constraints

- `Topology = filesystem_replicated` rejects routes that need cross-instance state (`shared_outbox`, `route.session` lease). Use the HA/DynamoDB profile instead.
- `SSMEndpoint` set without `DevMode = true` fails validation (production-bypass guard).
- Bootstrap file size capped at 1 MiB.
