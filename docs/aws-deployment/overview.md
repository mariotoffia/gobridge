# AWS Deployment Overview

GoBridge runs on AWS as ECS Fargate services with EFS for configuration,
SSM Parameter Store for secrets, and an optional DynamoDB-coordinated HA profile. This guide covers the architecture, design
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

## Deployment Topologies

The CDK library deliberately exposes two different multi-task profiles:

| Facade | Coordination model | Intended use | Failover objective |
|---|---|---|---|
| `GoBridgeCluster` | `filesystem_replicated`; independent replicas read one EFS config | Scale independent routes horizontally | None. It has no active/standby takeover and no coordinated failover SLO. |
| `GoBridgeDynamoDBHA` | `dynamodb_coordinated_ha`; one lease holder plus warm standbys | Exclusive MQTT continuity with shared-outbox fencing | Explicit per-route `failover_slo`, admitted by Task 9 and measured externally. |

`GoBridgeCluster` remains unchanged. It rejects `route.session` and
`shared_outbox`; it must not be described as HA. `GoBridgeDynamoDBHA` deploys one
config-control task and at least two worker tasks across a subnet selection that
spans at least two Availability Zones. All three tasks participate in DynamoDB
lease acquisition, so the normal steady state has one active holder and at
least two warm candidates. Worker Availability Zone rebalancing is enabled;
the RW control service uses a 0/100 non-overlapping replacement with rebalancing
disabled.

### Coordinated HA data plane

`GoBridgeDynamoDBHA` creates exactly three encrypted, point-in-time-recoverable,
delete-protected, retained `PAY_PER_REQUEST` tables. The key/index shapes are
the adapter contracts, not deployment inventions:

| Table | Schema | TTL invariant |
|---|---|---|
| Lease (`gobridge-leases` default) | `PK` string hash key; no sort key or indexes | **Disabled.** The row carries the permanent monotonic fencing version. Deleting it can reset fencing and permit split brain. |
| Shared outbox (`gobridge-outbox` default) | `PK`/`SK`; `ExpiryIndex` KEYS_ONLY, `RecordIDIndex` KEYS_ONLY, `ClaimIndex` ALL | Enabled on `ttl` only for terminal records and old fence metadata. Pending work is never TTL-reaped. |
| Managed subscriptions (`gobridge-managed-subscriptions` default) | `storage_identity` string hash key | Disabled; exact MQTT filter history is durable. |

The data API is `DynamoDBHAData`, returned by `bridge.Data()`. It is the only HA
facade surface exposing table objects, names, and ARNs.

On-demand billing is appropriate for bursty takeover and outage recovery, but it
does not eliminate hot keys. A single Exclusive MQTT session concentrates the
outbox on one `SESSION#...` partition. Split unrelated workloads across session
IDs before that partition approaches DynamoDB limits. Preserve `ClaimIndex` to
avoid the adapter O(backlog) compatibility scan, and monitor the sparse
`ExpiryIndex` guidance for expiry-heavy traffic.

### Identity and endpoint rules

The shared bridge YAML must use `deployment_mode: clustered`, DynamoDB lease,
outbox, and managed-subscription stores, `delivery_mode: shared_outbox`,
`ack_after: outbox_persist`, and explicit `failover_slo` plus
`startup_allowance`. Every Exclusive MQTT standby uses the same broker domain,
`client_id`, clean-start/session-expiry behavior, and managed-subscription
storage identity. `client_id_suffix` is rejected for Exclusive sessions because
a per-task MQTT identity strands queued broker state after holder loss.

The facade also stamps the admitted canonical config fingerprint and exact table
identities into deployment-owned bootstrap. Every process validates the EFS
logical config against them before store or transport planning, so a stale or
tampered SeedOnce/AdoptValid file cannot bypass synth-time identity/route/table
admission.

Static `bridge.cluster.endpoints` are rejected by this profile. The bootstrap
registers the existing ECS metadata endpoint resolver and each holder writes its
own reachable endpoint into the lease row. This endpoint also lets the
credentialed proof map the lease to one exact ECS task without guessing.

### Least-privilege task roles

Either task role can become active, so control and workers receive the same
narrow data access:

