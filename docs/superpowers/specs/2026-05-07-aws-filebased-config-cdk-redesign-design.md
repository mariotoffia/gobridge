# AWS File-Based Config CDK Redesign — Design

Status: Draft (brainstorming-approved 2026-05-07)
Scope: `deployment/aws-filebased-config/` (cdk + infra; lib runtime unchanged)

## Problem

The current constructs deploy a single ECS task with EFS-backed config. Two gaps
motivate a redesign:

1. **No clustered topology.** Filesystem-replicated bridges (multi-worker, single
   control) require RW/RO mount split, ALB admin-API routing, and a control-task
   service-discovery entry. Today's construct can't express any of this.
2. **Configurability is all-or-nothing.** External CDK consumers either accept the
   defaults or fork the module. No programmatic builder for `bridge.yaml`, no
   automatic IAM grant derivation from yaml contents, no validation at synth time.

The redesign delivers a **versatile but hard-to-misconfigure** API: external CDK
stacks should pick a single construct (single or cluster), supply a bridge.yaml
(file or programmatic), and have IAM, EFS, ALB routing, and validation derived
automatically. Misconfigurations fail at `cdk synth`, not at runtime.

## Non-goals

- Cross-region failover (single-region only).
- DynamoDB-backed config profile (separate `deployment/aws-dynamodb-config/`).
- Schema-breaking yaml migrations (handled by runtime, out of scope here).

## Construct surface

Five public constructs, plus a programmatic `bridge.yaml` builder package:

| Construct | Layer | Purpose |
|---|---|---|
| `GoBridgeSingle` | L2 | One ECS task, RW EFS mount, no clustering. |
| `GoBridgeCluster` | L2 | Control + worker services, RW/RO mount split, Cloud Map SD. |
| `GoBridgeEfsConfig` | L2 | EFS filesystem + access points (RW/RO). Reused by both. |
| `GoBridgeALBAttachment` | L2 | Listener rules: admin paths → control SD, data paths → worker SD. |
| `GoBridgeEnvironment` | L3 | One-shot: cluster + EFS + ALB + alarms. For consumers who want defaults. |
| `GoBridgeControlAbsenceAlarm` | L2 | CloudWatch alarm: control task missing > N minutes (default 5). |

Plus:

| Package | Purpose |
|---|---|
| `cdk/bridgecfg/` | Programmatic builder for `*ports.BridgeConfig` with sane defaults. |
| `cdk/registry/` | `QueueRegistry` and `SsmParamRegistry` — name → `IQueue` / `IParameter` maps. |

## Authoring `bridge.yaml`

Two paths supported; consumers pick per-stack:

**Hand-written yaml:**

```go
single := gobridgecdk.NewGoBridgeSingle(scope, "Bridge", &gobridgecdk.SingleProps{
    Cluster:       cluster,
    BridgeConfig:  gobridgecdk.BridgeYamlAsset(filepath.Join("config", "bridge.yaml")),
    QueueRegistry: queueRegistry,
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
    Cluster:      cluster,
    BridgeConfig: gobridgecdk.BridgeYamlInline(cfg),
    QueueRegistry: queueRegistry,
    SsmParamRegistry: ssmRegistry,
})
```

The builder lives inside `cdk/bridgecfg/` (CDK-package-local). It returns
`*ports.BridgeConfig`; the runtime is unchanged.

`AdminAPIDefaults()` enables the admin API on `:8080` with `/healthz`, `/readyz`,
`/api/v1/*` routes. `WithClusterDefaults(controlEndpoint)` is auto-applied by
`GoBridgeCluster` if the supplied yaml omits cluster settings.

## Tier B: parse, validate, derive

At synth time, every `BridgeConfig` (whether asset or inline) is processed:

1. **Parse** via existing `config.ParseFile` / `config.MarshalYAML` round-trip
   for assets, or used directly for inline.
2. **Validate** via existing stage-1 validators in `config/validate.go`.
3. **Walk** the `*ports.BridgeConfig` to build a resource manifest:
   - SSM `pms://` URIs → `ssm:GetParameter` grants (resolved via `SsmParamRegistry`).
   - SQS queue names in receivers/senders → `sqs:Send/Receive/Delete/GetAttributes`
     grants (resolved via `QueueRegistry`).
   - HTTP receiver ports → ports added to task definition `Exposure`.
   - SQLite store paths → must live under EFS mount root; cluster workers must
     not reference RW-only sub-paths.
   - `bridge.cluster.endpoints` → must match Cloud Map names CDK creates.
