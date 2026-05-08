# AWS File-Based Config CDK Redesign — Design

Status: Approved (grilling complete 2026-05-07)
Scope: `deployment/aws-filebased-config/` (cdk + infra; lib runtime unchanged)
Compatibility: **No backward compatibility.** This module has never been deployed; old constructs are deleted outright.

## Problem

The current constructs deploy a single ECS task with EFS-backed config. Two gaps:

1. **No clustered topology.** Filesystem-replicated bridges (multi-worker, single
   control) require RW/RO mount split, ALB admin-API routing, and shared
   peer-discovery state. Today's construct cannot express any of this.
2. **Configurability is all-or-nothing.** External CDK consumers either accept
   the defaults or fork the module. No programmatic builder for `bridge.yaml`,
   no automatic IAM grant derivation, no synth-time validation.

The redesign delivers a **versatile but hard-to-misconfigure** API: external
CDK stacks pick a single construct (Single or Cluster), supply a `bridge.yaml`
(file or programmatic) and registries of queues/SSM params, and have IAM,
EFS, ALB routing, and validation derived automatically. Misconfigurations
fail at `cdk synth`, not at runtime.

## Non-goals

- Cross-region failover (single-region only).
- DynamoDB-backed config profile (separate `deployment/aws-dynamodb-config/`).
- Schema-breaking yaml migrations (handled by runtime, out of scope here).
- Multiple gobridge instances per AWS account (forbidden — see Singleton constraint).

## Construct surface

Five constructs and two helper packages.

| Construct | Layer | Purpose |
|---|---|---|
| `GoBridgeSingle` | L2 | One ECS task, RW EFS mount, no clustering. Facade over shared base. |
| `GoBridgeCluster` | L2 | Control + worker services, RW/RO mount split. Facade over shared base. |
| `GoBridgeEfsConfig` | L2 | EFS filesystem + access points (RW/RO). Reused by both. |
| `GoBridgeALBAttachment` | L2 | Listener rules: admin paths → control TG, data paths → worker TG. |
| `GoBridgeAlarms` | L2 | Bundle: control-absence, worker-degraded, EFS-IO, ALB-unhealthy, ALB-5xx. |

| Package | Purpose |
|---|---|
| `cdk/bridgecfg/` | Hand-written fluent builder for `*ports.BridgeConfig`. |
| `cdk/registry/` | `QueueRegistry` + `SsmParamRegistry`: name → `IQueue`/`IParameter` maps. |

`GoBridgeSingle` and `GoBridgeCluster` are thin facades over a private base
package `cdk/constructs/internal/gobridgebase/` that owns the shared
ECS/EFS/IAM/seeder/asset machinery.

**No L3 wrapper.** A copy-paste quickstart in the README composes the five
constructs for the common case. This avoids the prop-bloat / fork-me failure
mode of "do everything" L3 constructs.

## Authoring `bridge.yaml`

Two paths supported; consumers pick per-stack.

**Hand-written yaml:**

```go
single := gobridgecdk.NewGoBridgeSingle(scope, "Bridge", &gobridgecdk.SingleProps{
    Cluster:          cluster,
    BridgeConfig:     gobridgecdk.BridgeYamlAsset(filepath.Join("config", "bridge.yaml")),
    QueueRegistry:    queueRegistry,
    SsmParamRegistry: ssmRegistry,
})
```

**Programmatic builder:**

```go
cfg := bridgecfg.New("orders-bridge").
    WithHTTPAdminAPI(bridgecfg.AdminAPIDefaults()).
    WithSQSReceiver("orders-in", queueRegistry.Ref("orders-in")).
    WithSQSSender("orders-out", queueRegistry.Ref("orders-out")).
    WithMQTTBroker("iot", "tcp://broker:1883",
        bridgecfg.MQTTCredsFromSSM(ssmRegistry.Ref("/bridge/mqtt"))).
    WithSQLiteOutbox("/mnt/gobridge/state/outbox.db").
    WithRoute("orders-in", "orders-out").
    Build()

single := gobridgecdk.NewGoBridgeSingle(scope, "Bridge", &gobridgecdk.SingleProps{
    Cluster:          cluster,
    BridgeConfig:     gobridgecdk.BridgeYamlInline(cfg),
    QueueRegistry:    queueRegistry,
    SsmParamRegistry: ssmRegistry,
})
```

The builder lives only in `cdk/bridgecfg/` (CDK-package-local). It is NOT
exposed from each plugin. **Adding a new plugin requires syncing
`bridgecfg/<kind>.go` and `internal/grants/<kind>.go` in the same change** —
documented contributor obligation, optionally enforced by a CI check that
asserts every kind in `ports.DefaultRegistry` has both a builder method and
a grant function.

