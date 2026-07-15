# aws-filebased-config

AWS deployment profile for GoBridge. Runs the bridge on **ECS Fargate** with **EFS** for hot-reloadable bridge config and **SSM Parameter Store** (SecureString) for secrets. The CDK surface is a flat set of composable L2 constructs — **there is no L3 wrapper stack**: consumers wire VPC, cluster, ALB and registries themselves.

## Module Layout

| Path                                          | Purpose                                                                                 |
|-----------------------------------------------|-----------------------------------------------------------------------------------------|
| `infra/`                                      | Zero-dep types (`BootstrapConfig`, `Exposure`, `AppSpec`). No CDK / runtime imports.    |
| `cdk/gobridgecdk/`                            | Top-level facade: `BridgeYamlAsset`, `BridgeYamlInline`, `LookupBridge`, `BridgeRef`.   |
| `cdk/bridgecfg/`                              | Fluent `*Builder` for `*ports.BridgeConfig` + plaintext-secret scanner.                 |
| `cdk/registry/`                               | `QueueRegistry` / `SsmParamRegistry` mapping logical names → CDK handles.               |
| `cdk/constructs/`                             | `GoBridgeEfsConfig` (shared EFS + 2 access points).                                     |
| `cdk/constructs/gobridgesingle/`              | `GoBridgeSingle` — one Fargate task, RW EFS.                                            |
| `cdk/constructs/gobridgecluster/`             | `GoBridgeCluster` — control + worker(s), shared EFS (RW/RO split).                      |
| `cdk/constructs/gobridgealbattachment/`       | `GoBridgeALBAttachment` — derives target groups + listener rules from the deployed yaml.|
| `cdk/constructs/gobridgealarms/`              | `GoBridgeAlarms` — opinionated CloudWatch alarm bundle.                                 |
| `cdk/ssmexports/`                             | Functional options for the cross-stack SSM export contract.                             |
| `cdk/integration/`                            | `//go:build integration_aws` end-to-end tests (opt-in).                                 |
| `lib/`                                        | Runtime bootstrap library + binary `gobridge-filebased`.                                |

`infra/` is intentionally dependency-free so CDK consumers never pull the runtime tree.

## Public Constructs

| Construct | Package | Purpose |
|-----------|---------|---------|
| `GoBridgeSingle` | `cdk/constructs/gobridgesingle` | One Fargate control task with RW EFS. `DesiredCount=1` (deploy 0/100). |
| `GoBridgeCluster` | `cdk/constructs/gobridgecluster` | Control task (RW) + N worker tasks (RO) sharing one EFS. Replicated **scale-out**, **not** HA failover (no single-active lease owner). Default workers = 2; optional CPU autoscaling. |
| `GoBridgeEfsConfig` | `cdk/constructs` | EFS file system + control & worker access points (always-on encryption, ELASTIC throughput, RETAIN). |
| `GoBridgeALBAttachment` | `cdk/constructs/gobridgealbattachment` | Two target groups + listener rules derived from yaml admin paths and HTTP receivers. Reserves `[BasePriority, BasePriority+99]`. |
| `GoBridgeAlarms` | `cdk/constructs/gobridgealarms` | Alarms: control absence, worker degraded, EFS IO, ALB unhealthy hosts, ALB 5xx. SNS-routed. |

Supporting:

| Package | Surface |
|---------|---------|
| `cdk/gobridgecdk` | `BridgeYamlAsset(path) BridgeConfigSource`, `BridgeYamlInline(*ports.BridgeConfig) BridgeConfigSource`, `LookupBridge(scope, id, prefix, opts...) *BridgeRef`. |
| `cdk/bridgecfg` | `bridgecfg.New(name).With…().Build() (*ports.BridgeConfig, error)` plus `ScanForPlaintextSecrets`, `RegisterCredentialScheme`, `RegisterSensitiveField`. |
| `cdk/registry` | `NewQueueRegistry()` + `AddQueue(name, IQueue)`; `NewSsmParamRegistry()` + `AddParameter(uri, IParameter)`. |
| `cdk/ssmexports` | `IncludeARNs()` option for `WithSSMExports`. |

