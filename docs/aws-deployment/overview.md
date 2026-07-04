# AWS Deployment Overview

GoBridge runs on AWS as an ECS Fargate service with EFS for configuration and
SSM Parameter Store for secrets. This guide covers the architecture, design
decisions, and the CDK construct library that wires everything together.

For generic deployment considerations, see [Deployment Guide](../deployment-guide.md).
For configuration details specific to AWS, see [Configuration on AWS](configuration.md).

---

## Architecture

The diagram below shows the full AWS architecture for a file-based GoBridge
deployment. Every component is created or referenced by the CDK constructs
described later in this guide.

```mermaid
flowchart TD
    subgraph VPC["VPC (private subnets)"]
        subgraph ECS["ECS Fargate"]
            T1[Task 1\ngobridge-filebased]
            T2[Task N\ngobridge-filebased]
        end

        EFS[(EFS\nbridge.yaml)]
        ALB[Application\nLoad Balancer]
    end

    ECR[ECR\nContainer Registry] --> ECS
    SSM[SSM Parameter Store\nSecureString secrets] --> ECS
    CW[CloudWatch Logs\n& Metrics] --- ECS

    ALB --> T1
    ALB --> T2
    T1 -- NFS mount --> EFS
    T2 -- NFS mount --> EFS

    Client([External Clients]) --> ALB

    style EFS fill:#f5a623,stroke:#333,color:#000
    style SSM fill:#4a90d9,stroke:#333,color:#fff
    style ECR fill:#4a90d9,stroke:#333,color:#fff
    style CW fill:#4a90d9,stroke:#333,color:#fff
```

| Component | Role |
|-----------|------|
| **ECR** | Stores the `gobridge-filebased` container image. |
| **VPC** | Isolates the Fargate tasks in private subnets with NAT egress. |
| **ECS Fargate** | Runs the bridge container without EC2 instance management. |
| **EFS** | Provides a shared, hot-reloadable config file mount across all replicas. |
| **ALB** | Terminates TLS and routes HTTP traffic to the admin, monitor, or transport ports. |
| **SSM Parameter Store** | Holds API keys and credentials as `SecureString` parameters. |
| **CloudWatch** | Collects structured logs and optional custom metrics. |

---

## Why ECS Fargate

Fargate is the recommended compute platform for GoBridge because it removes
the operational overhead of managing EC2 instances, AMI patches, and cluster
bin-packing. You get:

- **Serverless containers** -- no EC2 instances to provision or scale.
- **Per-second billing** -- pay only while tasks are running.
- **Fargate Spot** -- up to 70% cost reduction for non-critical or
  development workloads that tolerate interruption.
- **Built-in integration** with EFS, ALB, CloudWatch, and IAM.

### Sizing Guidance

The table below provides starting points. We recommend load-testing with your
actual message shapes and processor chains before finalizing.

| Throughput | CPU (units) | Memory (MiB) | Fargate Spot? |
|------------|-------------|---------------|---------------|
| < 100 msg/s | 256 | 512 | Yes |
| 100 -- 1 000 msg/s | 512 | 1024 | Evaluate |
| > 1 000 msg/s | 1024 | 2048 | No |

The CDK facades default to **512 CPU / 1024 MiB**. The single-task profile
(`GoBridgeSingle`) runs exactly one task and has no auto-scaling. The clustered
profile (`GoBridgeCluster`) runs one control task plus `WorkerDesiredCount`
workers (default 2); worker CPU auto-scaling is opt-in by setting the
`AutoScaling` prop (`AutoScalingProps{Min, Max, TargetCPU}`, where `TargetCPU`
`0` is treated as 70). Override sizing via the `CPU` and `MemoryMiB` props.

---

## EFS for Configuration

### Why EFS Over Alternatives

| Alternative | Limitation |
|-------------|-----------|
| **Environment variables** | 4 KiB per variable, 32 KiB total. Bridge configs routinely exceed this. |
| **S3 + init container** | Config changes require a task restart; no hot-reload. |
| **EFS** | Shared POSIX filesystem. Poll watcher detects changes within seconds; no restart required. |

GoBridge uses a **poll watcher** (default interval: 1 second) to detect config
file changes on the mounted EFS volume. When the file changes, the runtime
reloads routes, processors, and transports without dropping in-flight messages.

### Access Point Design

The CDK `GoBridgeEfsConfig` construct creates an EFS access point with these
defaults:

