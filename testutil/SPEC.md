# Local AWS emulation and deployment e2e — specification

Decisions are **locked** unless a probe in §7 disproves them. Rationale and the
evidence behind each decision live in `testutil/PLAN.md`; this file is what to
build. `testutil/PLAN_DDB_MIRROR.md` holds the one drafted function.

Executable work is Chunks 26, 27 and 28 in `PROD_READY_PLAN.md`, in the order
**27 → 26 → 28**: retire the wrappers, then build the deployment harness on the
emulator that migration proved, then extend the matrix. Nothing in the
codebase may reference this file; promote durable outcomes to an ADR or a
`docs/` page before deleting it.

---

## 1. Emulator split

| Concern | Backend | Why |
|---|---|---|
| SQS, SSM, CloudWatch, CloudFormation, ECS, IAM, STS, ELBv2, Lambda, EC2, SNS, KMS, Logs | **floci** (`floci/floci:latest`, port 4566) | MIT, 85 services on one endpoint, real Docker execution for ECS and Lambda, CDK/CloudFormation support |
| **DynamoDB** | **DynamoDB Local** (`amazon/dynamodb-local`, the image `testutil/ddblocal` already pins) | Amazon's own emulator. Conditional-write and TTL semantics are the reference, and floci does not document `ConditionExpression` behaviour at all. The HA slot/lease design is compare-and-swap end to end — 146 call sites in `adapters/` depend on it. |
| MQTT, AMQP 0-9-1, AMQP 1.0, Azure Service Bus | unchanged (`mqttlocal`, `rabbitmqlocal`, `artemislocal`, `asblocal`) | floci replaces AWS APIs, not brokers. Its Amazon MQ is a 5-operation control plane over RabbitMQ; its IoT Data is a REST-JSON shadow API, not an MQTT broker on 1883. |

## 2. DynamoDB endpoint routing

Containers and test processes get both variables:

```
AWS_ENDPOINT_URL=http://floci:4566
AWS_ENDPOINT_URL_DYNAMODB=http://ddblocal:8000
```

`AWS_ENDPOINT_URL_<SERVICE>` outranks the global `AWS_ENDPOINT_URL` in the SDK
chain, so DynamoDB alone diverges. **No gobridge code changes**:
`docs/configuration-reference.md` already forbids `options.endpoint` on
DynamoDB stores ("rejected by the strict decoder"), so the SDK chain is the only
source and nothing in a config file can override it.

One asymmetry: the SQS transport **does** expose an `endpoint` plugin key
(`adapters/aws/transport/sqs/config_plugin.go`), which outranks both env vars.
It MUST be left empty in local configs, or the same config stops working
against real AWS.

## 3. Container lifecycle

- **floci** → `testutil/flocilocal`, one container per test binary, started in
  `TestMain`, on `testutil/dockerexec` like every other container helper.
- **Everything else** → `testutil/dockerexec`, unchanged.

This reverses an earlier decision to depend on
`github.com/floci-io/testcontainers-floci-go` directly. Two facts moved it:

- `adapters/aws/credentials/ssm` and `adapters/aws/metrics/cloudwatch` are
  **published** modules. The migration puts the emulator dependency in *their*
  `go.mod`, not in a `testutil/*` one — so `testcontainers-go` and its ~60
  indirect modules would ship in two published manifests. The earlier claim that
  it "costs consumers nothing" rested on the dependency living in `testutil/*`,
  which it does not.
- `testutil/localstack` was already a `bootstrap_module` in
  `scripts/release/modules.json`. `flocilocal` simply takes its slot, so the
  release ceremony is unchanged either way — the public module bought nothing
  there.

What the module actually contributed was about fifteen lines: run the image,
expose 4566, wait for health, hand back an endpoint. `dockerexec` already does
that, with digest pinning, `MustSucceed`, orphan sweeping and log-on-failure
that the module has no equivalent for, and `TESTS.md` §5 requires new container
dependencies to arrive as a `testutil/<thing>local` package anyway. Chunk 26's
`WithDedicatedNetwork()` becomes `docker network create` plus the
`FLOCI_SERVICES_DOCKER_NETWORK` env var.

## 4. `testutil/` changes

**Delete** (962 lines):

| Package | Importers to migrate |
|---|---|
| `s3local` | 0 — dead code. Also drop the `S3_ENDPOINT` row from `DEVELOPMENT.md`. |
| `localstack` | 3 — `adapters/aws/credentials/ssm`, `adapters/aws/metrics/cloudwatch`, `deployment/aws-filebased-config/lib/bootstrap` |
| `sqslocal` | 18 — 9 in `tests/integration/`, 9 in `tests/longrunning/` |