`AdminAPIDefaults()` enables the admin API on `:8080` with `/healthz`,
`/readyz`, `/api/v1/*` routes.

## Source resolution

Both `BridgeYamlAsset(path)` and `BridgeYamlInline(cfg)` return a sealed
`BridgeConfigSource` opaque type. The construct unwraps it internally.

- `BridgeYamlAsset(path)` — CDK `s3assets.NewAsset` reads the file for
  upload; tier B parses the same file from disk for validation. Single
  synth pass, no drift concern.
- `BridgeYamlInline(cfg)` — marshals via `config.MarshalYAML`, creates an
  asset from the marshaled bytes, then re-parses via `config.ParseFile` so
  tier B walks the same structure that gets seeded. **Single tier B code
  path** for both source types; what was validated is what gets deployed.

## Tier B: parse, validate, derive

At synth time, every `BridgeConfig` is processed in two phases:

### Phase 1 — Constructor (fast-fail)

Errors thrown immediately; surface at the construct call site.

1. yaml parses (`config.ParseFile`).
2. Stage-1 validators (`config/validate.go`).
3. Filesystem-topology constraints from existing `validateFilesystemProfile`:
   no `delivery_mode: shared_outbox`, no `route.session` lease.
4. Plaintext credential scan (see Secrets).
5. SQLite store paths under EFS mount root.
6. Worker referencing RW-only path (cluster only).

### Phase 2 — `construct.validate()` (aggregated)

Errors collected via `Annotations.of(scope).addError(...)` so synth reports
**all** missing references in one run, not iteration-by-iteration.

1. Every SQS queue name in yaml has a `QueueRegistry` entry; missing →
   `addError("yaml references SQS queue 'X' but no such entry in
   QueueRegistry. Add: registry.AddQueue(\"X\", queue)")`.
2. Every SSM URI has an `SsmParamRegistry` entry; missing → similar message.
3. yaml's `bridge.cluster.endpoints` (if present): each value parses as URL.

`QueueRegistry` and `SsmParamRegistry` are **conditionally required** props:
tier B inspects yaml first; the prop becomes required only if yaml uses
adapter types that need it. Missing registry when needed = typed synth
error, never nil panic.

### Phase 3 — Grant derivation

Tier B walks the parsed yaml and emits IAM grants on the task role(s) using
CDK's typed grant methods (no manual ARN construction).

- **SQS receivers:** `queue.GrantConsumeMessages(role)` always; additionally
  `queue.Grant(role, "sqs:ChangeMessageVisibility")` when `auto_extend: true`.
- **SQS senders:** `queue.GrantSendMessages(role)`. No FIFO extras.
- **SSM (credentials):** assumed always SecureString; `param.GrantRead(role)`
  (CDK handles `ssm:GetParameter` + `kms:Decrypt` for AWS-managed key).
- **EFS:** see EFS access enforcement below.
- **CloudWatch Logs:** auto-granted via `logGroup.GrantWrite(role)`.
- **EFS CMK:** auto-granted when `EfsKmsKey` prop is set.

Per-adapter grant functions live in `cdk/constructs/internal/grants/<kind>.go`.

## Cluster internal architecture

`GoBridgeCluster` creates:

- **EFS filesystem** with two access points sharing root path `/`:
  - Both APs use `posixUser: {uid: 1000, gid: 1000}` (matches bridge container
    user from `docs/aws-deployment/overview.md`).
  - Both APs see the same files; RW/RO is enforced at IAM and ECS mount level,
    NOT at POSIX-user level.
- **ControlService** (Fargate, `DesiredCount: 1` hard-coded):
  - Mounts EFS RW (`readOnly: false`).
  - IAM: `ClientMount` + `ClientWrite`.
  - Has yaml seeder init container.
  - Env: `NODE_ROLE=control`, `BRIDGE_BOOTSTRAP=...`.
  - Deployment strategy: `MinHealthyPercent=0`, `MaxHealthyPercent=100`
    (old task fully drained before new starts; eliminates concurrent EFS RW
    writers during rolling deploys).
- **WorkerService** (Fargate, `DesiredCount: 2` default; configurable):
  - Mounts EFS **RO** (`readOnly: true` at ECS volume level).
  - IAM: `ClientMount` only (no `ClientWrite`).
  - No seeder (boots from EFS; readiness blocks until yaml present).
  - Env: `NODE_ROLE=worker`, same `BRIDGE_BOOTSTRAP`.
  - Optional autoscaling via `AutoScaling: AutoScalingProps{Min, Max, TargetCPU}` —
    **off by default**; consumer opts in explicitly.