## Authoring Bridge Configuration

Two paths converge on the sealed `gobridgecdk.BridgeConfigSource` consumed by both facades:

```go
// (a) on-disk yaml — uploaded as an asset and parsed once for tier-B validation.
src := gobridgecdk.BridgeYamlAsset("config/bridge.yaml")

// (b) typed builder — assembled in Go, marshalled at synth time.
cfg, err := bridgecfg.New("my-bridge").
    WithSQSReceiver("orders-in", queues.Ref("orders-in")).
    WithSQSSender("orders-out", queues.Ref("orders-out")).
    WithRoute("orders-in", "orders-out").
    WithSQLiteOutbox("/var/lib/gobridge/outbox.db").
    Build()
if err != nil { panic(err) }
src := gobridgecdk.BridgeYamlInline(cfg)
```

Both factories return the same opaque token; the construct does file read / YAML marshal / parse / Phase-1 validation in one synth pass.

## Quickstart

### Snippet 1 — `GoBridgeSingle`

```go
package main

import (
    "github.com/aws/aws-cdk-go/awscdk/v2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
    "github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
    "github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
    "github.com/aws/jsii-runtime-go"

    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func main() {
    app := awscdk.NewApp(nil)
    stack := awscdk.NewStack(app, jsii.String("BridgeStack"), nil)

    vpc := awsec2.Vpc_FromLookup(stack, jsii.String("Vpc"),
        &awsec2.VpcLookupOptions{IsDefault: jsii.Bool(true)})
    cluster := awsecs.NewCluster(stack, jsii.String("Cluster"), &awsecs.ClusterProps{
        Vpc: vpc, ContainerInsights: jsii.Bool(true),
    })

    queues := registry.NewQueueRegistry()
    queues.AddQueue("orders-in",
        awssqs.Queue_FromQueueArn(stack, jsii.String("OrdersIn"),
            jsii.String("arn:aws:sqs:eu-west-1:123456789012:orders-in")))

    params := registry.NewSsmParamRegistry()
    params.AddParameter("/bridge/admin-key",
        awsssm.StringParameter_FromSecureStringParameterAttributes(stack,
            jsii.String("AdminKey"), &awsssm.SecureStringParameterAttributes{
                ParameterName: jsii.String("/bridge/admin-key"),
            }))

    gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
        Vpc:              vpc,
        Cluster:          cluster,
        Image:            awsecs.ContainerImage_FromRegistry(jsii.String("ghcr.io/mariotoffia/gobridge:latest"), nil),
        Bootstrap:        infra.BootstrapConfig{ /* admin/monitor addrs, etc. */ },
        BridgeConfig:     gobridgecdk.BridgeYamlAsset("config/bridge.yaml"),
        QueueRegistry:    queues,
        SsmParamRegistry: params,
    })

    app.Synth(nil)
}
```

### Snippet 2 — `GoBridgeCluster` + ALB attachment + alarms

```go
bridge := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
    Vpc:              vpc,
    Cluster:          cluster,
    Image:            awsecs.ContainerImage_FromEcrRepository(repo, jsii.String("v1.2.3")),
    Bootstrap:        bootstrap,
    BridgeConfig:     gobridgecdk.BridgeYamlInline(cfg),
    QueueRegistry:    queues,
    SsmParamRegistry: params,
    WorkerDesiredCount: jsii.Number(3),
    AutoScaling:      &gobridgecluster.AutoScalingProps{Min: 2, Max: 10, TargetCPU: 60},
})

attachment := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Attach"),
    &gobridgealbattachment.AttachmentProps{
        Cluster:      bridge,
        Listener:     listener, // consumer-managed elbv2.IApplicationListener
        Vpc:          vpc,
        BridgeConfig: gobridgecdk.BridgeYamlInline(cfg),
        BasePriority: 200,
    }).
    WithCfnOutputs("Bridge").
    WithSSMExports("/bridges/prod", ssmexports.IncludeARNs())

gobridgealarms.NewGoBridgeAlarms(stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
    Cluster:    bridge,
    Efs:        bridge.EfsConfig(),
    Attachment: attachment,
    AlarmTopic: snsTopic,
})
```

