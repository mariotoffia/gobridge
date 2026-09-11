# CDK Construct Library

## Overview

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
| `GoBridgeSingle` | `cdk/constructs/gobridgesingle` | One control Fargate task, optional EFS mount, no worker, no clustering. |
| `GoBridgeCluster` | `cdk/constructs/gobridgecluster` | Independent filesystem scale-out: one control task plus workers. |
| `GoBridgeDynamoDBHA` | `cdk/constructs/gobridgedynamodbha` | DynamoDB-coordinated active/warm-standby: one control plus at least two workers and three owned tables. |

The constructors are `NewGoBridgeSingle(scope, id, *SingleProps)`,
`NewGoBridgeCluster(scope, id, *ClusterProps)`, and
`NewGoBridgeDynamoDBHA(scope, id, *DynamoDBHAProps)`. There is no `GoBridgeService` or
`GoBridgeStack` construct and no `GoBridgeServiceProps` type.

## Runtime image source

All three facades require `Image`, a sealed `gobridgecdk.BridgeImageSource`.
Existing `awsecs.ContainerImage` values are no longer accepted. Choose one of:

| Constructor | Use |
|-------------|-----|
| `gobridgecdk.ImageFromRegistry(ref)` | A registry reference pinned with `@sha256:<digest>`. The reference is preserved exactly; mutable tags alone fail synth. |
| `gobridgecdk.ImageFromEcrRepository(repo, tag)` | A consumer-managed `awsecr.IRepository` with an explicit tag or SHA-256 digest. CDK grants the execution role pull access. Prefer immutable tags or digests. |
| `gobridgecdk.ImageFromGoBuild(props)` | Builds a downloaded module copy with the facade's parsed `BridgeConfig` in its fixed embed file; uses `go install package@version` only without embedded config. No Git checkout. |

`ImageGoBuildProps.Version` is required: supply a published profile version,
not `main` or `latest`. The profile `lib` module rides the same release train
as `cdk` and `infra`, so the version you already use for
`go get .../cdk@vX.Y.Z` is the one to pass here. `Package` defaults to
`github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/cmd/gobridge-filebased`.
The embedded Dockerfile uses digest-pinned Go and distroless nonroot bases;
`GoImage` and `BaseImage` overrides must also be digest-pinned. Synth stages the
context into the cloud assembly before cleaning up its temporary directory,
including when app-wide asset staging is disabled. Docker builds and publishes
the asset during deployment, not synth.

Nil `BuildTags` invokes `DeriveBuildTags` on the parsed bridge config. AWS, MQTT,
native stores and HTTP need no extra tags; AMQP 0-9-1, AMQP 1.0 and Azure Service
Bus select `gobridge_amqp091`, `gobridge_amqp10` and `gobridge_azure`, including
their aliases. Unknown kinds and unmapped processor registrations fail synth.
An explicit empty slice (`[]string{}`) adds no tags and bypasses derivation.

`Platform` defaults to `linux/amd64`; `linux/arm64` is also supported and sets
both the Docker build platform and Fargate task architecture. Both base images
must support the selected platform. Registry and ECR
sources use `linux/amd64`.