- lease: `GetItem`, `PutItem`, `UpdateItem`, `DescribeTable`,
  `DescribeTimeToLive`;
- outbox table: `GetItem`, `PutItem`, `UpdateItem`, `Query`,
  `TransactWriteItems`, `DescribeTable`;
- exact outbox index ARNs: `Query` only;
- managed-subscription history: `GetItem`, `UpdateItem`, `DescribeTable`.

No task role receives DynamoDB table creation/update/deletion,
`UpdateTimeToLive`, wildcard actions, or wildcard index resources. The external
proof principal receives no grant from the facade; its operator policy must
separately allow the required ECS/DynamoDB reads and
`cloudwatch:PutMetricData`/metric query calls.

### Alarms and objective honesty

The HA form of `GoBridgeAlarms` covers running/desired task count, minimum warm
standby, all-table DynamoDB throttles/system errors, lease expiry and takeover flapping, shared-outbox depth/drain latency/failures, DLQ
signals, and `FailureToFullDuration`.

`FailureToFullDuration` is emitted by the credentialed external health/failover
probe, not by the runtime. The probe conservatively starts timing before the
verified holder `StopTask` request, waits for that exact task to be `STOPPED`,
requires both lease owner and fencing version to change, and waits for a
different exact successor to report `ServiceLevelFull`. A sample is classified
`warm` only when that successor task ARN was already running in the pre-failure
standby snapshot; a replacement winner is classified `cold`. It publishes one
no-dimension millisecond sample in the configured deployment namespace. The
alarm uses `TreatMissingData=NOT_BREACHING`; the release test immediately
queries CloudWatch for the exact sample and fails if it is absent. Continuous
SLO evidence therefore requires an operator-scheduled external probe.

The checked example/fixture objective is **120 seconds**. Admission proves only
that configured worst-case terms fit that ceiling. It does not prove an achieved
production percentile. No 30–60 second claim is made. Publish a tighter target
only after enough warm and cold samples from the actual image, VPC, broker,
credentials, and AWS account support it. `OutboxDrainLatency` is a drain-cycle
measurement, not direct oldest-record age; inspect the oldest pending item when
triaging backlog age.

### Credentialed failover proof

The test runner needs CDK deploy/destroy credentials, a two-AZ VPC, a reachable
TLS MQTT broker, existing SecureString admin/MQTT parameters, CloudWatch metric
write/read permission, and VPC routing to task private addresses. The fixture
opens monitor port 8081 only from `GOBRIDGE_INT_HA_PROBE_CIDR`; production
security groups remain unchanged.

Required variables:

```text
GOBRIDGE_INT_HA=1
GOBRIDGE_INT_AWS_ACCOUNT
GOBRIDGE_INT_AWS_REGION
GOBRIDGE_INT_VPC_ID
GOBRIDGE_INT_AVAILABILITY_ZONES
GOBRIDGE_INT_SUBNET_IDS
GOBRIDGE_INT_PUBLIC_SUBNET_IDS
GOBRIDGE_INT_IMAGE
GOBRIDGE_INT_HA_MQTT_BROKER_URL
GOBRIDGE_INT_HA_MQTT_CLIENT_ID
GOBRIDGE_INT_HA_MQTT_CREDENTIAL_PARAM
GOBRIDGE_INT_HA_ADMIN_PARAM
GOBRIDGE_INT_HA_PROBE_CIDR
```

The availability-zone, private-subnet, and public-subnet lists must have the
same order and cardinality. The harness imports these concrete attributes and
produces an assembly with no VPC lookup context.

Optional `GOBRIDGE_INT_HA_SAMPLES` controls separate warm/cold sample counts
(1–20, default 1). Run:

```bash
cd deployment/aws-filebased-config/cdk
GOBRIDGE_INT_HA=1 go test -count=1 -v -tags=integration_aws -run TestHA_FailoverStopsVerifiedLeaseholder ./integration
```

When `GOBRIDGE_INT_HA=1`, missing variables, credentials, outputs, network
reachability, owner/fence changes, Full readiness, or the exact CloudWatch sample
fail the test. Without that explicit request, the credentialed build-tag test is
skipped and no AWS deployment occurs.

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