- **No Cloud Map.** Peer discovery is EFS-mediated via the LeaseStore (verified
  in `runtime/bridge.go` `WithClusterEndpoints` and
  `bridge/builder_prepare.go`). Each task auto-detects its own address via
  `EcsEndpointResolver` and registers in the LeaseStore; siblings discover
  via the LeaseStore.
- **Control role ≠ Worker role**: split for IAM EFS grants. Other grants
  (SQS, SSM, Logs) are identical between the two roles since both tasks
  process messages.

`GoBridgeSingle` is the same pattern with only the control service.

`DesiredCount: 1` for control is a runtime invariant (single LeaseStore
writer semantics). It is **NOT** exposed as a prop.

### Multi-AZ posture

- **ECS:** spans all private subnets in VPC by default. Opt-in restriction
  via `VpcSubnets: SubnetSelection{...}`.
- **EFS:** mount targets created in the same `VpcSubnets` selection as ECS.
  Mismatched selections fail at synth.
- **ALB:** consumer-owned; AWS rejects single-AZ ALB at creation directly.

### VPC

`Vpc` prop is **optional**. If nil, the construct does
`awsec2.Vpc_FromLookup(scope, "DefaultVpc", &VpcLookupOptions{IsDefault: jsii.Bool(true)})`.
First synth populates `cdk.context.json`.

### EFS posture (defaults)

| Setting | Default | Override |
|---|---|---|
| Encryption | Always-on (AWS-managed key) | `EfsKmsKey: kmsKey` for CMK; cannot disable |
| Backup | On (default plan, daily, 35-day retention) | `DisableBackup: true` |
| `RemovalPolicy` | `RETAIN` | `RemovalPolicy: cdk.RemovalPolicyDestroy` |
| Throughput | `ELASTIC` | `ThroughputMode` prop |
| Performance | General Purpose | locked, no prop |

## Singleton constraint

**Only one `GoBridgeSingle` or `GoBridgeCluster` instance is supported per
AWS account.** Multiple instances in the same account are forbidden.

Enforcement:
1. Prominent README warning.
2. Synth-time scope scan: error if more than one `GoBridgeSingle` or
   `GoBridgeCluster` is found in the same Stack tree.
3. Cross-account / cross-stack collisions remain operator responsibility
   (no custom resource enforcement).

The bridge name comes from yaml's `bridge.name` (validated at synth against
`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`). It is used as the log group middle
segment, alarm name prefix, EFS construct ID, etc. **No `Name` prop on
construct props** — derived from yaml.

## ALB attachment

`GoBridgeALBAttachment` adds listener rules to a consumer-supplied
`IApplicationListener`. Listener is required (no auto-create).

Path patterns are **derived from yaml**:

| Source | Target |
|---|---|
| yaml HTTP admin API paths (`/api/v1/*` by default) | Control TG |
| yaml HTTP admin status paths | Control TG |
| `/healthz`, `/readyz` (always) | Worker TG (load-balanced) |
| Each yaml HTTP receiver's `path` | Worker TG |

For `Single`, both TGs collapse to the single service.

### Listener rule priorities

`BasePriority int` prop, default `100`, step `10`. Documented offsets
(`+0` admin write, `+10` admin status, `+20` healthz, `+30+` user paths).
Range `[BasePriority, BasePriority+99]` reserved. Synth fails if
`BasePriority < 1` or any consumer rule already uses a reserved offset.

### Health checks

Both TGs health-check `/healthz`. Defaults: `Interval=15s`, `Timeout=5s`,
`HealthyThreshold=2`, `UnhealthyThreshold=2` (~30s detection window).
Override via optional `HealthCheck: HealthCheckProps{...}` prop.

### Defense in depth

Workers reject admin writes both because EFS is mounted read-only AND because
the ALB rule routes admin paths only to the control TG. Two layers.

## DNS, outputs, cross-stack lookup

### Construct accessors

Typed CDK accessors (NOT auto-`CfnOutput`s):

```go
// On GoBridgeCluster / GoBridgeSingle:
func ControlService() awsecs.IService  // escape hatch
func WorkerService()  awsecs.IService  // cluster only

// On GoBridgeALBAttachment:
func PublicDnsName() *string  // listener.LoadBalancer.LoadBalancerDnsName()
func AdminURL()      *string  // "https://<albdns>/api/v1/"
func HealthzURL()    *string
```

### Same-stack outputs

