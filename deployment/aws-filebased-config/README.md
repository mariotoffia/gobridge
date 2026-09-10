# aws-filebased-config

## Overview

AWS deployment profile for GoBridge. Runs on Amazon Elastic Container Service
(ECS) Fargate with config in Elastic File System (EFS) or DynamoDB. AWS Systems
Manager (SSM) Parameter Store supplies runtime credentials when configured.
Consumers compose Cloud Development Kit (CDK) constructs inside their own
stack; there is no wrapper stack.

The runtime can embed an initial logical document and create an absent target
at first startup. Existing config is never overwritten by initialization.
No configuration seeder container or S3 config download is required. See
[initial configuration](../../docs/aws-deployment/config-initialization.md).

## Module Layout

| Path                                          | Purpose                                                                                 |
|-----------------------------------------------|-----------------------------------------------------------------------------------------|
| `infra/`                                      | Zero-dep types (`BootstrapConfig`, `Exposure`, `AppSpec`). No CDK / runtime imports.    |
| `cdk/gobridgecdk/`                            | Top-level facade: `BridgeYamlAsset`, `BridgeYamlInline`, `LookupBridge`, `BridgeRef`.   |
| `cdk/bridgecfg/`                              | Fluent `*Builder` for `*ports.BridgeConfig`. |
| `cdk/registry/`                               | `QueueRegistry` / `SsmParamRegistry` mapping logical names → CDK handles.               |
| `cdk/constructs/`                             | `GoBridgeEfsConfig` (shared EFS + 2 access points).                                     |
| `cdk/constructs/gobridgesingle/`              | `GoBridgeSingle` — one Fargate task, RW EFS.                                            |
| `cdk/constructs/gobridgecluster/`             | `GoBridgeCluster` — independent filesystem-replicated scale-out (no coordinated failover). |
| `cdk/constructs/gobridgedynamodbha/`          | `GoBridgeDynamoDBHA` — DynamoDB-coordinated active/warm-standby HA.                     |
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
| `GoBridgeDynamoDBHA` | `cdk/constructs/gobridgedynamodbha` | One config-control task plus at least two workers using DynamoDB lease, shared outbox, and exact managed-subscription history for coordinated active/warm-standby failover. |
| `GoBridgeEfsConfig` | `cdk/constructs` | EFS file system + control & worker access points (always-on encryption, ELASTIC throughput, RETAIN). |
| `GoBridgeALBAttachment` | `cdk/constructs/gobridgealbattachment` | Two target groups + listener rules derived from yaml admin paths and HTTP receivers. Reserves `[BasePriority, BasePriority+99]`. |
| `GoBridgeAlarms` | `cdk/constructs/gobridgealarms` | Base ECS/EFS/ALB alarms plus HA warm-standby, DynamoDB, lease, outbox, DLQ, and externally measured failure-to-Full alarms. SNS-routed. |

Supporting:

| Package | Surface |
|---------|---------|
| `cdk/gobridgecdk` | `BridgeYamlAsset(path) BridgeConfigSource`, `BridgeYamlInline(*ports.BridgeConfig) BridgeConfigSource`, `LookupBridge(scope, id, prefix, opts...) *BridgeRef`. |
| `cdk/bridgecfg` | `bridgecfg.New(name).With…().Build() (*ports.BridgeConfig, error)`; explicit `ScanForPlaintextSecrets` utility, not a default policy. |
| `cdk/registry` | `NewQueueRegistry`, `AddQueue`, `BindQueueTags(name, tags, prefix)`, `ResolveQueue`; `NewSsmParamRegistry` and `AddParameter`. |
| `cdk/ssmexports` | `IncludeARNs()` option for `WithSSMExports`. |

## Coordinated HA: `GoBridgeDynamoDBHA`

`GoBridgeDynamoDBHA` is separate from `GoBridgeCluster`. It is the supported
single-region coordinated active/warm-standby profile. It reuses
`constructs/internal/gobridgebase.New` for both task definitions; it does not
copy or fork the base implementation.

### Required bridge configuration

The shared config must pass these synth-time checks:

- `bridge.deployment_mode: clustered`;
- no static `bridge.cluster.endpoints` entry, because each task registers the
  endpoint discovered by the existing ECS task-metadata resolver;