> **No L3 wrapper.** This profile no longer ships an opinionated single-call stack or service construct — every consumer composes the L2 constructs above inside their own `awscdk.Stack`.

## Singleton Constraint

> ⚠️ **One `GoBridgeSingle` OR one `GoBridgeCluster` per Stack tree.**
> A synth-time scope scan (see `cdk/constructs/internal/singleton`) panics if two facades share the enclosing `awscdk.Stack`. Cross-account / cross-stack deployments are the operator's responsibility — wire each bridge into its own stack and use `GoBridgeALBAttachment.WithSSMExports` + `gobridgecdk.LookupBridge` for cross-stack consumption.
>
> The bridge identity is taken from the deployed yaml's `bridge.name` field (validated against `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$` by Phase-1 tier-B validation). There is intentionally **no `Name` prop** on `SingleProps` / `ClusterProps`: a single source of truth avoids the deploy-vs-config drift class.

## Cross-Stack Lookup

Producer side (the bridge stack):

```go
attachment.WithSSMExports("/bridges/prod", ssmexports.IncludeARNs())
```

Publishes (under the chosen prefix): `admin-url`, `healthz-url`, `manifest-version`, plus `alb-arn`, `cluster-arn`, `efs-id` when `IncludeARNs()` is set.

Consumer side (any other stack / account that can read those parameters):

```go
ref := gobridgecdk.LookupBridge(stack, "ProdBridge", "/bridges/prod", ssmexports.IncludeARNs())
// ref.AdminURL(), ref.HealthzURL(), ref.PublicDnsName(),
// ref.AlbARN(), ref.ClusterARN(), ref.EfsID(), ref.ManifestVersion()
```

`manifest-version` is resolved via `awsssm.StringParameter_ValueFromLookup` (real synth-time string, cached in `cdk.context.json`) so a producer/consumer schema mismatch surfaces as a CDK Annotation error rather than a runtime surprise.

## Secrets Policy

- The fluent builder runs `ScanForPlaintextSecrets` from `Build()`. Default sensitive field names (matched on the deepest map key, case-insensitive): `password`, `secret`, `api_key`, `apikey`, `client_secret`, `bearer_token`, `private_key`, `privatekey`, `token`, `auth_token`, `access_token`, `refresh_token`, `passphrase`. See `cdk/bridgecfg/secrets.go`.
- Any literal string at one of those keys is rejected unless it is a credential URI from the registered allow-list (`pms`, `file` — extend with `bridgecfg.RegisterCredentialScheme(...)`).
- The supported secret backend is **SSM Parameter Store SecureString**, addressed as `pms:///path/to/param`. Wire each `pms://` URI through `SsmParamRegistry.AddParameter`; the construct grants `ssm:GetParameter[s]` (and KMS decrypt where applicable) only for registered parameters.
- The asset path (`BridgeYamlAsset`) and the marshalled inline config (`BridgeYamlInline`) are both run through the same parser plus tier-B validators, so the scan applies regardless of authoring path.

## What It Provisions

**`GoBridgeSingle`**

- One ECS Fargate service, `DesiredCount=1`, deployment policy `MinHealthyPercent=0 / MaxHealthyPercent=100` (full drain before replace — eliminates concurrent EFS RW writers).
- One control EFS access point mounted RW.
- ECS cluster auto-created when `Cluster` is nil; Container Insights on for the auto-created cluster.
- aws-cli **seeder** sidecar (mode `SeedOnce` by default) lays down the parsed yaml on EFS at first boot.
- Task SG → EFS SG NFS:2049 ingress; IAM grants for EFS client access, SSM `GetParameter`, KMS decrypt (when `EfsKmsKey` is set).
- CloudWatch Logs group (default retention one month, RETAIN).