`attachment.WithCfnOutputs(prefix string)` — auto-emits `CfnOutput`s for
`AdminURL` and `HealthzURL` named `<prefix>AdminURL`, `<prefix>HealthzURL`.

### Cross-stack lookup (SSM Parameter Store)

For projects deploying gobridge in one stack and consuming from microservice
stacks. Soft-coupled (vs hard `Fn.importValue`): producer rotates freely,
consumer re-resolves at next synth.

```go
// Producer stack
attachment.WithSSMExports("/gobridge/prod/orders-bridge")
// Optional: include implementation ARNs
attachment.WithSSMExports("/gobridge/prod/orders-bridge", ssmexports.IncludeARNs())
```

Consumer-supplied SSM URI prefix. Auto-publishes:

```
<prefix>/admin-url
<prefix>/healthz-url
<prefix>/manifest-version    ← schema sentinel
```

With `IncludeARNs()`: also `<prefix>/alb-arn`, `<prefix>/cluster-arn`,
`<prefix>/efs-id`.

```go
// Consumer (microservice) stack
ref := gobridgecdk.LookupBridge(scope, "BridgeRef", "/gobridge/prod/orders-bridge")
adminURL := ref.AdminURL()  // *string CDK token, deploy-time resolution
```

`LookupBridge` uses `awsssm.StringParameter_FromStringParameterName` (deploy-time
token, soft coupling). `BridgeRef` exposes the same accessor surface as the
producing constructs. The `manifest-version` sentinel allows future schema
changes to fail consumer synth fast with a clear "upgrade dependency"
message.

## Registries

Both registries are string-keyed with typed `Ref()` accessors that wrap the
key. Membership is **explicit only** (no auto-import scanning).

```go
type QueueRegistry struct{ ... }
// Accepts both newly-created and imported (Queue.fromQueueArn / fromQueueName) queues:
func (r *QueueRegistry) AddQueue(name string, queue awssqs.IQueue)
func (r *QueueRegistry) Ref(name string) QueueRef

type SsmParamRegistry struct{ ... }
// Accepts both newly-created and imported (Parameter.fromParameterName) params.
// Keyed by full URI / parameter path (e.g. "/bridge/mqtt"):
func (r *SsmParamRegistry) AddParameter(uri string, param awsssm.IParameter)
func (r *SsmParamRegistry) Ref(uri string) ParamRef
```

Tier B resolves `Ref` → `IQueue`/`IParameter` and uses CDK grant methods
directly — works identically for created and imported resources.

## Seeding & drift

Initial seed: an init container in the control task copies the asset to the
EFS RW mount.

| Mode | Behavior | Use case |
|---|---|---|
| `SeedOnce` (default) | Init container seeds iff EFS file is absent; warns if present-but-different. | Admin API is source of truth — CDK seeds once. |
| `Overwrite` | Always overwrites EFS with asset. | GitOps — CDK is source of truth. |
| `AbortDeploy` | Compares canonical hashes; aborts if different. | Strict gating — surface drift loudly. |

`SeedOnce` is the safe default: first-deploy seeds, subsequent deploys
no-op, admin-API edits are never silently overwritten.

### Drift detection (canonical hash)

Init container canonicalizes both the EFS file and the asset (sort keys,
strip comments, normalize whitespace via Python with PyYAML — already
present in the aws-cli image) before SHA-256 comparison. Robust to admin-API-
induced formatting differences.

### Init container

Pinned aws-cli image: tag + digest pinned (e.g.
`public.ecr.aws/aws-cli/aws-cli:2.15.0@sha256:abc...`). Override via
`SeederImage: string` prop (private mirrors / air-gapped). `make
update-seeder-image` target fetches latest 2.x digest periodically.

IAM scoped to the specific S3 asset key only.

### Atomic write

Seeder writes to a temp file, then `mv` (POSIX atomic):

```sh
aws s3 cp s3://...   /tmp/bridge.yaml.new
canonicalize         /tmp/bridge.yaml.new > /tmp/bridge.yaml.canon
mv                   /tmp/bridge.yaml.canon /mnt/gobridge/bridge.yaml
```

### Exit code convention

| Code | Reason |
|---|---|
| 0 | Success (seeded, or canonical hashes matched in SeedOnce) |
| 10 | `AbortDeploy` mode and hashes differ |
| 20 | S3 download failed (IAM/network) |
| 30 | yaml unparseable |
| 40 | EFS mount not writable |
| 50 | Canonicalizer missing (broken image) |

Each outcome emits a structured JSON log line:

```json
{"level":"error","mode":"AbortDeploy","reason":"hash_mismatch","expected":"sha256:abc","actual":"sha256:def","exit":10}
```