- DynamoDB `stores.lease`, `stores.outbox`, and
  `stores.managed_subscriptions` entries;
- at least one MQTT `session_mode: exclusive` session with one stable non-empty
  `client_id` and no `client_id_suffix`;
- one `ManagedSubscriptionBaselines` entry for every Exclusive MQTT session;
- a lease-managed route using `delivery_mode: shared_outbox` and
  `policy.ack_after: outbox_persist`;
- explicit `failover_slo` and `startup_allowance` on every coordinated route;
- one common `failover_slo` for the profile alarm threshold;
- an explicit `broker_health_step_down` on every coordinated route -- a positive
  duration, or `off` to record that this deployment accepts an unbounded
  node-local broker outage. A declared objective that leaves the decision unmade
  would silently exclude that failure mode.

The facade runs the builder admission path at synth time with a nil SDK client,
so validation performs no AWS calls. It admits the objective against BOTH
failure modes -- owner death and, when `broker_health_step_down` is enabled, the
node-local broker path. The owner-death budget is:

```text
lease_ttl
+ 2 * max(1ms, ceil(1.25 * acquire_poll_interval))
+ (1 + ceil(lease_ttl / min_jittered_poll)) * renew_call_timeout
+ complete post-takeover transport activation
+ startup_allowance
<= failover_slo
```

The broker-path budget, and both shipped lease profiles evaluated at their
defaults, are in [Failover budget](../../docs/failover-budget.md).

The profile forces bootstrap topology `dynamodb_coordinated_ha`, enables the
CloudWatch exporter, stamps the admitted canonical config fingerprint plus exact
three table identities into deployment-owned bootstrap, and leaves the exporter instance ID empty so each task
derives a unique task-local metric identity. This does not alter the MQTT
identity: every warm standby must use the same stable Exclusive MQTT
`client_id`, broker-session settings, and managed-subscription storage
identity. Per-task MQTT suffixes are rejected because they would strand the
failed holder broker queue. On every process initial apply, bootstrap compares
the EFS-loaded config with those deployment-owned identities and fingerprint
before it plans any store or transport. Existing target config cannot bypass
synth admission.

### Data tables

The facade creates exactly three encrypted on-demand tables. Names come from
the actual store configs; omitted `table_name` fields resolve through the
adapter defaults. Overrides must be literal resolved physical names. Unresolved
CDK tokens are rejected because token markers cannot be substituted inside the
embedded config document.

| Store | Default name | Primary key | Required indexes | TTL |
|---|---|---|---|---|
| Lease | `gobridge-leases` | `PK` (S) | none | **Disabled and must remain disabled.** The row is the permanent monotonic fencing counter. |
| Shared outbox | `gobridge-outbox` | `PK` (S), `SK` (S) | `ExpiryIndex` (`has_expiry`, `expires_at`, KEYS_ONLY), `RecordIDIndex` (`record_id`, KEYS_ONLY), `ClaimIndex` (`PK`, `claim_sort`, ALL) | Enabled on `ttl` for terminal records and abandoned fence metadata; pending work has no TTL. |
| Managed subscriptions | `gobridge-managed-subscriptions` | `storage_identity` (S) | none | Disabled. |

All three tables use `PAY_PER_REQUEST`, DynamoDB-managed encryption, point-in-time
recovery, deletion protection, and CloudFormation `RETAIN`. The facade exposes
table objects, names, and ARNs only through `bridge.Data()`
(`DynamoDBHAData`).

Before either ECS service can start, the facade initializes one managed-
subscription baseline row for each Exclusive MQTT session. Declare the
broker's complete known historical filter set by session ID:

```go
ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), &ha.DynamoDBHAProps{
    // ...
    ManagedSubscriptionBaselines: map[string][]string{
        "mqtt-ha": {"orders/legacy/#"},
    },
})
```

An explicit empty slice is an attestation that the stable broker identity is
new and has no historical subscriptions. Never use it for an existing broker
session merely because its history is unknown; reset to a genuinely new stable
client identity first. Missing entries and entries for unknown or unmanaged
sessions fail synthesis.