**`GoBridgeCluster`**

> ⚠️ **REPLICATED SCALE-OUT, NOT HIGH AVAILABILITY.** Despite the name,
> `GoBridgeCluster` is a *filesystem-replicated scale-out* topology: N replicas
> independently read one shared EFS config to scale throughput for **independent
> routes**. It is **not** coordinated active/standby lease failover. It
> deliberately **forces** `topology=filesystem_replicated` and **rejects**
> `shared_outbox` and `route.session` leases, so there is **no single-active
> lease owner and no 30–60s (or any bounded) HA failover SLO** — replicas do not
> take over for each other. Coordinated failover requires DynamoDB-backed
> lease/outbox stores; a DynamoDB-backed HA construct is **future work**, out of
> scope for this reference construct. See the `GoBridgeCluster` type doc
> (`cdk/constructs/gobridgecluster/cluster.go`) for the full advisory.

- All of the above, plus:
- Worker ECS Fargate service (`WorkerDesiredCount` default `2`, standard rolling deploy, optional CPU target-tracking via `AutoScalingProps`).
- Shared EFS file system with **two** access points (control RW, worker RO); RW/RO split enforced at IAM + ECS volume level.
- Worker seeder runs in `AdoptValid` mode — workers never write configuration
  but **adopt** whatever valid `bridge.yaml` the control node last wrote (CDK
  seed *or* an Admin-API `config-txn` commit) instead of aborting on hash
  drift. This lets the two reconfiguration paths coexist: an Admin-API edit no
  longer wedges later worker scale-out / crash-replacement. Set
  `ClusterProps.WorkerSeederMode = jsii.String("AbortDeploy")` for strict
  lock-step (workers refuse to start on any drift from the synth-time asset;
  never use the Admin API to reconfigure in that mode). See
  [seeder/README.md](cdk/constructs/internal/seeder/README.md#reconfiguration-paths-why-adoptvalid-is-the-worker-default).
- Separate task SGs (`ControlSecurityGroup`, `WorkerSecurityGroup`) both granted EFS ingress.

**`GoBridgeEfsConfig`** — created automatically by either facade when `EfsConfig` is nil; can be passed in to share one filesystem across facades (within the singleton-per-stack rule) or to override KMS / throughput / removal policy / backup.

## Constraints

- **Singleton**: see above.
- **Topology = `filesystem_replicated` (SCALE-OUT, not HA failover)** rejects routes that need cross-instance write coordination (`shared_outbox`, `route.session` lease). Those routes require a distributed lease/outbox store (e.g. DynamoDB) that the file-based EFS profile does not provision — remove them from `bridge.yaml`, or provision your own DynamoDB-backed lease/outbox store. `GoBridgeCluster` **forces** `filesystem_replicated` on both task definitions regardless of the caller's `Bootstrap.Topology`, so these guards always fire for a multi-instance cluster. Consequence: replicas are independent readers of the same config — there is **no single-active lease owner, no coordinated active/standby failover, and no 30–60s failover SLO**. Coordinated HA failover is future work (a DynamoDB-backed HA construct).
- **`SSMEndpoint`** set without `DevMode = true` fails Bootstrap validation (production-bypass guard).
- Bootstrap env payload capped at 1 MiB.
- **MQTT ingress memory:** the CDK base stamps the actual Fargate task memory
  into bootstrap. Runtime reserves 25% for consumed MQTT ingress sessions,
  divides it equally by session, and derives each default Receive Maximum with
  the adapter's byte model. `reserved_memory_bytes` plus this reservation must
  leave at least 20% task headroom. Used Persistent/Exclusive sender-only
  sessions consume a share with route concurrency zero because resumed durable
  state can deliver stale backlog; Ephemeral sender-only sessions consume no
  share. Impossible or explicitly unsafe profiles fail startup/reload.
- `GoBridgeALBAttachment` reserves listener-rule priorities `[BasePriority, BasePriority+99]`. Add the attachment last on a listener, or pick a `BasePriority` outside any consumer-managed range.
- `ControlAbsence` / `WorkerDegraded` alarms read Container Insights metrics: when passing your own `Cluster`, enable Container Insights yourself.

## Runtime Library

`lib/bootstrap.NewApp(cfg, opts...)` loads `BootstrapConfig` from env (`GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` or `…_FILE`, max 1 MiB), polls `ConfigFilePath` on EFS, reloads bridge config without restart, resolves `pms://` SSM secrets, applies the MQTT ingress memory profile on every initial load/reload, and starts the admin / monitor / transport HTTP servers plus a `bridge.Runtime` with the `mqtt`, `sqs`, `http` transports and `memory`, `sqlite` stores. Reload uses `swapModeOverlap` by default; `swapModePrepareCommit` when any transport advertises `CapExclusiveIdentity`.

Options: `WithLogger`, `WithLogLevelVar`, `WithParameterResolver`, `WithCredentialStore`, `WithShutdownTimeout`, `WithTerminalPollInterval`. Binary: `lib/cmd/gobridge-filebased`.

**Terminal-runtime backstop.** `App.Run` polls the active runtime and returns
`ErrRuntimeTerminal` (exiting the process non-zero) once the runtime enters an
unrecoverable terminal state, so the orchestrator restarts the task instead of
leaving a "running" container that bridges nothing. The container image also
ships a self-probing health check: the CDK task definition sets
`HealthCheck.Command = ["CMD", "/usr/local/bin/gobridge-filebased", "-healthcheck"]`
(the binary GETs its own monitor `/live` endpoint, which 503s on terminal) and
`StopTimeout = 60s` (> the 30s drain budget, so in-flight drains are not
SIGKILLed). Override via `HealthCheckCommand`, `DisableHealthCheck`,
`StopTimeout` and `ContainerUser` on the base props (exposed through the
facades' internal base).

**Hot log level.** `bridge.log_level` in the reloaded `bridge.yaml` retunes
verbosity at runtime when the binary wires a `*slog.LevelVar` via
`WithLogLevelVar` (the production binary does this by default) — no redeploy
needed. Recognized values: `debug`, `info`, `warn`, `error` (unknown/empty
leaves the level unchanged).

**Container image.** Built by the repository-root `Dockerfile`
(`make docker-build`): a static, CGO-free `gobridge-filebased` on
`distroless/static:nonroot` (runs as uid:gid `65532:65532`, no shell/curl/wget).
Published to `ghcr.io/mariotoffia/gobridge:<tag>` by the release workflow on
core `v*` tags.

## Integration Tests

Opt-in end-to-end tests live under `cdk/integration/`, all guarded by `//go:build integration_aws`. They are excluded from the default `go test ./...` run.

```bash
make integration-aws   # cd cdk && go test -tags=integration_aws -count=1 -timeout=45m ./integration/...
```

Required environment: `GOBRIDGE_INT_*` variables (account/region/image), live AWS credentials, and the `cdk` CLI on PATH. Cadence: nightly on `main` and on every release tag; not gated on PRs.

## Related Docs

| Doc | Scope |
|-----|-------|
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Module-internal architecture, DDD mapping, runtime/CDK layering. |
| [UBIQUITOUS.md](./UBIQUITOUS.md) | Terms unique to this deployment profile. |
| [docs/aws-deployment/overview.md](../../docs/aws-deployment/overview.md) | End-to-end AWS architecture, IAM, image build. |
| [docs/aws-deployment/configuration.md](../../docs/aws-deployment/configuration.md) | Full bootstrap + bridge config reference. |
| [docs/aws-deployment/tco.md](../../docs/aws-deployment/tco.md) | Cost analysis with worked use cases. |
| [docs/scenarios/cdk/](../../docs/scenarios/cdk/) | Default-VPC, custom-VPC, API GW, production, multi-bridge cluster. |