4. **Apply** grants to the task role(s) automatically.
5. **Reject synth** on any unresolved reference, with an actionable error.
6. **Upload** yaml as a CDK S3 asset (after re-marshaling for inline configs).

Tier B catches a documented subset of misconfigurations; runtime stage-2
validation in `bridge/builder_prepare.go` remains the source of truth for
plugin-internal correctness.

## Seeding & drift

Initial seed: an init container in the control task runs
`aws s3 cp s3://<asset>/<key> /mnt/gobridge/bridge.yaml --no-overwrite`. Workers
have no seeder — they wait for EFS to contain the file (readiness probe).

Updates use a `OnConfigDrift` prop with three modes:

| Mode | Behavior | Use case |
|---|---|---|
| `AbortDeploy` (default) | Init container compares EFS yaml hash to asset hash; aborts if different. | Mixed workflows — surface drift loudly. |
| `Overwrite` | Init container always overwrites EFS with asset. | GitOps — CDK is source of truth. |
| `KeepExisting` | Init container only seeds if EFS file is absent. | Admin API is source of truth — CDK seeds once. |

The default forces operators to consciously choose a workflow before the first
admin-API edit can be silently overwritten.

Init container image: `public.ecr.aws/aws-cli/aws-cli:latest`. IAM scoped to the
specific S3 asset key only.

## Cluster internal architecture

`GoBridgeCluster` creates:

- **EFS filesystem** with two access points:
  - Control: `/control` (POSIX 755), mounted RW.
  - Workers: `/workers` (POSIX 555), mounted RO.
  - Both point into the same EFS — write-protection is at access-point level,
    not separate filesystems.
- **ControlService** (Fargate, DesiredCount: 1):
  - Mounts EFS RW.
  - Has yaml seeder init container.
  - Env: `NODE_ROLE=control`, `BRIDGE_BOOTSTRAP=...`.
- **WorkerService** (Fargate, DesiredCount: configurable, default 2):
  - Mounts EFS RO.
  - No seeder (boots from EFS; readiness blocks until yaml present).
  - Env: `NODE_ROLE=worker`, same `BRIDGE_BOOTSTRAP`.
  - Optional autoscaling (`AutoScalingProps{Min, Max, TargetCPU}`).
- **Cloud Map service discovery**:
  - `<name>-control.<namespace>` → control tasks.
  - `<name>-worker.<namespace>` → worker tasks.
- **Shared task IAM role**: derived from yaml via Tier B; identical for both
  services. RW/RO enforcement lives at EFS, not IAM.

`GoBridgeSingle` is the same pattern with only the control service and no Cloud
Map worker entry.

## ALB attachment

`GoBridgeALBAttachment` adds listener rules to a consumer-supplied ALB:

| Path pattern | Target |
|---|---|
| `/api/v1/config/*` | Control SD (admin writes) |
| `/api/v1/status*` | Control SD (preferred — has freshest control state) |
| `/healthz`, `/readyz` | Worker SD (load-balanced) |
| User-defined HTTP receiver paths | Worker SD |

Workers reject admin writes both because EFS is RO (write fails) and because the
ALB rule prevents the request reaching them. Two layers of defense.

For single-node deployments, the attachment routes everything to the single
service.

## Failover

| Failure | Detection | Recovery | Worker impact | Admin API impact |
|---|---|---|---|---|
| Control task crash | ECS health check (~30s) | ECS replaces task (~30s) | None | 503 for ~35–70s |
| Worker task crash | ECS + ALB health (~30s) | Replaced; surviving workers absorb | Reduced capacity briefly | None |
| EFS region issue | Mount errors | No automated recovery | Continue with in-memory runtime until restart | Writes fail |
| Bad yaml via admin API | Reload validator | Keep last-good `appliedRef`; expose error on `/status` | None | Reload rejected |
| Bad yaml via CDK deploy | Init container hash check | Deploy aborts, ECS rollback | None | None |

`GoBridgeControlAbsenceAlarm` (default threshold 5 minutes, configurable)
publishes to a consumer-supplied SNS topic when the control service has zero
healthy tasks for the configured period.

## Validation matrix (synth-time)