CloudWatch Logs Insights query:
`fields @timestamp, reason, exit | filter exit > 0`.

## Secrets policy

yaml is scanned at synth for plaintext credentials. **Hard error** when a
known credential field (`password`, `secret`, `api_key`, `client_secret`,
`bearer_token`, `private_key`, etc.) has a non-empty value that is not a
credential URI (`pms://` or other supported scheme).

Error message identifies field path and points to the SSM URI alternative.
**No opt-out, no warning mode.** The supported-scheme allow-list lives in
`cdk/bridgecfg/secrets.go` close to the credential-resolver registration.

## Logging

| Setting | Default | Override |
|---|---|---|
| Log group name | `/gobridge/<bridge.name>/control` and `/gobridge/<bridge.name>/worker` | `LogGroupPrefix` (replaces `/gobridge/`); `LogGroup: existingLg` (BYO group) |
| Retention | 30 days | `LogRetention: awslogs.RetentionDays_*` |
| `RemovalPolicy` | `DESTROY` | `LogGroupRemovalPolicy: cdk.RemovalPolicyRetain` |
| Driver mode | `NON_BLOCKING` | locked |
| Buffer size | 25 MiB | `LogBufferSize` |
| Seeder logs | Same group as bridge container, distinguished by stream prefix (`control-seeder/*` vs `control-bridge/*`) | — |

## HTTP exposure

ECS task definition `portMappings` are **always** auto-derived from the
union of:

1. yaml HTTP receiver listen ports.
2. `BootstrapConfig.AdminAddr` and `MonitorAddr` ports.

No `Exposure` prop. No "port not in Exposure" validation row.

## Alarms (`GoBridgeAlarms`)

Single bundle construct that wires multiple alarms to one supplied SNS
topic:

| Alarm | Detection |
|---|---|
| Control absence | ECS `RunningTaskCount == 0` for N min (default 5) |
| Worker capacity degraded | `RunningTaskCount < DesiredCount` for N min |
| EFS IO saturation | `PercentIOLimit > 90%` for N min |
| ALB target unhealthy | `UnHealthyHostCount > 0` for N min, per TG |
| ALB 5xx | `HTTPCode_Target_5XX_Count > threshold` |

```go
gobridgecdk.NewGoBridgeAlarms(scope, "BridgeAlarms", &AlarmsProps{
    Cluster:    cluster,           // for control + worker metrics
    Efs:        cluster.EfsConfig(), // for EFS metrics
    Attachment: attachment,        // OPTIONAL — ALB alarms skipped if absent
    AlarmTopic: snsTopic,
    // Per-alarm threshold overrides + Disable<Name> opt-outs available.
})
```

`Attachment` is optional so `Single` deployments without ALB still get
cluster + EFS alarms.

## Failover

| Failure | Detection | Recovery | Worker impact | Admin API impact |
|---|---|---|---|---|
| Control task crash | ECS health check (~30s) | ECS replaces task (~30s) | None | 503 for ~35–70s |
| Worker task crash | ECS + ALB health (~30s) | Replaced; surviving workers absorb | Reduced capacity briefly | None |
| EFS issue | Mount errors | No automated recovery | Continue with in-memory runtime until restart | Writes fail |
| Bad yaml via admin API | Reload validator | Keep last-good `appliedRef`; `/status` exposes error | None | Reload rejected |
| Bad yaml via CDK deploy | Init container hash check (`AbortDeploy` mode) | Deploy aborts, ECS rollback | None | None |

## Validation matrix (synth-time)