| Setting | Default | Source |
|---------|---------|--------|
| POSIX UID | `1000` | `GoBridgeEfsConfigProps.PosixUID` |
| POSIX GID | `1000` | `GoBridgeEfsConfigProps.PosixGID` |
| Access point path | `/gobridge` | `GoBridgeEfsConfigProps.AccessPointPath` |
| Directory permissions | `755` | Set in `CreateAcl` |

The EFS access point enforces this POSIX identity for every file operation
regardless of the container's process UID, so the image need not run as UID
1000 — the production image runs as the distroless nonroot user (65532:65532)
and still reads and writes `bridge.yaml` through the access point.

### Mount Path vs. Access Point Path

These two paths serve different purposes:

- **`AccessPointPath`** (`/gobridge` by default) -- the directory *inside* the
  EFS filesystem where config files live. This is set when the access point is
  created and is fixed for the life of the filesystem.
- **`MountPath`** (`/var/lib/gobridge` by default, `infra.DefaultMountPath`) --
  the directory *inside the container* where EFS is mounted. The container sees
  `/var/lib/gobridge/bridge.yaml`, but EFS stores it at `/gobridge/bridge.yaml`.

The `BootstrapConfig.ConfigFilePath` should reference the container mount path,
for example `/var/lib/gobridge/bridge.yaml`.

### File Size Limit

The file-based config loader enforces a **4 MiB** maximum file size. This is
more than enough for even the most complex multi-route configurations.

---

## SSM Parameter Store for Secrets

GoBridge resolves API keys and credentials from AWS Systems Manager Parameter
Store at startup. All parameters should be stored as **SecureString** type,
which encrypts values at rest using KMS.

### Credential URI Mapping

The `BootstrapConfig` maps logical names to SSM parameter paths. At startup
the bootstrap library resolves each parameter and injects the plaintext value
into the runtime configuration.

| Bootstrap field | Example SSM parameter | Runtime use |
|----------------|----------------------|-------------|
| `AdminAPIKeyParam` | `/gobridge/admin-key` | Admin HTTP API `X-API-Key` header |
| `MonitorAPIKeyParam` | `/gobridge/monitor-key` | Monitor HTTP API `X-API-Key` header |
| `HTTPReceiverAPIKeyParams["rx-1"]` | `/gobridge/rx-1-key` | HTTP receiver authentication |
| `HTTPSenderAPIKeyParams["tx-1"]` | `/gobridge/tx-1-key` | HTTP sender authentication |

`AdminAPIKeyParam` is **required**; the bootstrap validator rejects configs
without it. All other parameters are optional.