The facade derives the same opaque `storage_identity` as the Paho adapter and
uses a create-only custom resource to set `baseline=true` and union any declared
filters after validating MQTT wildcard and shared-subscription syntax. A durable
identity change creates a new initializer; changing only the declared filters
does not. Updates and deletes intentionally perform no write, so a later stack
update cannot resurrect a filter that the runtime removed. Each initializer
hides request data in logs, has only `dynamodb:UpdateItem` on the exact managed-
subscriptions table, and is a dependency of both ECS services.

On-demand mode removes capacity-unit forecasting, not capacity engineering.
Watch throttles and system errors. A single hot Exclusive session concentrates
outbox traffic under one `SESSION#...` partition; split workload across
independent session IDs before that partition approaches DynamoDB limits. The
sparse `ExpiryIndex` currently uses the adapter `has_expiry = 1` access pattern;
expiry-heavy workloads must follow the adapter sharding guidance before they
saturate that index partition. Keep `ClaimIndex`: omitting it forces the
correct but O(backlog) scan fallback.

### Compute and Availability Zones

The facade provisions one control service task and a worker service with a
minimum desired count of two. Every task participates in lease acquisition;
`control` only identifies the EFS config writer. Therefore a three-task steady
state has one active holder and at least two warm candidates. Worker counts
below two are rejected. `WorkerDesiredCount` must be a resolved finite integral
number at least two; unresolved CDK tokens fail because synth cannot prove the
warm-standby invariant. Selected private subnets must span at least two
Availability Zones. Both services use a 0/100 deployment with AZ rebalancing
disabled: the single RW control service so two config writers never overlap, and
the worker service so an incompatible revision never runs as a second cohort
beside the one it replaces. The worker service is deployed after the control
service. Workers remain read-only and idle until valid config is available.
The costs are an ingress gap for the duration of every deploy, AZ spread that is
best-effort at launch instead of continuously rebalanced, and a warm-standby
alarm that breaches for the length of each deploy.

This is single-region HA. It is not cross-region disaster recovery and does not
remove MQTT, DynamoDB, VPC, or regional failure domains.

### IAM

Both control and worker task roles need the same coordination operations because
either can become active. Grants are scoped to exact table and required index
ARNs:

| Store | Allowed task-role actions |
|---|---|
| Lease | `GetItem`, `PutItem`, `UpdateItem`, `DescribeTable`, `DescribeTimeToLive` |
| Outbox table | `GetItem`, `PutItem`, `UpdateItem`, `Query`, `TransactWriteItems`, `DescribeTable` |
| Outbox indexes | `Query` on the three exact index ARNs |
| Managed subscriptions | `GetItem`, `UpdateItem`, `DescribeTable` |

Task roles never receive `CreateTable`, `UpdateTable`, `DeleteTable`,
`UpdateTimeToLive`, wildcard DynamoDB actions, or wildcard index ARNs. SQS,
SSM, EFS, logs, and metric publishing remain derived by the existing grants
helpers. The external credentialed proof principal, not either task role, needs
`cloudwatch:PutMetricData` and read access for its proof query.

### Alarms and measured failure-to-Full

Compose `GoBridgeAlarms` with `AlarmsProps.DynamoDBHA`. The HA bundle covers:

- desired/running task count and loss of the minimum warm standby;
- throttle and system-error metrics for all three DynamoDB tables;
- `LeaseExpiries` and lease-transfer flapping;
- `OutboxDepth`, `OutboxDrainLatency`, depth-query failures, record failures,
  and stalled drains;
- DLQ depth, entries, and write failures;
- `FailureToFullDuration` against the declared `failover_slo`.

`FailureToFullDuration` is not emitted by the runtime. A dead holder cannot
measure its own outage. The credentialed external probe verifies the exact lease
owner, maps its advertised ECS endpoint to one exact task, takes a conservative
timestamp before `StopTask`, waits for that task to be `STOPPED`, requires owner
and fencing version to change, and probes the different successor directly for
`ServiceLevelFull`. A sample is warm only when that successor task ARN appears in
the pre-failure running-standby snapshot; a replacement winner is cold. It then publishes one dimensionless millisecond sample in
the configured deployment metrics namespace. The alarm uses
`TreatMissingData=NOT_BREACHING`; release proof immediately queries CloudWatch
and fails unless the exact sample is present, so missing data cannot create a
false release pass. Operators need a scheduled external probe for continuing
SLO evidence.