**Version prerequisite:** pick a version from a train that publishes the profile
modules; the `v0.3.x` profile tags are not a complete set and no released train
has published `lib`
([RELEASE.md](../../RELEASE.md#canonical-release-graph)).

**Optional families.** The default `Package` always links AWS, MQTT, native
stores and HTTP. `lib` adds AMQP 0-9-1, AMQP 1.0 and Azure Service Bus through
tagged family files, so a derived or explicit `gobridge_amqp091`,
`gobridge_amqp10` or `gobridge_azure` tag links that transport; every `lib`
version a train publishes carries those files. Tags are not checked against the
config: an explicit `BuildTags` slice that omits a family the config uses still
builds, and the config then fails to decode at container startup. The startup
log names every kind the binary can decode.
A custom `Package` must implement this profile's bootstrap and health-check
contract, provide `initial-config.base64` consumed through `go:embed`, and
support the build's `-initial-config-digest` probe. The standard commands embed
that file in `main.initialConfigBase64` and decode it before initialization.

After compilation, the generated build runs
`/gobridge-filebased -initial-config-digest` and compares its output with the
SHA-256 hash of the staged document bytes. The probe exits before runtime or
network startup and reveals only the hash. A missing probe or mismatched hash
fails the build. A missing fixed embed file also fails; an older package cannot
silently omit the declared document.

The initial document is staged as `initial-config-<rawSHA>.base64`, containing
only Base64 data. The hash identifies the unencoded serialized document.
The config-bearing build downloads the requested package version with Go
tooling, locates its owning module, and copies it to a writable build directory.
It fills the command's fixed embed file in that copy, then runs `go build`
with small version/commit linker flags. The module cache stays unchanged.
Without embedded config, the build retains `go install package@version`.

Config bytes never enter the command arguments or child environment.
There is no payload-bearing `GOENV`/`GOFLAGS` path, separate S3 config asset,
or runtime download grant. See [initial configuration](config-initialization.md).

Registry and ECR images cannot be changed by CDK. They must contain their own
initial document, use an existing target, or wait for operator creation.
`BridgeConfig` still drives validation and grants; it does not overwrite the
target. Literal credentials may be embedded, but are readable in the binary,
image, and build artifacts. Base64 is not secrecy.

## Runtime config source

Use `Bootstrap.ConfigSource`; there is no separate config-source prop. Empty or
`file` selects EFS. `dynamodb` on Single or
DynamoDB HA creates one retained, on-demand, PITR-enabled config table and stamps
its name into bootstrap without mutating caller settings. Workers receive only
read access to that table; control receives read/write access. Set
`Bootstrap.ConfigDynamoDB.WatchMode` to `streams` to enable the table stream and
stream-read grants. The filesystem-replicated Cluster accepts only `file`.

`BridgeConfig` still supplies YAML for synth-time validation, port mappings and
adapter grants for either runtime source. A Go-built image embeds that logical
document for optional control-only creation when the target is absent. The
main process starts its control plane without waiting for another container.
See [strict creation](config-initialization.md#strict-creation).

EFS is needed only for file config or parsed SQLite paths. `EfsConfig()` returns
nil otherwise, including when an unused `EfsConfig` prop was supplied. Pass that
value unchanged to `GoBridgeAlarms`: the EFS alarm is omitted when no filesystem
exists. ALB SSM exports with `IncludeARNs()` omit `efs-id` for EFS-free facades;
the URL and cluster/ALB exports are unchanged. `LookupBridge(..., IncludeARNs())`
uses an optional synth-time SSM lookup for EFS presence and returns nil from
`EfsID()` until that lookup resolves, or when the producer has no filesystem.
When changing the producer between filesystem-backed and EFS-free config, refresh
the cached `efs-id` lookup in `cdk.context.json` after deploying the producer.

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
| `Image` | `gobridgecdk.BridgeImageSource` | *required* | Sealed runtime image from `ImageFromRegistry`, `ImageFromEcrRepository`, or `ImageFromGoBuild`. |
| `Bootstrap` | `infra.BootstrapConfig` | *required* | Runtime config; `NodeRole` forced to `control`. |
| `BridgeConfig` | `source.Source` | *required* | Sealed config from `gobridgecdk.BridgeYamlAsset`/`BridgeYamlInline`. |
| `QueueRegistry` | `*registry.QueueRegistry` | conditionally required | Resolves SQS queue names in the config. |
| `SsmParamRegistry` | `*registry.SsmParamRegistry` | conditionally required | Resolves SSM parameter URIs in the config. |
| `ManagedSubscriptionBaselines` | `map[string][]string` | required per durable subscribing session | For every persistent or exclusive MQTT session that subscribes, the exact filters its broker identity already holds; an empty list attests a new identity. Validated at synth, stamped into the bootstrap document as `managed_subscription_baselines`, and seeded by the runtime at every boot. See [SQLite stores on the config mount](storage-and-secrets.md#sqlite-stores-on-the-config-mount). |
| `CPU` | `*float64` | `512` | Fargate CPU units. |
| `MemoryMiB` | `*float64` | `1024` | Fargate memory (MiB). |
| `MountPath` | `*string` | `/var/lib/gobridge` | Container EFS mount path. |

The single profile runs exactly one task (`DesiredCount` is not a prop) and has
no auto-scaling.

## ClusterProps (selected)

`ClusterProps` shares `Vpc`, `Image`, `Bootstrap`, `BridgeConfig`, the
registries, `CPU`, `MemoryMiB`, and `MountPath` with `SingleProps` (applied to
both services), plus:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `WorkerDesiredCount` | `*float64` | `2` | Worker task count (must be ≥ 1). |
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
is the baseline config digest of the admitted document.

For both file and DynamoDB sources, that baseline uses
`bridge.DeploymentBaselineContentDigest`, excluding only the top-level version.
Initialization assigns target version 1 independently of the embedded version.
The actual committed artifact keeps its stored version and full digest.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `WorkerDesiredCount` | `*float64` | `2` | Interchangeable worker task count on the autoscaled shape. Rejected together with `MemberSlots`. |
| `MemberSlots` | `*MemberSlots` | `nil` | Opts into the static member-slot shape: `{ControlMemberID, WorkerMemberIDs}`. One single-task ECS service per id, each with its own task definition and `member_id`. Requires two to eight worker ids, ids matching `[A-Za-z0-9][A-Za-z0-9_-]*` (≤ 64 characters), `bridge.cluster.rollout: coordinated`, and a `bridge.cluster.members` roster naming exactly these ids. The upper bound is the CloudWatch metric-math input budget of the fleet warm-standby alarm. |

Only the static member-slot shape can host the coordinated cluster rollout
barrier; the autoscaled shape rejects `rollout: coordinated` and a non-empty
`bridge.cluster.members` at synth time. See
[Two worker shapes](topologies.md#two-worker-shapes).

With `MemberSlots` the facade also provisions the retained rollout coordination
table, grants each task role `dynamodb:GetItem` and `dynamodb:PutItem` on it, and
exposes `MemberSlotIDs()`,
`RolloutTableName()`, `WorkerServices()` and `WorkerTaskDefinitions()` alongside
the single-valued accessors.

### Worker configuration authority

Workers only read configuration. They never initialize or overwrite the target,
including when they use the same image as control. Missing config leaves the
data plane idle and not ready. After first activation, confirmed absence stops
new intake. An activated clustered worker exits for replacement rather than
returning to idle. Standalone operation can drain and release its runtime, then
rebuild after valid config returns. Uncertain teardown also requires exit.
Source read errors retain the last successful config as degraded.

Existing invalid config cannot be replaced by the embedded document. HA
deployment fingerprints and cluster-rollout admission still apply to existing
valid config. There are no seeder-mode or seeder-image props.

### SQS references

Embedded Amazon Simple Queue Service (SQS) config names a physical queue or
selects one by tags and an optional name prefix. A registry alias is not a
physical queue name. For example, alias `orders` may refer to physical queue
`orders-prod`; only `orders-prod` may appear as `queue_name`.

| API | Contract |
|---|---|
| `QueueRegistry.AddQueue(name, queue)` | Registers an `IQueue` under a logical alias for grants and dependencies. |
| `QueueRegistry.BindQueueTags(name, tags, prefix)` | Binds a literal selector to an already registered queue; returns an error for invalid or conflicting declarations. |
| `QueueRegistry.ResolveQueue(cfg sqs.Config)` | Returns `(QueueRef, error)` by matching the declared reference to registered handles, without AWS calls; ambiguous or missing name/tag mappings fail. |
| `QueueRef.PhysicalName()` | Returns the known physical name, never an alias or unresolved token; empty means the name is unknown at synth. |
| `QueueRef.QueueTags()` | Returns an isolated copy of the explicitly bound selector. |
| `QueueRef.QueueNamePrefix()` | Returns the optional discovery prefix bound to that selector. |

Register a queue before binding its selector:

```go
queue := awssqs.NewQueue(stack, jsii.String("Orders"), &awssqs.QueueProps{
    QueueName: jsii.String("orders-prod"),
})
queues := registry.NewQueueRegistry()
queues.AddQueue("orders", queue)
if err := queues.BindQueueTags("orders", map[string]string{
    "application": "gobridge",
    "purpose":     "orders",
}, "orders-"); err != nil {
    panic(err)
}

// Use this sender declaration in your bridgecfg builder chain.
builder := bridgecfg.New("orders-bridge").
    WithSQSSender("orders-out", queues.Ref("orders"))
```

Pass `queues` as the facade's `QueueRegistry`. `BindQueueTags` applies tags
to CDK-owned queues. For an imported queue, it asserts that the producer has
already applied them; CDK cannot change the imported resource's tags.
The prefix must match the deployed physical name. For generated names,
pass an empty prefix unless you can guarantee that match.

`WithSQSReceiver` and `WithSQSSender` prefer the bound tags. Without tags they
use `PhysicalName()`. A generated name that is unknown at synth needs an
explicit tag binding for embedding. `WithRoute` gives a tag-selected sender's
generated binding the address `sqs.QueueAddress`, whose wire value is
`sqs:queue`. It means “use this sender's configured queue,” not per-message
queue discovery.

The image builder calls `bridgecfg.ValidateEmbeddedSQSConfig` before embedding.
It requires decoded SQS options and rejects every `queue_url`, including literal
URLs and URLs paired with a name. SQS receiver and sender options must each
declare `queue_name` or `queue_tags`; a session's queue options are not inherited.
Non-empty SQS binding addresses must be physical names or `sqs:queue`.
Unresolved tokens anywhere in the full embedded document are also rejected.
The selected queue URL remains runtime-only.

The facade's shared base calls `GrantSQSConfig`, which uses `ResolveQueue`
for receiver, sender, and binding references. It keeps exact queue handles for
message-operation grants and resource dependencies. Only tag mode adds
`ListQueues` and `ListQueueTags` metadata reads; name mode uses `GetQueueUrl`.
Direct URLs remain available for non-embedded config. An unregistered direct
URL leaves message-operation grants to the consumer.

See [runtime selection](config-initialization.md#queue-references-in-embedded-documents)
and [discovery grants](iam.md#sqs-discovery-grants).

### Literal credentials

`bridgecfg.Builder.Build` and construct Phase 1 do not reject literal
credentials. `bridgecfg.ScanForPlaintextSecrets(cfg)` remains an explicit
utility for consumers who choose a reference-only credential policy. It is
not part of the default builder or facade validation path. Embedded literals
remain visible to readers of the binary and build artifacts.

## Usage Example

```go
import (
    gobridgesingle "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
    "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
    "github.com/aws/jsii-runtime-go"
)

qr := registry.NewQueueRegistry()
qr.AddQueue("inbound", inboundQueue)

// cfg is a *ports.BridgeConfig (build it with the bridgecfg builder).
single := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
    Vpc:   vpc,
    Image: gobridgecdk.ImageFromRegistry("123456789.dkr.ecr.eu-west-1.amazonaws.com/gobridge@sha256:<digest>"),
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