The `AdminAPIKeyParam` value is either a single key (folded under the name
`admin`) or a JSON object of named keys (`{"alice":"<key>","bob":"<key>"}`).
Named keys attribute each admin action to the operator's key name in the audit
log; a plain string keeps the legacy single-key behaviour. See
[configuration](configuration.md#admin-key-parameter-value).

### KMS Encryption

The default AWS-managed SSM key (`alias/aws/ssm`) works without additional
IAM configuration. If you use a **customer-managed KMS key (CMK)**, you must
add an explicit `kms:Decrypt` grant for the ECS task role on that key.

### DynamoDB Stores

When the bridge config uses DynamoDB-backed stores (`lease`, `outbox`, or
`dlq`), the stack grants the ECS task role read/write access on each table the
store names. Two operator responsibilities follow:

- **Pre-provision the tables.** The stack imports each table by name and grants
  access to it; it does **not** create the table, its key schema, or its TTL.
  Provision each DynamoDB table out-of-band (matching the adapter's expected key
  schema) before deploying.
- **Set `table_name` on every store.** A store that omits `table_name` falls
  back to the adapter's built-in default table, which the stack cannot name and
  therefore cannot grant — the task role would hit `AccessDenied` at runtime.
  Synth emits a warning for this case; set `table_name`, or grant the default
  table to the task role externally.

### DevMode Guard

The `BootstrapConfig.SSMEndpoint` field allows overriding the SSM endpoint for
local development against tools like LocalStack. To prevent accidental use of
custom endpoints in production, the bootstrap validator **rejects**
`SSMEndpoint` unless `DevMode` is explicitly set to `true`:

```go
// This passes validation -- DevMode enables the custom endpoint.
bootstrap := infra.BootstrapConfig{
    SSMEndpoint: "http://localhost:4566",
    DevMode:     true,
}

// This FAILS validation -- SSMEndpoint without DevMode is rejected.
bootstrap := infra.BootstrapConfig{
    SSMEndpoint: "http://localhost:4566",
}
```

When `DevMode` is `true` and `SSMEndpoint` is set, the bootstrap library
automatically configures static test credentials (`test`/`test`) so that
LocalStack access works without real AWS credentials.

---

## Runtime Metrics

The bootstrap config selects the runtime metrics backend. The loader reads
`BootstrapConfig` from `GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` (or a file named by
`GOBRIDGE_FILEBASED_BOOTSTRAP_FILE`) as **JSON** — it is not YAML.

| Bootstrap key | Values / default | Effect |
|---------------|------------------|--------|
| `metrics_exporter` | `""` / `"noop"` (default) / `"cloudwatch"` | `""`/`noop` emits nothing; `cloudwatch` publishes runtime metrics via the CloudWatch exporter. Any other value fails validation. |
| `metrics_namespace` | default `GoBridge/Runtime` | CloudWatch namespace used when `metrics_exporter=cloudwatch`. |
| `instance_id` | default empty | Stamps the `instance_id` metric dimension. Empty lets the exporter derive a per-task `<hostname>-<pid>`. |

The CDK base grants `cloudwatch:PutMetricData` **only** when
`metrics_exporter=cloudwatch`, scoped by the `cloudwatch:namespace` condition to
the effective namespace. A `noop` deployment gets no CloudWatch permissions.
`PutMetricData` has no resource-level restriction, so the namespace condition
must match the exporter's namespace or every publish is denied. See
[Monitoring and Observability](monitoring.md) for the exporter and alarm detail.

---

## Container Image

### Production Dockerfile

The repository ships a multi-stage `Dockerfile` at the root that builds the
`gobridge-filebased` binary as a static, **CGO-free** executable — the SQLite
store uses `modernc.org/sqlite`, which is pure Go, so there is no cgo and no
`CGO_ENABLED=1` — and ships it on `distroless/static-debian12:nonroot`:

```dockerfile
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod
RUN cd deployment/aws-filebased-config/lib && \
    go build -trimpath -ldflags="-s -w" \
      -o /out/gobridge-filebased ./cmd/gobridge-filebased

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/gobridge-filebased /usr/local/bin/gobridge-filebased
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/gobridge-filebased", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/gobridge-filebased"]
```

Build from the repository root — the binary module resolves the rest of
GoBridge through relative `replace` directives (`docker build -t
gobridge-filebased:latest .`).

Key points:

- The **EFS access point** enforces the POSIX file identity, so the container
  runs as the distroless nonroot user (65532), not UID 1000.
- **CA certificates** ship in the distroless base for TLS to AWS services.
- The image has **no shell, curl, or wget**, so the health check reuses the
  binary's `-healthcheck` flag (which probes the local monitor `/live`
  endpoint) instead of an HTTP client.

### ECR Lifecycle Policy

We recommend keeping the **last 10 tagged images** and expiring untagged
images after 1 day. This prevents unbounded storage growth while retaining
enough history for rollbacks.

```json
{
  "rules": [
    {
      "rulePriority": 1,
      "description": "Expire untagged images after 1 day",
      "selection": {
        "tagStatus": "untagged",
        "countType": "sinceImagePushed",
        "countUnit": "days",
        "countNumber": 1
      },
      "action": { "type": "expire" }
    },
    {
      "rulePriority": 2,
      "description": "Keep last 10 tagged images",
      "selection": {
        "tagStatus": "tagged",
        "tagPrefixList": ["v"],
        "countType": "imageCountMoreThan",
        "countNumber": 10
      },
      "action": { "type": "expire" }
    }
  ]
}
```

---

## CDK Construct Library

The GoBridge CDK constructs are written in Go and live at:

```text
github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs
```

The shared infrastructure types (zero external dependencies) live at:

```text
github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra
```

### Construct Overview

The library provides an EFS config construct plus two façade constructs, each
deploying a complete profile:

| Construct | Package | Purpose |
|-----------|---------|---------|
| `GoBridgeEfsConfig` | `cdk/constructs` | EFS filesystem + access point for config mounting. |
| `GoBridgeSingle` | `cdk/constructs/gobridgesingle` | One control Fargate task, RW EFS mount, no worker, no clustering. |
| `GoBridgeCluster` | `cdk/constructs/gobridgecluster` | One control task (RW EFS) plus `WorkerDesiredCount` worker tasks (RO EFS). |

The constructors are `NewGoBridgeSingle(scope, id, *SingleProps)` and
`NewGoBridgeCluster(scope, id, *ClusterProps)`. There is no `GoBridgeService` or
`GoBridgeStack` construct and no `GoBridgeServiceProps` type.