The repository fixture is admitted with a **120-second configured objective**.
That is a conservative configuration ceiling, not a measured production claim.
No 30–60 second claim is made. Publish an achieved target only after enough warm
and cold samples from the target VPC, image, broker, and credential path support
the stated percentile. `OutboxDrainLatency` measures a drain cycle, not the age
of the oldest pending record; inspect the oldest record directly when triaging a
deep backlog.

### Credentialed proof

Use the [credentialed failover procedure](../../docs/aws-deployment/topologies.md#credentialed-failover-proof)
for prerequisites, environment variables, and the exact test command. It requires
a reachable broker, a multi-zone VPC, protected credentials, and direct monitor
access to the verified successor task. Missing CloudWatch samples fail the proof.

This credentialed proof is a mandatory **post-merge external production-
approval gate**. The source-tag workflow does not own a repository-specific AWS
account, protected environment, VPC, broker, or release role and therefore does
not run it. Image publication produces a release candidate, not production
approval. Record the failover samples and CloudWatch evidence before promoting
that candidate for production use.

## Authoring Bridge Configuration

Two paths converge on the sealed `gobridgecdk.BridgeConfigSource` consumed by all facades:

```go
// (a) Local YAML, parsed for validation and optional image embedding.
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

With `ImageFromGoBuild`, the facade embeds this parsed config through
`go:embed` and the fixed `initial-config.base64` file, not payload-bearing flags
or environment variables. No S3 config asset is produced. Name a version whose
release train published the profile modules, and whose profile binary links any
optional family the document uses.

Registry and ECR images are consumer-built and cannot be changed by CDK.
They need their own embedded initial document, an existing target, or operator
creation. The snippets use registry images; `BridgeConfig` still drives
validation and grants but does not overwrite the target.

## Quickstart

### Snippet 1 — `GoBridgeSingle`

Use these declarations inside your CDK stack with `vpc` and `cluster` supplied
by your app. The [quickstart](../../docs/scenarios/cdk/01-quickstart-default-vpc.md)
shows a complete entry point.

```go
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
        // Pin the digest from the release's gobridge-image-digest.txt asset.
        Image:            gobridgecdk.ImageFromRegistry("ghcr.io/mariotoffia/gobridge@sha256:<digest>"),
        Bootstrap:        infra.BootstrapConfig{ /* admin/monitor addrs, etc. */ },
        BridgeConfig:     gobridgecdk.BridgeYamlAsset("config/bridge.yaml"),
        QueueRegistry:    queues,
        SsmParamRegistry: params,
    })

```

For cluster composition and load-balancer attachment, use the
[multi-bridge example](../../docs/scenarios/cdk/05-multi-bridge-cluster.md).
For coordinated HA props and member slots, use the
[construct reference](../../docs/aws-deployment/cdk-constructs.md#dynamodbhaprops-selected).

> **No L3 wrapper.** This profile no longer ships an opinionated single-call stack or service construct — every consumer composes the L2 constructs above inside their own `awscdk.Stack`.

## Singleton Constraint

> ⚠️ **One `GoBridgeSingle`, one `GoBridgeCluster`, OR one `GoBridgeDynamoDBHA` per Stack tree.**
> A synth-time scope scan (see `cdk/constructs/internal/singleton`) panics if two facades share the enclosing `awscdk.Stack`. Cross-account / cross-stack deployments are the operator's responsibility — wire each bridge into its own stack and use `GoBridgeALBAttachment.WithSSMExports` + `gobridgecdk.LookupBridge` for cross-stack consumption.
>
> The bridge identity is taken from the deployed yaml's `bridge.name` field (validated against `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$` by Phase-1 tier-B validation). There is intentionally **no `Name` prop** on `SingleProps`, `ClusterProps`, or `DynamoDBHAProps`: a single source of truth avoids the deploy-vs-config drift class.

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

## Credentials and artifact visibility

Logical config may carry literal credentials or references. Neither the builder
nor Phase 1 runs `ScanForPlaintextSecrets`; consumers may call it explicitly.
Anyone with access to an image or binary can recover its
embedded document; build contexts, CDK assemblies, and caches also need suitable
access controls. Base64 does not conceal secrets.

SSM Parameter Store SecureString references use `pms://path/to/param` or
`pms:///path/to/param`; both normalize to `/path/to/param`. Register each
parameter for scoped read and applicable Key Management Service (KMS) decrypt
grants. References stay unresolved in the initial logical copy.