| Check | Phase | Error format |
|---|---|---|
| yaml unparseable | 1 | `"bridge.yaml: <yaml lib error with line/col>"` |
| Stage-1 validator fail | 1 | `"bridge.yaml: <stage-1 error>"` |
| Plaintext credential at field path | 1 | `"yaml field '<path>' contains a plaintext credential. Use a credential URI (pms:// or supported scheme)."` |
| Filesystem topology + `delivery_mode: shared_outbox` | 1 | `"GoBridge{Cluster,Single} on filesystem topology does not support delivery_mode: shared_outbox (route 'X'). Use the DynamoDB profile."` |
| Filesystem topology + `route.session` lease | 1 | `"GoBridge{Cluster,Single} on filesystem topology does not support route.session lease coordination (route 'X'). Use the DynamoDB profile."` |
| Store path outside EFS mount | 1 | `"SQLite store path '/x' is not under EFS mount '/mnt/gobridge'. Use '/mnt/gobridge/state/...'"` |
| Worker referencing RW-only path (cluster) | 1 | `"yaml store path '/mnt/gobridge/control-only/...' is RW-only but workers mount this read-only"` |
| `bridge.name` invalid regex | 1 | `"bridge.name '<value>' must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"` |
| `bridge.cluster.endpoints` value malformed URL | 1 | `"bridge.cluster.endpoints['<key>']='<value>' is not a valid URL"` |
| Multiple GoBridge in same stack | 1 | `"Only one GoBridgeSingle or GoBridgeCluster instance is supported per stack/account; found N."` |
| Unknown SQS queue name | 2 (`addError`) | `"yaml references SQS queue 'X' but no such entry in QueueRegistry. Add: registry.AddQueue(\"X\", queue)"` |
| Unknown SSM parameter URI | 2 (`addError`) | `"yaml references SSM parameter '/path' but no such entry in SsmParamRegistry. Add: registry.AddParameter(\"/path\", param)"` |
| ALB priority collision | 1 (in attachment ctor) | `"ALB BasePriority N reserves [N..N+99]; consumer rule already uses N+M"` |
| EFS subnet selection ≠ ECS | 1 | `"GoBridgeEfsConfig VpcSubnets must match GoBridge cluster VpcSubnets"` |

Every error includes (a) what was found, (b) what was expected, (c) how to
fix.

## Module layout

```
deployment/aws-filebased-config/
├── README.md
├── ARCHITECTURE.md
├── UBIQUITOUS.md
├── cdk/
│   ├── go.mod                 (single module covers all sub-packages)
│   ├── go.sum
│   ├── gobridgecdk.go         (top-level: re-exported constructors,
│   │                           BridgeYamlAsset/Inline, LookupBridge)
│   ├── constructs/
│   │   ├── gobridge_single.go
│   │   ├── gobridge_cluster.go
│   │   ├── gobridge_efs_config.go
│   │   ├── gobridge_alb_attachment.go
│   │   ├── gobridge_alarms.go
│   │   └── internal/
│   │       ├── gobridgebase/  (shared base for Single/Cluster facades)
│   │       ├── grants/        (per-adapter IAM grant funcs)
│   │       ├── seeder/        (init container script + Dockerfile pin)
│   │       └── validation/    (tier B walker + secret scanner)
│   ├── bridgecfg/             (programmatic builder)
│   │   ├── builder.go
│   │   ├── defaults.go
│   │   ├── secrets.go
│   │   ├── sqs.go
│   │   ├── mqtt.go
│   │   ├── http.go
│   │   ├── stores.go
│   │   └── routes.go
│   └── registry/
│       └── registry.go
├── lib/                       (unchanged)
└── infra/                     (unchanged — BootstrapConfig already has
                                NodeRole + Topology; nothing to add)
```

Public surface from top-level `cdk` package:
- `NewGoBridgeSingle`, `NewGoBridgeCluster`, `NewGoBridgeAlarms` + their
  `*Props` types.
- `BridgeYamlAsset`, `BridgeYamlInline` + sealed `BridgeConfigSource`.
- `LookupBridge` + `BridgeRef`.

`bridgecfg` and `registry` are imported as separate packages, not
re-exported.

## Testing

| Layer | Tool | Coverage |
|---|---|---|
| `bridgecfg` builder | Go `testing` | Defaults correct; round-trips through `config.MarshalYAML` + `config.ParseFile`. |
| Tier B grant derivation | Go `testing`, table-driven, per-adapter file under `internal/grants/` | Each adapter type → expected IAM statements. |
| Tier B validation | Go `testing`, **one named test per validation matrix row** | Negative paths produce documented error. |
| CDK constructs | `aws-cdk-lib/assertions` **targeted** assertions (not full template snapshots) | Resources, IAM, EFS access points, ALB rules, port mappings. |
| Integration (opt-in) | `make integration-aws`, `//go:build integration_aws` | Deploy single + cluster against sandbox; healthz 200; status JSON; SQS round-trip; cluster: scale workers, kill one, verify continuity. |
| Existing | `make test` + `make lint` | Unchanged. |

Integration tests run nightly + on release tags, NOT on every PR.

## Documentation deliverables

- Update `deployment/aws-filebased-config/README.md` with new construct table,
  quickstart compose snippet (replaces L3), singleton constraint warning.
- Update `ARCHITECTURE.md` with single-vs-cluster diagrams + tier B explanation
  + "no Cloud Map" rationale (LeaseStore-mediated peer discovery).
- Update `UBIQUITOUS.md` with new terms (`QueueRegistry`, `SsmParamRegistry`,
  `OnConfigDrift`, `BridgeConfigSource`, `BridgeRef`, `LookupBridge`).