Each migrated file repoints its `Configure`/`Shutdown` pair at `flocilocal`
and builds clients from `flocilocal.AWSConfig(t)`. The change is mechanical;
queue plumbing (`UniqueQueue`, `CreateQueue`, `CreateQueueWithAttrs`) moves into
the calling package — do not resurrect a per-service wrapper package for it.

**Keep unchanged:** `ddblocal`, `mqttlocal`, `rabbitmqlocal`, `artemislocal`,
`asblocal`, `tlsgen`, `wait`, `dockerexec`.

**Kept:** `testcontent`. It has no Go importer, but it is pure assertion logic
with its own tests, no container and no release-bootstrap cost, and it is
exactly the TID-based lost/duplicate accounting the deployment e2e matrix needs
(§8 E5, E15, E16). The real defect was discoverability, so it is now listed in
`TESTS.md` §7 alongside the other fixtures.

**Watch for:** floci stores SecureString parameters in clear. Any assertion that
depends on encryption at rest must be rewritten or moved — a test that passes
for the wrong reason is a regression, not a migration.

## 5. Deployment harness

Extends `deployment/aws-filebased-config/cdk/integration/`, which already
synthesizes, shells out to `cdk deploy --outputs-file`, asserts against stack
outputs and destroys. This adds a second backend to that harness — not a second
harness.

- **Build tag `integration_local`.** `integration_aws` keeps meaning "real
  account, real money" and is not touched.