### GoBridgeEfsConfigProps

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Vpc` | `awsec2.IVpc` | *required* | VPC for EFS mount targets. |
| `FileSystem` | `awsefs.IFileSystem` | new filesystem | Existing EFS filesystem to reuse. |
| `AccessPointPath` | `*string` | `/gobridge` | POSIX path inside EFS. |
| `PosixUID` | `*string` | `"1000"` | POSIX user ID for the access points. |
| `PosixGID` | `*string` | `"1000"` | POSIX group ID for the access points. |
| `RemovalPolicy` | `interface{}` | `RETAIN` | What happens to the filesystem on stack deletion. |

### SingleProps (selected)

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

### ClusterProps (selected)

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

#### Worker seeder: AdoptValid vs AbortDeploy

Workers mount EFS read-only and cannot write config, so their default seeder
mode is **`AdoptValid`**: on startup a worker adopts whatever valid `bridge.yaml`
the EFS filesystem currently holds — whether written by the CDK seed or by an
Admin-API config-txn commit — and never fails on hash drift from the synth-time
asset. This lets the two reconfiguration paths coexist: a CDK redeploy and a
live admin edit both leave a valid file that scale-out and crash-replacement
workers pick up without a redeploy. Set `WorkerSeederMode` to **`AbortDeploy`**
for strict lock-step deployments where every worker must match the synth-time
asset exactly (an absent or mismatched file aborts the task).

### Usage Example

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

## IAM Least Privilege

Follow the principle of least privilege when configuring IAM roles. The CDK
constructs create scoped policies automatically, but if you manage IAM
manually, use these as a reference.

### Task Role

The task role is assumed by the running container. It needs access to EFS,
SSM, and any transport-specific services (e.g. SQS).

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EfsAccess",
      "Effect": "Allow",
      "Action": [
        "elasticfilesystem:ClientMount",
        "elasticfilesystem:ClientRead"
      ],
      "Resource": "arn:aws:elasticfilesystem:REGION:ACCOUNT:file-system/fs-XXXXXXXX",
      "Condition": {
        "StringEquals": {
          "elasticfilesystem:AccessPointArn":
            "arn:aws:elasticfilesystem:REGION:ACCOUNT:access-point/fsap-XXXXXXXX"
        }
      }
    },
    {
      "Sid": "SsmParameterAccess",
      "Effect": "Allow",
      "Action": "ssm:GetParameter",
      "Resource": [
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/admin-key",
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/monitor-key",
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/rx-*",
        "arn:aws:ssm:REGION:ACCOUNT:parameter/gobridge/tx-*"
      ]
    },
    {
      "Sid": "SqsAccess",
      "Effect": "Allow",
      "Action": [
        "sqs:SendMessage",
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:GetQueueAttributes"
      ],
      "Resource": "arn:aws:sqs:REGION:ACCOUNT:my-queue-*"
    }
  ]
}
```

The SQS statement is optional and should be scoped to the exact queue ARNs
your bridge routes reference. Omit it entirely if your deployment does not use
SQS transport.

### Execution Role

The execution role is used by the ECS agent to pull images and write logs.
It does not need access to application-level resources.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EcrPull",
      "Effect": "Allow",
      "Action": [
        "ecr:GetAuthorizationToken",
        "ecr:BatchGetImage",
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchCheckLayerAvailability"
      ],
      "Resource": "arn:aws:ecr:REGION:ACCOUNT:repository/gobridge"
    },
    {
      "Sid": "EcrAuth",
      "Effect": "Allow",
      "Action": "ecr:GetAuthorizationToken",
      "Resource": "*"
    },
    {
      "Sid": "CloudWatchLogs",
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:REGION:ACCOUNT:log-group:/ecs/gobridge-*:*"
    }
  ]
}
```

Note that `ecr:GetAuthorizationToken` requires `Resource: "*"` because the
authorization token is account-scoped, not repository-scoped.

---

## Related Guides

| Guide | Description |
|-------|-------------|
| [Configuration on AWS](configuration.md) | Bridge YAML reference with AWS-specific settings. |
| [Monitoring and Observability](monitoring.md) | CloudWatch metrics, structured logging, and alerting. |
| [HTTP API and Networking](http-api.md) | ALB target groups, security groups, and TLS termination. |
| [Total Cost of Ownership](tco.md) | Fargate, EFS, and SSM cost breakdown with worked examples. |
| [CDK Scenarios](../scenarios/cdk/) | Complete, runnable CDK deployment examples. |
| [Deployment Guide](../deployment-guide.md) | Platform-agnostic deployment considerations. |