For embedded SQS config, use a stable physical `queue_name` or optional
`queue_tags` with `queue_name_prefix`; all embedded queue URLs are rejected.
Tag-selected bindings use `sqs:queue`; CDK retains handles for grants. See
[queue APIs and examples](../../docs/aws-deployment/cdk-constructs.md#sqs-references).

## What It Provisions

**`GoBridgeSingle`**

- One ECS Fargate service, `DesiredCount=1`, deployment policy `MinHealthyPercent=0 / MaxHealthyPercent=100` (full drain before replace — eliminates concurrent EFS RW writers).
- One control EFS access point mounted RW.
- ECS cluster auto-created when `Cluster` is nil; Container Insights on for the auto-created cluster.
- One bridge container; optional embedded initialization runs inside control.
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
> take over for each other. Coordinated failover is provided separately by `GoBridgeDynamoDBHA`; selecting that facade is an explicit topology change, not an upgrade of `GoBridgeCluster`. See the `GoBridgeCluster` type doc
> (`cdk/constructs/gobridgecluster/cluster.go`) for the full advisory.

- All of the above, plus:
- Worker ECS Fargate service (`WorkerDesiredCount` default `2`, standard rolling deploy, optional CPU target-tracking via `AutoScalingProps`).
- Shared EFS file system with **two** access points (control RW, worker RO); RW/RO split enforced at IAM + ECS volume level.
- Workers read current config and never initialize or overwrite it. Missing
  config leaves their data planes idle. Clustered changes follow the
  [rollout rules](../../docs/adr/0012-cluster-config-whole-cohort-replacement.md),
  not an image-level drift mode.
- Separate task SGs (`ControlSecurityGroup`, `WorkerSecurityGroup`) both granted EFS ingress.

**`GoBridgeEfsConfig`** — created automatically by any facade when `EfsConfig` is nil; can be passed in to share one filesystem across facades (within the singleton-per-stack rule) or to override KMS / throughput / removal policy / backup.

## Constraints

- **Singleton**: see above.
- **Topology = `filesystem_replicated` (SCALE-OUT, not HA failover)** rejects routes that need cross-instance write coordination (`shared_outbox`, `route.session` lease). Those routes require a distributed lease/outbox store (e.g. DynamoDB) that the file-based EFS profile does not provision — remove them from `bridge.yaml`, or provision your own DynamoDB-backed lease/outbox store. `GoBridgeCluster` **forces** `filesystem_replicated` on both task definitions regardless of the caller's `Bootstrap.Topology`, so these guards always fire for a multi-instance cluster. Consequence: replicas are independent readers of the same config — there is **no single-active lease owner, no coordinated active/standby failover, and no 30–60s failover SLO**. Use the separate `GoBridgeDynamoDBHA` facade for coordinated active/warm-standby failover.
- **`SSMEndpoint`** set without `DevMode = true` fails Bootstrap validation (production-bypass guard).
- Bootstrap env payload capped at 1 MiB.
- **MQTT ingress memory:** the CDK base stamps the actual Fargate task memory
  into bootstrap. Runtime reserves 25% for consumed MQTT ingress sessions,
  divides it equally by session, and derives each default Receive Maximum with
  the adapter's byte model. `reserved_memory_bytes` plus this reservation must
  leave at least 20% task headroom. Every Persistent/Exclusive session referenced
  by a declared sender consumes a share even when no route references that
  sender, with route concurrency zero, because the session is still built and
  resumed durable state can deliver stale backlog. Ephemeral sender-only sessions
  consume no share. Impossible or explicitly unsafe profiles fail startup/reload.
- `GoBridgeALBAttachment` reserves listener-rule priorities `[BasePriority, BasePriority+99]`. Add the attachment last on a listener, or pick a `BasePriority` outside any consumer-managed range.
- `ControlAbsence` / `WorkerDegraded` alarms read Container Insights metrics: when passing your own `Cluster`, enable Container Insights yourself.

## Runtime Library

`lib/bootstrap.NewApp(cfg, opts...)` loads `BootstrapConfig` from env (`GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` or `…_FILE`, max 1 MiB), watches the selected config source (`file` on EFS or `dynamodb`), reloads bridge config without restart, resolves `pms://` SSM secrets, applies the MQTT ingress memory profile on every initial load/reload, and starts the admin / monitor / transport HTTP servers plus a `bridge.Runtime` with the `mqtt`, `sqs`, `http` transports and `memory`, `sqlite`, `dynamodb` stores. Clustered configs also register the existing ECS task-metadata endpoint resolver. Reload uses `swapModeOverlap` by default; `swapModePrepareCommit` when any transport advertises `CapExclusiveIdentity`.

The file source keeps polling and the control-only single-writer guard. The
DynamoDB source uses one loader for loading, watching and CAS-safe control
writes; `config_dynamodb` selects its table and polling or streams mode.
With valid bootstrap, the control plane starts live but not ready while the
data plane waits for valid config. Optional initialization uses
`ports.ConfigInitializer.CreateIfAbsent` only after confirmed document absence,
creates version 1, and rereads the winner.

After activation, confirmed absence stops new intake. Standalone runtimes
drain, release resources, and return to idle. Clustered runtimes, or uncertain
teardown, require process exit and replacement. Read failures retain the last
successful config as degraded. Same-process idle never rearms initialization;
a fresh process may initialize again. Backend-table absence is an error.
Only `DevMode` creates config tables at runtime; CDK owns production tables.
Authenticated `POST /api/v1/admin/config` creates absent config; see
[initial creation and outcomes](../../docs/aws-deployment/config-initialization.md#operator-creation-and-rollout).

Options: `WithLogger`, `WithLogLevelVar`, `WithParameterResolver`, `WithCredentialStore`, `WithDynamoDBClient`, `WithShutdownTimeout`, `WithTerminalPollInterval`. Binary: `lib/cmd/gobridge-filebased`.

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
Both base images are pinned by top-level multi-platform OCI index digest
(`linux/amd64` + `linux/arm64`); refresh them only through a reviewed change per
[DEVELOPMENT.md](../../DEVELOPMENT.md) (Base image digests). A source rebuild is
reproducible only to the extent the pinned bases, the locked module `go.sum`, and
the Go toolchain are fixed — nothing here claims bit-for-bit reproducibility
beyond those. Published **by digest** to `ghcr.io/mariotoffia/gobridge` by the
release workflow on stable `cmd/gobridge/vX.Y.Z` tags; the digest is the
`gobridge-image-digest.txt` asset of that release and the only version-to-image
association ([RELEASE.md](../../RELEASE.md#image-publication)).

## Integration Tests

Opt-in end-to-end tests live under `cdk/integration/`, all guarded by `//go:build integration_aws`. They are excluded from the default `go test ./...` run.

```bash
make integration-aws   # cd cdk && go test -tags=integration_aws -count=1 -timeout=45m ./integration/...
```

Required: AWS account/region settings, live credentials, Docker, and `cdk`.
Set `GOBRIDGE_INT_VERSION` to a profile `lib` version from a published train.
No released train has published `lib` yet.
Each fixture uses `ImageFromGoBuild` to embed its own config.

## Related Docs

| Doc | Scope |
|-----|-------|
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Module-internal architecture, DDD mapping, runtime/CDK layering. |
| [UBIQUITOUS.md](./UBIQUITOUS.md) | Terms unique to this deployment profile. |
| [docs/aws-deployment/overview.md](../../docs/aws-deployment/overview.md) | End-to-end AWS architecture, and the page map to topologies, storage, image, CDK constructs, and IAM. |
| [docs/aws-deployment/configuration.md](../../docs/aws-deployment/configuration.md) | Full bootstrap + bridge config reference. |
| [docs/aws-deployment/tco.md](../../docs/aws-deployment/tco.md) | Cost analysis with worked use cases. |
| [docs/scenarios/cdk/](../../docs/scenarios/cdk/) | Default-VPC, custom-VPC, API GW, production, multi-bridge cluster. |