- **`RequireSandbox` gains a local branch.** `GOBRIDGE_INT_LOCAL=1` synthesizes
  a `SandboxEnv` (account `000000000000`, `us-east-1`, VPC and subnets created
  against floci's EC2 API) instead of skipping on missing `GOBRIDGE_INT_*`.
- **`DeployStack`/`DestroyStack`** run `cdklocal`; the outputs-file contract is
  unchanged.
- **Post-deploy `MirrorTable`** copies each table's schema from floci to
  DynamoDB Local (`testutil/PLAN_DDB_MIRROR.md`). CloudFormation still creates
  the table, so the deploy path stays proven; the data plane runs where
  conditional writes are real.
- **Network.** Containers must resolve each other by name — floci launches the
  ECS task container, which must reach `mosquitto`, `ddblocal` and `floci:4566`.
  Prefer a dedicated Docker network created by the harness, with `flocilocal`
  joining it and floci given `FLOCI_SERVICES_DOCKER_NETWORK` so the task
  containers it launches land there too; fall back to one `compose.yaml` under
  `deployment/aws-filebased-config/` with a per-run project name. **One
  mechanism, not both.**
- **`make test-local-deploy`**, following the `test-integration` pattern: full
  log to `reports/`, prints command/status/count/duration.

`testutil/*` stays on per-binary containers and `dockerexec.FreePort()` —
do **not** convert it to compose. Fixed ports and a shared lifecycle across
parallel packages is the flake class `TESTS.md` exists to prevent.

## 6. Known gaps and required mitigations

| Gap | Mitigation | Residual |
|---|---|---|
| **EFS has no NFS data plane.** `AWS::EFS::*` is absent from floci's CloudFormation resource list, but ECS backs `efsVolumeConfiguration` with shared local Docker volumes. | Probe P1. If CFN does not carry through, add a harness-side CDK Aspect that rewrites the synthesized task definition's volume — beside the existing `ApplyDestroyAspect`, **never** as a test-mode branch inside `GoBridgeEfsConfig`. | none if P1 passes |
| **ELBv2 does not route to ECS tasks** — instance targets only, no auto-registration. | HTTP traffic hits the container port directly. ALB stays synth-asserted, as it is today. Add **E24**: the synthesized target-group health-check path must be a path the container answers 200 on. | AWS's own LB routing — not a gobridge bug surface |
| **Alarms do not auto-evaluate.** | Definition stays synth-asserted; consequence driven by `SetAlarmState`. Add **E25**: replay the alarm's own math expression through `GetMetricData` against real `PutMetricData` volume and assert it crosses the threshold. | CloudWatch's evaluation state machine — AWS's code |
| **Container stdout does not reach the `awslogs` driver.** | Log assertions read `docker logs`. | none material |
| **SQS max message size is 1 MB, not 256 KB.** | The 256 KB *rejection* case stays a unit test against the real limit. | none |
| **FIFO deduplication has no time window.** Duplicates are suppressed only while the original is still in the queue; real SQS remembers a deduplication id for five minutes whether or not the message was consumed. Measured, not assumed: two identical sends with no consumer yield one message, but the same pair with a drain in between yields two. | A test that means to exercise dedup must enqueue both the originals and the duplicates before any consumer starts. | Amazon's five-minute window, which no local run asserts |

## 7. Probes — run before Chunk 27, and P1 before Chunk 26

Spike code lives in the scratchpad, never the repo; binaries get a `.out` suffix.
Each probe's answer is recorded in `testutil/PLAN.md` §7.

- **P1 — EFS through ECS. BLOCKING for Chunk 26**, and runnable in parallel
  with Chunk 27. Deploy an EFS filesystem, an access point
  and two ECS tasks mounting it; write from task A, read from task B. Decides
  whether §6's Aspect is needed.
- **P2 — CDK deploy.** `cdklocal bootstrap` + `deploy` + `--outputs-file` +
  `destroy` of a trivial stack. Records whether bootstrap is required.
- **P3 — Go Lambda.** A `provided.al2023` function with an SQS event source
  mapping is actually invoked. Gates D5/E8 only.
- **P4 — DynamoDB semantics on floci.** *Informative, not blocking.* Conditional
  `PutItem`/`UpdateItem`, a failing `TransactWriteItems`, and real TTL expiry.
  Decides whether the DynamoDB Local split is permanent or transitional.

## 8. Topologies and test matrix

| # | Topology | Status |
|---|---|---|
| D1 | Single Fargate task, SQS in/out, config on EFS | **landed** — `TestLocal_SQSDataPlane` |
| D2 | Single task, MQTT↔SQS (`mqttlocal` broker on the shared network) | **landed** — `TestLocal_MQTTSubjectAndAddressMapping` |
| D3 | Cluster: control task RW config + N worker tasks RO | **landed** — `TestLocal_ClusterSharedConfigAndScaling` |
| D4 | DynamoDB HA, static slots and leases | **landed** — `TestLocal_StaticSlotCohort` |
| D5 | Go producer/consumer Lambdas either side of the bridge | **not local** — P3 not run; stays on `integration_aws` |
| D6 | Alarms + SNS | **landed** — `TestLocal_DeadLetterAndAlarms` |
| D7 | ALB attachment | synth only, plus E24 against the container |
| D8 | Config rollout over D3 | **landed** — `TestLocal_StaticSlotCohort` |

The per-behaviour outcome, and the measured reason behind every entry that has
no local test, is `docs/aws-deployment/local-deployment-suite.md`. That page is
the durable record; this table is the design's own view of it.

**Data plane** — E1 SQS↔SQS roundtrip · E2 MQTT→SQS with `Subject`/`Address`
mapping · E3 SQS→MQTT · E4 attribute/header fidelity · E5 batch-of-10 without
duplicates · E6 FIFO ordering and dedup · E8 Lambda-to-Lambda closed loop
asserted in DynamoDB.

**Deployment shape** — E9 outputs well-formed · E10 idempotent redeploy, service
not replaced · E11 destroy leaves nothing · E12 control-written config visible
to a worker (the EFS sharing proof) · E13 least-privilege: the task role's own
credentials pass a granted call and are denied a non-granted one.

**Lifecycle and resilience** — E14 rolling config change picked up without
redeploy · E15 task restart loses no in-flight message · E16 worker scale
1→3→1 without duplicate delivery · E17 N workers claim N slots, one each ·
E18 lease failover within the TTL after killing the holder · E19 split-brain:
two workers race one slot, exactly one wins · E20 DLQ redrive.

**Observability** — E21 metrics visible via `ListMetrics`/`GetMetricData` ·
E22 `SetAlarmState` reaches the SNS subscription · E23 `/healthz` ready after
deploy · E24 health-check path parity (§6) · E25 metric-math threshold
replay (§6).

## 9. Non-goals

- Replacing any non-AWS broker helper.
- Retiring `integration_aws`. The credentialed suite stays; local runs do not
  claim credentialed evidence.
- Proving AWS's own behaviour — ELB routing, CloudWatch alarm evaluation, ECS
  task-replacement identity. floci runs ECS task definitions as real Docker
  containers, so a local run proves the **synthesized shape wires identity
  correctly**; it does not prove AWS ECS behaves that way. Any published claim
  must say which of the two it rests on.
- Converting `testutil/*` to Docker Compose.