| Check | Error message format |
|---|---|
| yaml unparseable | `"bridge.yaml: <yaml lib error with line/col>"` |
| Unknown SQS queue name | `"yaml references SQS queue 'X' but no such entry in QueueRegistry. Add: registry.AddQueue(\"X\", queue)"` |
| Unknown SSM parameter URI | `"yaml references SSM parameter '/path' but no such entry in SsmParamRegistry. Add: registry.AddParameter(\"/path\", param)"` |
| HTTP port not in Exposure | `"yaml HTTP receiver listens on :N but Exposure declares only [...]. Add N or set AutoExposePorts: true"` |
| Store path outside EFS mount | `"SQLite store path '/x' is not under EFS mount '/mnt/gobridge'. Use '/mnt/gobridge/state/...'"` |
| Worker referencing RW-only path | `"yaml store path '/mnt/gobridge/control-only/...' is RW-only but workers mount this read-only"` |
| Cluster yaml without cluster block | `"GoBridgeCluster requires bridge.cluster.enabled: true (or use bridgecfg.WithClusterDefaults())"` |
| Single yaml WITH cluster block | `"GoBridgeSingle does not support clustering. Use GoBridgeCluster or remove bridge.cluster from yaml"` |

Every error includes (a) what was found, (b) what was expected, (c) how to fix.

## Module layout

```
deployment/aws-filebased-config/
├── README.md
├── ARCHITECTURE.md
├── UBIQUITOUS.md
├── cdk/
│   ├── go.mod
│   ├── constructs/
│   │   ├── gobridge_single.go
│   │   ├── gobridge_cluster.go
│   │   ├── gobridge_efs_config.go
│   │   ├── gobridge_alb_attachment.go
│   │   ├── gobridge_environment.go
│   │   ├── gobridge_alarms.go
│   │   └── internal/
│   │       ├── grants/
│   │       ├── seeder/
│   │       └── validation/
│   ├── bridgecfg/
│   │   ├── builder.go
│   │   ├── defaults.go
│   │   ├── sqs.go
│   │   ├── mqtt.go
│   │   ├── http.go
│   │   ├── stores.go
│   │   └── cluster.go
│   └── registry/
│       └── registry.go
├── lib/      (unchanged)
└── infra/    (BootstrapConfig: add cluster.* fields)
```

## Testing approach

| Layer | Tool | Coverage |
|---|---|---|
| `bridgecfg` builder | Go `testing` | Defaults correct; round-trips through `config.MarshalYAML` + `config.ParseFile`. |
| Tier B grant derivation | Go `testing`, table-driven | Each adapter type → expected IAM statements; missing registry entry → typed error. |
| CDK constructs | `aws-cdk-lib/assertions` snapshots | Resources, IAM, EFS access points, ALB rules, SD entries. |
| Integration (opt-in) | `make integration-aws` | Deploy to sandbox, smoke-test admin API + SQS round-trip, tear down. |
| Existing | `make test` + `make lint` | Unchanged. |

## Documentation deliverables

- Update `deployment/aws-filebased-config/README.md` with new construct table.
- Update `ARCHITECTURE.md` with single vs cluster diagrams + Tier B explanation.
- Update `UBIQUITOUS.md` with new terms (`QueueRegistry`, `SsmParamRegistry`,
  `OnConfigDrift`, `GoBridgeControlAbsenceAlarm`).
- Update `docs/scenarios/cdk/05-multi-bridge-cluster.md` to reference the new
  `GoBridgeCluster` construct.
- Update `docs/aws-deployment/tco.md` if cluster default sizing changes cost
  shape.

## Open implementation questions

(For the writing-plans phase, not blocking spec approval.)

- Should `bridgecfg` accept a generic `WithReceiver(name, plugin, opts)` escape
  hatch for plugin types it doesn't have first-class methods for?
- ALB attachment: do we expose the listener rules as separate constructs so
  consumers can override priorities?
- Init container failure exit codes: what surfaces best in CloudWatch Logs
  Insights?

## Dependencies & risks

- Depends on existing `config.ParseFile`, `config.MarshalYAML`, and
  `ports.BridgeConfig` schema stability. Any breaking change to `ports.*`
  ripples into `bridgecfg`.
- Risk: Tier B grant derivation may grow complex as new adapter types ship.
  Mitigation: each adapter type registers its grant-derivation function via
  a small interface; new adapters add one file in `cdk/constructs/internal/grants/`.
- Risk: ALB rule priority collisions with consumer's own listener. Mitigation:
  expose `BasePriority` prop and document the range used.