- Update `docs/scenarios/cdk/05-multi-bridge-cluster.md` to use
  `GoBridgeCluster`.
- Update `docs/aws-deployment/tco.md` if cluster default sizing changes cost
  shape.

## Dependencies & risks

- Depends on existing `config.ParseFile`, `config.MarshalYAML`,
  `config.validate.go`, `validateFilesystemProfile`, and `ports.BridgeConfig`
  schema stability.
- Risk: tier B grant derivation grows complex as new adapter types ship.
  Mitigation: each adapter has its own file in `internal/grants/`; CI check
  enforces presence for every kind in `ports.DefaultRegistry`.
- Risk: ALB rule priority collisions with consumer's own rules. Mitigation:
  `BasePriority` prop, documented reserved range, synth-time collision check.

---

## Tasks

Tasks are sized to be implementable independently where possible. The
"Depends on" column lists hard prerequisites; other ordering is preference,
not requirement. Tasks with no dependencies (foundation layer) can start in
parallel.

| ID | Task | Depends on |
|---|---|---|
| T01 | `cdk/registry/` package: `QueueRegistry`, `SsmParamRegistry`, `QueueRef`, `ParamRef` types + tests. — DONE | — |
| T02 | `cdk/constructs/internal/seeder/`: pinned aws-cli Dockerfile/manifest, seeder shell script with canonicalization (PyYAML), atomic temp+rename, exit codes 0/10/20/30/40/50, structured JSON log lines. `make update-seeder-image` target. — DONE | — |
| T03 | `cdk/bridgecfg/secrets.go`: scheme allow-list (`pms://` and any other supported credential schemes) + `ScanForPlaintextSecrets(*ports.BridgeConfig) error`. — DONE | — |
| T04 | `cdk/bridgecfg/`: builder package — `New(name)`, `Build()`, fluent methods for SQS receiver/sender, MQTT broker (incl. `MQTTCredsFromSSM`), HTTP admin API + `AdminAPIDefaults()`, SQLite outbox/lease/dlq, routes. Tests including round-trip through `config.MarshalYAML`+`config.ParseFile`. — DONE | — |
| T05 | Top-level `cdk/gobridgecdk.go`: sealed `BridgeConfigSource` type, `BridgeYamlAsset(path)`, `BridgeYamlInline(*ports.BridgeConfig)`. Internal asset+parsed-config bundling. — DONE | — |
| T06 | `cdk/constructs/internal/validation/`: tier B walker + Phase 1 fast-fail validators (yaml parse, stage-1, `validateFilesystemProfile` invocation, secret scan via T03, store path checks, name regex, endpoints URL parse). Returns typed errors. — DONE | T03, T05 |
| T07 | `cdk/constructs/internal/validation/`: Phase 2 aggregated validator (registry resolution via `Annotations.addError`). Single+Cluster `validate()` method calls into this. — DONE | T01, T05 |
| T08 | `cdk/constructs/internal/grants/`: per-adapter grant derivation functions — `sqs.go` (consume + auto_extend ChangeMessageVisibility, send), `ssm.go` (param read), `efs.go` (topology-derived control vs worker), `logs.go`, `kms.go` (CMK). Table-driven tests per file. — DONE | T01 |
| T09 | `GoBridgeEfsConfig` construct: EFS filesystem with always-on encryption, optional CMK, ELASTIC throughput, General Purpose, RETAIN default; two access points (control RW, workers RO) sharing root path with UID/GID 1000; mount-target subnet validation. — DONE | — |
| T10 | `cdk/constructs/internal/gobridgebase/`: shared facade base — Fargate task def construction, EFS volume mount wiring (control RW, worker RO via `readOnly:true`), seeder init container wiring with EXPECTED_HASH/MODE env, log group creation with prefix scheme, port mapping derivation from yaml + BootstrapConfig, IAM role + grant application via T08. — DONE | T02, T05, T08, T09 |
| T11 | `GoBridgeSingle` facade construct + `SingleProps`. Constructs single control service over T10. Phase 1 validation invocation. — DONE | T06, T07, T10 |
| T12 | `GoBridgeCluster` facade construct + `ClusterProps`. Constructs control (DesiredCount=1, deployment 0/100) + worker (DesiredCount=2 default, optional autoscaling) services over T10. Phase 1 validation invocation. RW/RO mount per service. — DONE | T06, T07, T10 |
| T13 | Singleton constraint: synth-time scope scan that errors if multiple `GoBridgeSingle`/`GoBridgeCluster` exist in the same Stack tree. Lives in T10 or shared helper. | T11, T12 |
| T14 | `GoBridgeALBAttachment` construct + props. Creates control + worker target groups, attaches ECS services, derives listener rule paths from yaml HTTP receivers + admin API, BasePriority+offsets, health checks `/healthz` with tuned defaults. Validates priority collisions. | T11, T12 |
| T15 | Output helpers: `attachment.WithCfnOutputs(prefix)`, `attachment.WithSSMExports(prefix, opts...)` + `ssmexports.IncludeARNs()`. Construct accessors `AdminURL()`, `HealthzURL()`, `PublicDnsName()`, `ControlService()`, `WorkerService()`. | T14 |
| T16 | `LookupBridge(scope, id, prefix)` + `BridgeRef` type. Uses `awsssm.StringParameter_FromStringParameterName`. Manifest-version check. | T15 |
| T17 | `GoBridgeAlarms` bundle construct + `AlarmsProps`. Wires control-absence, worker-degraded, EFS PercentIOLimit, ALB unhealthy, ALB 5xx to supplied SNS topic. Optional `Attachment` prop. Per-alarm threshold + Disable opt-outs. | T11, T12, T14 |
| T18 | Delete old `efs_config.go`, `efs_config_test.go`, `gobridge_service.go`, `gobridge_service_test.go`. Update any internal references. | T11, T12 |
| T19 | Per-validation-matrix-row negative tests, one named test per row (`Test_TierB_Validation_<RowName>`). | T06, T07 |
| T20 | Targeted `aws-cdk-lib/assertions` tests for each construct (resources, IAM statements, EFS APs, ALB rules, port mappings, mount readOnly flags). | T11, T12, T14, T17 |
| T21 | Integration test scaffold: `//go:build integration_aws`, `make integration-aws` target. Deploy single + cluster, healthz/status checks, SQS round-trip, cluster worker scale + kill scenario, teardown with `RemovalPolicy.DESTROY` override. | T11, T12, T14 |
| T22 | Update `deployment/aws-filebased-config/README.md`: new construct table, quickstart compose snippet, singleton warning, secrets policy. | T11, T12, T14, T15, T17 |
| T23 | Update `deployment/aws-filebased-config/ARCHITECTURE.md`: single+cluster diagrams, tier B explanation, "no Cloud Map" rationale (LeaseStore peer discovery). | T22 |
| T24 | Update `deployment/aws-filebased-config/UBIQUITOUS.md`: `QueueRegistry`, `SsmParamRegistry`, `OnConfigDrift`, `BridgeConfigSource`, `BridgeRef`, `LookupBridge`. | T22 |
| T25 | Update `docs/scenarios/cdk/05-multi-bridge-cluster.md` to use `GoBridgeCluster`. | T22 |
| T26 | Update `docs/aws-deployment/tco.md` if cluster default sizing changes cost shape. | T17 |
| T27 | CI check: every kind in `ports.DefaultRegistry` has matching `bridgecfg/<kind>.go` builder method AND `internal/grants/<kind>.go` derivation function. Fail build on missing. | T04, T08 |