These are per-task Fargate sizes (**1024 CPU units = 1 vCPU**). The authoritative
throughput-to-resource tiers and `max_in_flight` guidance live in the
[Deployment Guide — CPU and Memory Sizing](../deployment-guide.md#cpu-and-memory-sizing);
the table below maps those tiers to valid Fargate task sizes and Spot
suitability. Load-test with your actual message shapes and processor chains
before finalizing.

| Throughput | CPU (units) | vCPU | Memory (MiB) | Fargate Spot? |
|------------|-------------|------|--------------|---------------|
| < 100 msg/s | 256 | 0.25 | 512 | Yes |
| 100 -- 1 000 msg/s | 512 | 0.5 | 1024 | Evaluate |
| > 1 000 msg/s (per worker) | 1024 | 1.0 | 2048 | No |

For a single non-clustered task above 1 000 msg/s, size vertically to the
Deployment Guide's `High` tier (2--4 vCPU / 4--8 GiB) instead of adding workers.
The CDK facades default to **512 CPU / 1024 MiB**. The single-task profile
(`GoBridgeSingle`) runs exactly one task and has no auto-scaling. The independent
scale-out profile (`GoBridgeCluster`) runs one control task plus
`WorkerDesiredCount` workers (default 2); its worker CPU auto-scaling is opt-in
through `AutoScalingProps`. The coordinated profile (`GoBridgeDynamoDBHA`) runs
one control plus at least two workers and requires a resolved finite integral
worker count of at least two. Unresolved CDK numeric tokens are rejected because
they cannot prove warm capacity. Size every
warm task for the full takeover load. Override sizing with `CPU` and `MemoryMiB`.

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

For `GoBridgeSingle` and `GoBridgeCluster`, when the bridge config uses imported
DynamoDB-backed stores (`lease`, `outbox`, `dlq`, or `managed_subscriptions`),
the stack grants each ECS task role only the adapter-specific data operations
on each table and required index the store names. Two operator responsibilities follow:

- **Pre-provision only imported tables.** `GoBridgeSingle` and
  `GoBridgeCluster` import every configured DynamoDB store, so provision those
  tables out-of-band with the adapter schema before deploying. Do **not**
  pre-provision the three names owned by `GoBridgeDynamoDBHA`: that facade creates
  its mandatory lease, outbox, and managed-subscription tables and a same-name
  resource would collide. Only an optional HA DynamoDB DLQ remains
  operator-provisioned/imported.
- **Use a resolved physical `table_name` only to override a role default.** HA
  rejects unresolved CDK tokens because token strings cannot be substituted in
  the immutable config asset. When omitted, runtime
  preflight and the stack resolve and grant the same exact default:
  `gobridge-leases`, `gobridge-outbox`, `gobridge-dlq`, or
  `gobridge-managed-subscriptions`.

The adapter's expected key schemas -- what an out-of-band table must match, and
what the store's own `EnsureTable`/`CreateTable` helper provisions -- are below.
All four tables are created `PAY_PER_REQUEST` (on-demand).

**Lease table** (default `gobridge-leases`)

| Attribute | Type | Role |
|---|---|---|
| `PK` | `S` | Partition key |

No sort key and no GSIs. **DynamoDB TTL must be DISABLED** on this table: the
lease row is the fencing counter of record, and a TTL reaper that deletes it
resets the fencing version and opens a split-brain window. The lease preflight
enforces this (see [IAM Least Privilege](#iam-least-privilege) below).

**Outbox table** (default `gobridge-outbox`)

| Attribute | Type | Role |
|---|---|---|
| `PK` | `S` | Partition key |
| `SK` | `S` | Sort key |

| GSI | Key schema | Projection | Notes |
|---|---|---|---|
| `ExpiryIndex` | `has_expiry` (S) HASH, `expires_at` (N) RANGE | `KEYS_ONLY` | Sparse; drives expiry sweeps |
| `RecordIDIndex` | `record_id` (S) HASH | `KEYS_ONLY` | `Complete` record lookup |
| `ClaimIndex` | `PK` (S) HASH, `claim_sort` (S) RANGE | `ALL` | Sparse, age-ordered claim path; **optional** |

`ClaimIndex` is optional -- `Claim` falls back to a whole-partition scan when it
is absent, so an un-migrated table still boots. When it is present it **must** be
`Projection: ALL`: the claim query filters on the non-key `status` attribute, so
an under-projected index fails every claim at runtime. Preflight rejects a
present-but-under-projected `ClaimIndex` at startup, and the running store
degrades to the scan path if the index becomes unusable.

**DLQ table** (default `gobridge-dlq`)

| Attribute | Type | Role |
|---|---|---|
| `PK` | `S` | Partition key |

| GSI | Key schema | Projection | Notes |
|---|---|---|---|
| `RouteIndex` | `route_id` (S) HASH, `failed_at` (N) RANGE | `ALL` | List/purge by route |
| `CategoryIndex` | `category` (S) HASH, `failed_at` (N) RANGE | `ALL` | List/purge by category |

DLQ entries carry no TTL by default. Setting a `retention` window enables
DynamoDB TTL on the `ttl` attribute through the store's `EnsureTable` helper.

**Managed-subscriptions table** (default `gobridge-managed-subscriptions`)

| Attribute | Type | Role |
|---|---|---|
| `storage_identity` | `S` | Partition key |

No sort key and no GSIs. It stores one baseline and an exact filter String Set
per secret-safe durable MQTT identity.

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
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod
RUN cd deployment/aws-filebased-config/lib && \
    go build -trimpath -ldflags="-s -w" \
      -o /out/gobridge-filebased ./cmd/gobridge-filebased

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b AS runtime
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
- **Base images are pinned by digest.** Both `FROM` lines carry a top-level
  multi-platform OCI index digest (verified to include `linux/amd64` and
  `linux/arm64`), so a rebuild pulls the exact reviewed bytes rather than
  whatever the mutable tag points at. Refresh a digest only through a reviewed
  change — see [DEVELOPMENT.md](../../DEVELOPMENT.md) (Base image digests) for the
  resolve/verify commands. A source rebuild is reproducible only to the extent
  the pinned bases, the locked per-module `go.sum`, and the Go toolchain are
  fixed; nothing here claims bit-for-bit reproducibility beyond those facts.

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

### DynamoDBHAProps (selected)

`DynamoDBHAProps` shares the common VPC, image, bootstrap, config, registry,
sizing, and EFS fields. Its `WorkerDesiredCount` defaults to `2` and must be a
resolved finite integer greater than or equal to `2`; unresolved numeric tokens
are rejected. It has no worker auto-scaling surface. Table names and the
canonical config fingerprint are derived from the admitted bridge config and
injected into bootstrap by the facade, not supplied independently by callers.

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
SSM, any transport-specific services (e.g. SQS), and DynamoDB when you configure
DynamoDB lease/outbox/DLQ stores.

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
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueUrl",
        "sqs:GetQueueAttributes"
      ],
      "Resource": "arn:aws:sqs:REGION:ACCOUNT:my-queue-*"
    },
    {
      "Sid": "DynamoDbStoreAccess",
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem",
        "dynamodb:Query",
        "dynamodb:Scan",
        "dynamodb:TransactWriteItems",
        "dynamodb:DescribeTable",
        "dynamodb:DescribeTimeToLive"
      ],
      "Resource": [
        "arn:aws:dynamodb:REGION:ACCOUNT:table/gobridge-*",
        "arn:aws:dynamodb:REGION:ACCOUNT:table/gobridge-*/index/*"
      ]
    }
  ]
}
```

The SQS statement is optional and should be scoped to the exact queue ARNs
your bridge routes reference. Omit it entirely if your deployment does not use
SQS transport.

`sqs:ChangeMessageVisibility` backs the receiver's `auto_extend` (visibility
renewal at one-third of the timeout); a missing grant surfaces as `NOT_AUTHORIZED`
only after the first extension attempt, not at startup. `sqs:GetQueueUrl` backs
queue-name resolution -- a receiver or sender configured with `queue_name`
(rather than a full `queue_url`) resolves the canonical URL at build time. The
adapter does not call `GetQueueAttributes`; the action is retained here as a
harmless allowance for operators who inspect queues out-of-band, and can be
dropped from a least-privilege policy.

**DynamoDB stores.** The `DynamoDbStoreAccess` statement is needed only when a
store role is configured with `type: dynamodb`. Scope `Resource` to your actual
table ARNs -- the default names are `gobridge-leases`, `gobridge-outbox`, and
`gobridge-dlq` -- and keep the `/index/*` entry, which the outbox and DLQ queries
need for their GSIs. Omit the statement entirely for memory/SQLite-only
deployments. The data-plane actions each role uses, if you split the statement
per table for tighter least privilege:

| Role | Runtime data-plane actions |
|------|----------------------------|
| Lease | `GetItem`, `PutItem`, `UpdateItem` |
| Outbox | `GetItem`, `PutItem`, `UpdateItem`, `Query`, `TransactWriteItems` |
| DLQ | `GetItem`, `PutItem`, `DeleteItem`, `Query`, `Scan` |

Each store also runs a boot-time schema **preflight** that adds control-plane
actions on top of the data-plane set above:

- Outbox, DLQ, and managed-subscriptions additionally call `dynamodb:DescribeTable`.
- Lease additionally calls `dynamodb:DescribeTable` **and**
  `dynamodb:DescribeTimeToLive` -- it enforces that DynamoDB TTL is **disabled**
  on the fencing table, which a reaper would otherwise use to delete lease rows
  and reset the fencing version.

Preflight posture is **fail-closed** and matters for how you grant these actions:

- A **confirmed schema mismatch** -- the table exists but has the wrong key
  schema or is missing a required GSI -- is **fatal at boot**. The store refuses
  to start against a mis-shaped table (the guard against a copy-pasted table name
  silently shredding messages).
- A `DescribeTable` call that **cannot verify** the table -- the permission is
  missing (`AccessDenied`), the control plane throttles it during a mass rollout,
  or the backend does not implement `DescribeTable` -- is **also fatal at boot**.
  An unreadable table is not proof the table is valid, and an unreadable +
  mis-shaped table is the exact silent-shredder scenario the preflight exists to
  catch (the first record per partition writes, the rest ack-and-drop as
  "duplicates"). The store refuses to start.
- On the lease role, an **observed enabled (or enabling) DynamoDB TTL** on the
  fencing table is **fatal at boot**. A `DescribeTimeToLive` call that **cannot
  verify** the TTL state (missing `dynamodb:DescribeTimeToLive`, a throttle, or a
  backend that does not implement it) is **fatal for the same reason**: it proves
  nothing about the TTL state, and a TTL-reaped fence row is a split-brain hazard.
- The generated CDK task-role policy grants `dynamodb:DescribeTable` on every
  configured/default store table and additionally grants
  `dynamodb:DescribeTimeToLive` on the exact lease table. Both are therefore
  **required** for boot under the default posture,
  and TTL must stay disabled on the lease table.

The advisory opt-outs are **Go-code-level factory options, not config keys.**
`WithSchemaPreflightAdvisory()` downgrades an unverifiable `DescribeTable` to a
loud WARN-and-continue; `WithTTLPreflightAdvisory()` does the same for the lease
TTL check (both an observed enabled TTL and an unverifiable `DescribeTimeToLive`).
Neither relaxes a **confirmed** schema mismatch, which stays fatal. Use them only
for a dev/emulator that cannot serve these control-plane calls.

The shipped `aws-filebased-config` deployment builds the factory as
`NewDynamoDBStoreFactory(client)` with no options and exposes **no**
`schema_preflight_advisory` or `ttl_preflight_advisory` config key, so opting into
advisory mode requires code-level wiring in a custom composition root. The
DynamoDB Local (`ddblocal`) test emulator implements both `DescribeTable` and
`DescribeTimeToLive`, so tests and local development against it boot cleanly under
the default fail-closed posture -- only an emulator or backend that lacks these
control-plane calls needs the advisory opt-outs.

Table creation and TTL setup (`dynamodb:CreateTable`, `dynamodb:UpdateTimeToLive`)
are a deploy-time concern; the CDK constructs provision tables out-of-band. Grant
those two actions only if you let the bridge self-provision through its
`EnsureTable` helper. See the
[DynamoDB Store](../processors-and-stores.md#dynamodb-store) reference for store
behavior and [Monitoring](monitoring.md#key-metrics) for the backlog and
store-health signals.

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