### Dependency graph (compact)

```
Foundation (no deps): T01, T02, T03, T04, T05, T09
   ├─ T06 ← T03, T05
   ├─ T07 ← T01, T05
   ├─ T08 ← T01
   └─ T10 ← T02, T05, T08, T09
            ├─ T11 ← T06, T07, T10
            ├─ T12 ← T06, T07, T10
            └─ T13 ← T11, T12
                ├─ T14 ← T11, T12
                │   ├─ T15 ← T14
                │   │   └─ T16 ← T15
                │   ├─ T17 ← T11, T12, T14
                │   ├─ T20 ← T11, T12, T14, T17
                │   └─ T21 ← T11, T12, T14
                ├─ T18 ← T11, T12
                ├─ T19 ← T06, T07
                └─ T22 ← T11, T12, T14, T15, T17
                    ├─ T23 ← T22
                    ├─ T24 ← T22
                    └─ T25 ← T22

T26 ← T17
T27 ← T04, T08
```

### Parallelizable work

- **Wave 1 (no deps):** T01, T02, T03, T04, T05, T09 — six tasks in parallel.
- **Wave 2:** T06, T07, T08 (after their respective wave-1 deps) and T10
  (after T02+T05+T08+T09).
- **Wave 3:** T11, T12 (parallel) and T19 (parallel with them after T06+T07).
- **Wave 4:** T13, T14, T18, T20 (T20 also waits for T17), T21, T27.
- **Wave 5:** T15, T17 (parallel after T14 + T11/T12).
- **Wave 6:** T16, T22, T26.
- **Wave 7:** T23, T24, T25 (all after T22).
