# testutil ↔ floci — evaluation and plan

Status: **planning only**. Nothing here is implemented.
Scratch document — delete when done. Anything durable (an endpoint
convention, a build tag, a topology decision) must be promoted to an ADR or a
page under `docs/` **before** this file is removed. Nothing in the codebase may
reference this file.

---

## 1. Where the three packages are used today

Repo-wide grep for the import path (excluding each package's own directory):

| Package | Backing image | Go files importing it | Verdict |
|---|---|---|---|
| `testutil/localstack` (347 lines) | `localstack/localstack` (Community) | **3** | replaceable |
| `testutil/sqslocal` (351 lines) | ElasticMQ | **18** | keep (for now) |
| `testutil/s3local` (264 lines) | MinIO | **0** | dead — delete |

### `testutil/localstack` — 3 importers

- [adapters/aws/credentials/ssm/integration_test.go](adapters/aws/credentials/ssm/integration_test.go) — `WithServices("ssm")`, uses `Endpoint(t)`
- [adapters/aws/metrics/cloudwatch/integration_test.go](adapters/aws/metrics/cloudwatch/integration_test.go) — `WithServices("cloudwatch")`, uses `Endpoint(t)`
- [deployment/aws-filebased-config/lib/bootstrap/app_integration_test.go](deployment/aws-filebased-config/lib/bootstrap/app_integration_test.go) — `WithServices("ssm")`, uses `SSMClient(t)` + `Endpoint(t)`

Surface actually consumed: `Configure`, `WithServices`, `WithCleanOrphans`,
`Endpoint`, `SSMClient`, `Shutdown`. That is the whole contract to preserve.

### `testutil/sqslocal` — 18 importers

9 files in [tests/integration/](tests/integration/), 9 in
[tests/longrunning/](tests/longrunning/). Surface: `Configure`, `Client`,
`Endpoint`, `CreateQueue`, `UniqueQueue`, `Shutdown`, plus purge/attr helpers.

### `testutil/s3local` — 0 importers

No Go file imports it; the only reference anywhere is the `S3_ENDPOINT` row in
`DEVELOPMENT.md`. 264 lines of unexercised Docker lifecycle code.
(`testutil/testcontent` is in the same state — no Go importer, one mention in
`docs/timing-audit.md`. Flagged, out of scope here.)

### Not in scope

`mqttlocal` (68 importers), `rabbitmqlocal` (17), `artemislocal` (13),
`asblocal` (2), `ddblocal` (32), `tlsgen` (2), `wait` (165), `dockerexec` (13).
floci replaces AWS APIs, not brokers — see §5.1. All stay on `dockerexec`.

---

## 2. Can floci replace them?

[floci](https://github.com/floci-io/floci) — MIT, Quarkus-native, one process
on port `4566`, 85 emulated AWS services, standard `AWS_ENDPOINT_URL`, and a
[testcontainers-floci-go](https://github.com/floci-io/testcontainers-floci-go)
module. It is a LocalStack-Community replacement with no feature gates.

### API coverage — checked against what gobridge actually calls

Every operation below was taken from a grep of production adapters and CDK
constructs, then matched against floci's per-service operation lists.

**SQS** — [floci/services/sqs](https://floci.io/floci/services/sqs/), 23 ops.
Full coverage: `SendMessage`/`SendMessageBatch`, `ReceiveMessage`,
`DeleteMessage`(`Batch`), `ChangeMessageVisibility`(`Batch`), `GetQueueUrl`,
`Get`/`SetQueueAttributes`, `CreateQueue`, `PurgeQueue`, `TagQueue`, plus FIFO,
message attributes and redrive policy. Nothing gobridge sends to SQS is
missing.

**DynamoDB** — [floci/services/dynamodb](https://floci.io/floci/services/dynamodb/), 28 ops.

| gobridge calls | floci |
|---|---|
| `PutItem`, `GetItem`, `UpdateItem`, `DeleteItem` | ✅ |
| `Query`, `Scan`, `CreateTable`, `DescribeTable` | ✅ (GSI creation shown) |
| `TransactWriteItems` | ✅ ("ACID write transaction") |
| `UpdateTimeToLive`, `DescribeTimeToLive` | ✅ (API listed) |
| Streams: `DescribeStream`, `GetShardIterator`, `GetRecords`, `ListStreams` | ✅ `NEW_AND_OLD_IMAGES` supported |
| `ConditionExpression` → `ConditionalCheckFailedException` | ⚠️ **not documented** |
| Real TTL *expiry* (not just the config API) | ⚠️ **not documented** |

The two ⚠️ rows are load-bearing: 146 call sites in `adapters/` use
`ConditionExpression` / `ConditionalCheckFailed`, and the whole
`gobridgedynamodbha` lease/slot design rests on compare-and-swap. **These must
be probed before any DynamoDB work moves to floci.** See task T2.

**SSM Parameter Store** — [floci/services/ssm](https://floci.io/floci/services/ssm/), 22 ops.
`GetParameter`, `GetParameters`, `GetParametersByPath` (recursive),
`PutParameter`, `DeleteParameter`, `GetParameterHistory` — all ✅. One
divergence: **SecureString is stored in clear**. Our credential adapter reads
values and never asserts ciphertext, so this is cosmetic — but a test that
asserts encryption at rest would pass for the wrong reason.

**CloudWatch** — [floci/services/cloudwatch](https://floci.io/floci/services/cloudwatch/).
`PutMetricData`, `ListMetrics`, `GetMetricData` (math expressions),
`GetMetricStatistics`, `PutMetricAlarm`, `DescribeAlarms`, `DeleteAlarms`,
`SetAlarmState` — ✅. Two divergences: **alarms are not documented to
auto-evaluate** (drive them with `SetAlarmState`), and **Logs subscription
filters do not forward** to the destination ARN.

### Verdict per package

| Package | Action | Why |
|---|---|---|
| `s3local` | **delete** | 0 importers, 264 dead lines. Rung 1 of the ladder. |
| `localstack` | **replace with floci** | Only 3 importers and a 6-symbol surface. floci covers SSM + CloudWatch fully, starts in ~24ms vs LocalStack's cold start, is MIT with no Community feature gates, and the same container then unlocks §3. |
| `sqslocal` | **delete** | Superseded — see §5.2. Migrate the 18 call sites to floci and drop the package. |
| `ddblocal` | **keep** | DynamoDB Local is AWS's own emulator; its conditional-write and TTL semantics are the reference, and our HA design is compare-and-swap end to end. 32 importers. |

Net: delete one package, port three test files, leave the rest alone.

---

## 3. Deploying `deployment/aws-filebased-config` locally and running e2e on it

### What already exists

There is already a full deploy-and-assert suite:
[deployment/aws-filebased-config/cdk/integration/](deployment/aws-filebased-config/cdk/integration/),
build tag `integration_aws`. `DeployStack` synths the CDK app and shells out to
`cdk deploy --app <asm> --outputs-file …`, reads stack outputs, and registers a
`DestroyStack` cleanup. Tests then drive the deployed stack with the real AWS
SDK — e.g. `TestIntegration_Single_SQS_Roundtrip` sends to the inbound queue and
polls the outbound one.

It requires six `GOBRIDGE_INT_*` env vars, a real AWS account, a real VPC, and
real money. It is opt-in and almost never run.

**So question 3 is not "can we build this" — it is "can floci be the backend
for the harness we already have, so it runs on a laptop with no AWS account".**
That reframing is the whole point: the change is an endpoint and a bootstrap,
not a new test suite.

### CloudFormation coverage for our stacks

Resources our constructs emit, against
[floci/services/cloudformation](https://floci.io/floci/services/cloudformation/):

| Resource | floci | Note |
|---|---|---|
| `AWS::SQS::Queue` | ✅ | QueuePolicy accepted, not enforced |
| `AWS::SNS::Topic`, `::Subscription` | ✅ | |
| `AWS::DynamoDB::Table` | ✅ | |
| `AWS::SSM::Parameter` | ✅ | |
| `AWS::IAM::Role`, `::Policy` | ✅ | SigV4 + IAM auth emulated |
| `AWS::KMS::Key`, `::Alias` | ✅ | |
| `AWS::Logs::LogGroup` | ✅ | |
| `AWS::CloudWatch::Alarm` | ✅ | see alarm-evaluation caveat |
| `AWS::EC2::VPC`, `::Subnet`, `::SecurityGroup` | ✅ | |
| `AWS::ECS::Cluster`, `::TaskDefinition`, `::Service` | ✅ | tasks run as **real Docker containers** |
| `AWS::ElasticLoadBalancingV2::LoadBalancer`, `::TargetGroup`, `::Listener`, `::ListenerRule` | ⚠️ | listener binds a real socket, but **instance targets only** and **no ECS auto-registration** |
| `AWS::Lambda::Function`, `::EventSourceMapping` | ✅ | real Docker execution, Go runtime, SQS ESM polls and invokes |
| `AWS::EFS::FileSystem`, `::AccessPoint` | ❌ | **not in the supported CFN list** — accepted, synthetic physical ID, not provisioned |

CDK itself is supported: nested stacks, `Ref`/`Fn::GetAtt`, Outputs,
Lambda-backed custom resources and the CDK provider framework, bootstrap
values resolved from SSM. floci's own compatibility suite deploys a real CDK
stack via `cdklocal`.

### The three real gaps, and what each costs

**1. EFS — the load-bearing one.** This profile is *file-based config*: the
config file lives on EFS, the control task mounts an RW access point, workers
mount RO. floci's EFS service is control-plane only — "no actual NFSv4.1 data
plane". But the ECS service says something different and more useful: *"a
task's `efsVolumeConfiguration` volumes are backed by shared local Docker
volumes"*, with `floci.storage.efs.*` emulating access-point uid/gid/permission
behaviour. A shared Docker named volume across control + worker containers is
exactly the sharing semantic we need. **Unverified**: whether an EFS filesystem
created via CloudFormation (which floci does *not* provision) yields an id that
the ECS task definition can then bind to a Docker volume. This single question
decides whether §3 is "port the harness" or "port the harness plus a
local-only EFS shim". Probe it first — T3.

**2. ALB → task routing.** ELBv2 forwards to *instance* targets resolved via
EC2 private addresses, and nothing auto-registers an ECS service's tasks.
`gobridgealbattachment` will synth and deploy, but traffic will not reach the
bridge through the load balancer. Cost: HTTP e2e assertions hit the task's
container port directly instead of the ALB DNS name. The ALB construct is then
covered by synth assertions only, which is what it has today anyway.

**3. Alarms do not self-fire.** `gobridgealarms` deploys, but no metric volume
will transition an alarm to `ALARM`. Drive transitions with `SetAlarmState` and
assert the *consequence* (SNS delivery, rollback action) rather than the
threshold arithmetic. Threshold arithmetic stays a synth-level assertion.

Minor: ECS container stdout stays with the Docker container rather than
reaching the `awslogs` driver, so log assertions read `docker logs`, not
CloudWatch Logs.

### Deployments worth standing up

Each maps to a construct that already exists under `cdk/constructs/`.

| # | Deployment | Composition | Feasible on floci |
|---|---|---|---|
| D1 | **Single, SQS↔SQS** | 1 Fargate task, inbound + outbound queue, config on EFS | ✅ once EFS is settled |
| D2 | **Single, MQTT↔SQS** | 1 Fargate task + a Mosquitto container on the same Docker network, outbound queue | ✅ — `mqttlocal` supplies the broker, floci the queue |
| D3 | **Cluster** | control task (RW config) + N worker tasks (RO config), shared queues | ✅ — this is the shared-Docker-volume test |
| D4 | **DynamoDB HA, static slots** | cluster + DDB table for slots/leases | ⚠️ gated on conditional-write fidelity (T2) |
| D5 | **Producer/consumer Lambdas** | Go Lambda → inbound queue → bridge → outbound queue → Go Lambda via ESM | ✅ — closes the loop with no test-process SDK calls |
| D6 | **Alarms + SNS** | D1 plus `gobridgealarms` + SNS topic | ⚠️ alarms driven via `SetAlarmState` |
| D7 | **ALB-fronted HTTP** | D1 plus `gobridgealbattachment` | ⚠️ synth-only; direct container port for traffic |
| D8 | **Config rollout** | D3 plus an SSM-parameter or EFS config change mid-flight | ✅ |

### E2E test types to run against them

Grouped by what they actually pin down.

*Data-plane correctness*
- E1 SQS→SQS roundtrip: unique payload in, same payload out, within a window (D1). This is the existing test, unchanged except for the endpoint.
- E2 MQTT→SQS: publish to a topic, assert the mapped queue message and its `Subject`/`Address` mapping (D2).
- E3 SQS→MQTT reverse path (D2).
- E4 Message-attribute and header fidelity across the bridge (D1).
- E5 Batch send: 10-message `SendMessageBatch` in, 10 distinct out, no dupes (D1).
- E6 FIFO ordering and dedup within a message group (D1 with FIFO queues).
- E7 Large payload at the 256 KB boundary — note floci's SQS allows 1 MB, so the *rejection* case must stay a unit test against real limits.
- E8 Lambda-to-Lambda closed loop: producer Lambda writes, consumer Lambda records receipt in DynamoDB, test asserts the DDB row (D5). Nothing in the test process touches the queues.

*Deployment shape*
- E9 Deploy → assert stack outputs exist and are well-formed (all D).
- E10 Idempotent redeploy: `cdk deploy` twice, second is a no-op, service not replaced (D1, D3).
- E11 Destroy leaves nothing: post-`cdk destroy`, `ListQueues`/`ListTables`/`DescribeServices` are empty (all D).
- E12 Config-file placement: after deploy, the config the control task wrote is visible to a worker task (D3). The EFS sharing proof.
- E13 IAM least-privilege: with the task role's own credentials, a granted call succeeds and a non-granted call is denied. floci does SigV4 + IAM, so this is testable locally for the first time.

*Lifecycle and resilience*
- E14 Rolling config change: update the EFS config, assert workers pick it up without a redeploy (D8).
- E15 Task restart: `docker kill` the bridge container, ECS restarts it, in-flight messages are not lost (D1).
- E16 Scale worker count 1→3→1, assert no duplicate delivery (D3).
- E17 Slot claim on cold start: N workers, N slots, each claims exactly one (D4).
- E18 Lease failover: kill the slot holder, assert another worker takes the slot within the lease TTL (D4).
- E19 Split-brain guard: two workers race for one slot, exactly one wins (D4). **This is the test that dies if floci's conditional writes are approximate — and it would die by passing.**
- E20 DLQ redrive: poison message exceeds `maxReceiveCount`, lands on the DLQ, redrive returns it (D1).

*Observability*
- E21 Metrics published: after a roundtrip, `ListMetrics`/`GetMetricData` show the bridge's namespace (D1).
- E22 Alarm consequence: `SetAlarmState(ALARM)` → SNS subscription receives the notification (D6).
- E23 Health endpoint: `/healthz` on the container port reports ready after deploy (D1).

**Suggested first slice — D1 + E1 + E9 + E11.** That is the existing
`TestIntegration_Single_SQS_Roundtrip` running with no AWS account. If that
green, everything else is incremental.

### Shape of the change

Not a new suite. A second backend for the harness we have:

- floci started by `testcontainers-floci-go`, one instance per test binary (§4.1).
- `RequireSandbox` grows a local mode: `GOBRIDGE_INT_LOCAL=1` synthesizes a
  `SandboxEnv` pointing at floci (account `000000000000`, `us-east-1`, a VPC
  created against floci's EC2 API) instead of skipping on missing
  `GOBRIDGE_INT_*`.
- `DeployStack`/`DestroyStack` run `cdklocal` — one branch, not a fork.
- Build tag `integration_local`, so `integration_aws` keeps meaning "real
  account, real money" and stays untouched.
- `make test-local-deploy`, following the `test-integration` pattern.

The local harness goes beside the existing one under `cdk/integration/`;
durable outcomes land in `docs/` or an ADR.

---

## Open questions

*(§4 and §5 resolved most of the original list. What remains:)*

1. Can a CloudFormation-declared EFS filesystem + access point be referenced by
   an ECS task definition and resolve to a shared Docker volume? Blocks D1/D3.
2. Is `cdk bootstrap` needed against floci, or does `cdklocal` shortcut it?
3. Does floci enforce `ConditionExpression`? No longer blocking (§4.2) — it
   only decides whether the DynamoDB Local split is permanent or temporary.

---

## 4. Follow-up decisions

Answers to the round-2 questions. These supersede the corresponding rows in §2
and §3 where they conflict.

### 4.1 Testcontainers instead of `testutil/*` wrappers?

Facts that decide it:

- `testcontainers-go` is **not currently a dependency** anywhere in the repo.
- Every `testutil/*` helper is **its own Go module** (`testutil/sqslocal/go.mod`,
  `testutil/localstack/go.mod`, …) and **none of them are in
  `scripts/release/modules.json`** — 31 published modules, zero of them
  `testutil`. So a heavy test-only dependency added inside one of these modules
  costs downstream consumers nothing. The earlier worry about dep-tree
  pollution does not apply.
- `testutil/dockerexec` (441 lines) has no `go.mod` — it lives in the **root**
  module, and each helper module pulls it in via `replace … => ../..`.
- `dockerexec` is used by 13 files across `mqttlocal`, `rabbitmqlocal`,
  `artemislocal`, `asblocal`, `ddblocal`, `sqslocal`, `localstack`. Adopting
  testcontainers **does not delete it** — the non-AWS brokers still need it.

Decision:

| Where | What | Why |
|---|---|---|
| Deployment harness (`cdk/integration/`, Phase 3) | **`testcontainers-floci-go`** | It is a separate, unpublished module; floci's ECS and Lambda executors need a dedicated Docker network, which the module handles via `WithDedicatedNetwork()`. No new `testutil` package is created — the lifecycle lives in the harness, as asked. |
| `testutil/localstack`'s 3 call sites | **`testcontainers-floci-go`, no new wrapper** | The `TestMain` shape (`NewFlociContainer().Start(ctx)` … `fc.Stop(ctx)`) is the same 6 lines the current `Configure`/`Shutdown` pair provides. Three call sites do not justify a fourth wrapper package. **Task T8 is cancelled** — do not write `testutil/flocilocal`. |
| `testutil/sqslocal`'s 18 call sites | **`testcontainers-floci-go`, delete the package** | Superseded by §5.2. |
| `mqttlocal`, `rabbitmqlocal`, `artemislocal`, `asblocal`, `ddblocal` | **unchanged, `dockerexec`** | Not AWS. Rewriting five working helpers onto a second container idiom buys nothing and leaves two idioms in the tree. |

So: yes to testcontainers, scoped to floci, and yes — no new `testutil`
package is needed for the deployment work.

### 4.2 DynamoDB Local alongside floci

**floci has no proxy or passthrough for DynamoDB.** Confirmed against
[floci/services/dynamodb](https://floci.io/floci/services/dynamodb/): the only
knobs are `FLOCI_SERVICES_DYNAMODB_ENABLED` and
`FLOCI_STORAGE_SERVICES_DYNAMODB_MODE`. There is no external-endpoint setting.

But a proxy is not needed. The AWS SDKs honour
**`AWS_ENDPOINT_URL_<SERVICE>`**, which takes precedence over the global
`AWS_ENDPOINT_URL` for that one service
([AWS SDKs and Tools reference](https://docs.aws.amazon.com/sdkref/latest/guide/feature-ss-endpoints.html)).
So the bridge container gets:

```
AWS_ENDPOINT_URL=http://floci:4566
AWS_ENDPOINT_URL_DYNAMODB=http://ddblocal:8000
```

and every DynamoDB call — leases, slots, outbox, DLQ, rollout, managed
subscriptions — lands on Amazon's own emulator with real conditional-write
semantics, while SQS/SSM/CloudWatch/ECS stay on floci. No code change: this is
resolved by `config.LoadDefaultConfig` from the environment.

The one thing it costs: **`AWS::DynamoDB::Table` in the CFN stack is created in
floci's DynamoDB, not in DynamoDB Local.** Two ways out:

- (a) `FLOCI_SERVICES_DYNAMODB_ENABLED=false`, so the CFN resource falls into
  floci's "accepted, synthetic physical ID" bucket, and the harness creates the
  table in DynamoDB Local from the same schema.
- (b) Let floci create it, then mirror the schema into DynamoDB Local after
  deploy.

Either way "CloudFormation provisioned the table" stops being an e2e assertion
and stays a synth assertion — which is what it already is today. That is a good
trade: the thing E17–E19 exist to prove is compare-and-swap under contention,
not that CDK emits a table resource.

**Does this mean everything works if conditional writes were supported?** No —
it unblocks D4 and E17–E19 specifically. EFS (§4.3), ALB registration (§4.4),
alarm evaluation (§4.5) and the `awslogs` driver are independent gaps. Routing
DynamoDB to DynamoDB Local sidesteps the DynamoDB question **without waiting
for floci**, which makes T2 informative rather than blocking. Adjusted: run T2
anyway to learn the truth, but D4 is no longer gated on it.

### 4.3 EFS workaround in CDK/CFN "test mode"

Yes — but not as an `if testMode` branch inside `GoBridgeEfsConfig`. Test
branching does not belong in production infrastructure code; it changes the
artefact under test and the local run stops proving anything about the real
one.

The established pattern in this repo is a **CDK Aspect applied by the harness**:
`ApplyDestroyAspect(stack)` already does exactly this in
`cdk/integration/harness.go`. The local harness adds one more aspect that
rewrites the synthesized task definition, swapping the `efsVolumeConfiguration`
volume for a Docker volume. The construct source is untouched; only the local
harness knows about it, and the aspect is a handful of lines in a file that
never ships.

**This may not be needed at all.** floci's ECS says a task's
`efsVolumeConfiguration` volumes are backed by shared local Docker volumes.
T3 decides: if the CFN-declared filesystem resolves through to a shared Docker
volume, there is no workaround, just a config value. The aspect is the
fallback, not the plan.

### 4.4 Is the ALB gap a problem?

Largely no, and it is worth being precise about what is lost.

Still covered, unchanged: target-group properties, health-check path, interval
and thresholds, listener rules, security-group rules. All synth assertions —
which is exactly the coverage `gobridgealbattachment` has today. Nothing
regresses.

Lost: proof that a request reaches the bridge *through* the load balancer. That
is AWS's routing implementation, not ours, and it is not a plausible source of
a gobridge bug.

The one real risk that hides in the gap is a **health-check path that the
container does not actually serve** — config drift between the CDK props and
the HTTP adapter's routes. That is cheap to close without a load balancer:
assert that the synthesized target-group health-check path equals a path the
running container answers with 200. One test, no ALB, and it catches the actual
failure mode. Added as **E24**.

### 4.5 Can the alarm not be tested?

Most of it can. Split the alarm into three things and only one is unavailable:

| Part | Ours or AWS's? | Local coverage |
|---|---|---|
| Alarm **definition** — threshold, period, evaluation periods, math expression, treat-missing-data | ours | ✅ synth assertions (already exist) |
| Alarm **arithmetic** — does the configured threshold actually trip on realistic traffic? | ours | ✅ replay the same math expression through `GetMetricData` against real `PutMetricData` volume and assert the computed value crosses the threshold. floci supports metric math. This tests the *threshold choice*, which is the part most likely to be wrong. |
| Alarm **consequence** — SNS notification, rollback action | ours | ✅ `SetAlarmState(ALARM)` and assert downstream |
| Alarm **state machine** — CloudWatch's evaluation timing and transition rules | AWS's | ❌ not emulated, and not our bug surface |

So the untestable residue is AWS's own evaluator. Everything gobridge decides
about alarms is reachable. Added as **E25** (metric-math threshold replay).

### 4.6 Docker Compose?

Split answer.

**No for `testutil/*`.** Those helpers deliberately use
`dockerexec.FreePort()` and one container per test *binary*, so packages
running in parallel cannot collide on a port or inherit another package's
queue state. Compose means fixed ports and one shared lifecycle across the
whole run — reintroducing exactly the cross-package interference `TESTS.md`
exists to prevent. Do not convert them.

**Yes for the deployment harness.** There, containers must resolve each other
by stable DNS name on one network: floci launches the ECS task container, and
that container needs to reach `mosquitto` and `ddblocal` by hostname, and floci
itself at `floci:4566`. Compose expresses that declaratively; doing it through
testcontainers means building the network and aliases imperatively. One
`compose.yaml` under `deployment/aws-filebased-config/`, brought up and torn
down by `make test-local-deploy`, scoped with a per-run project name so
concurrent runs do not share state.

If T4/T14 show testcontainers' `WithDedicatedNetwork()` handles the aliasing
cleanly on its own, prefer that and drop the compose file — one mechanism beats
two. Decide with evidence in T14, not now.

### 4.7 Additional tests from this round

- **E24 — health-check path parity.** The synthesized target-group health-check
  path must be a path the running container answers with 200. Closes the only
  real risk the ALB gap leaves open (§4.4).
- **E25 — metric-math threshold replay.** Drive real traffic, `PutMetricData`
  as production does, then evaluate the alarm's own math expression via
  `GetMetricData` and assert it crosses the configured threshold (§4.5).

---

## 5. Round-3 answers

### 5.1 floci instead of Artemis? No.

floci's service matrix lists Amazon MQ as
`/v1/brokers/... + RabbitMQ broker | REST JSON + AMQP | 5 operations` — a
five-operation control plane in front of a **RabbitMQ** broker. Artemis is
ActiveMQ, a different broker with different AMQP 1.0 semantics, and the
`adapters/amqp/transport/amqp10` suite (8 integration files, plus 4 in
`tests/longrunning`) tests exactly those semantics: durable subscriptions,
sibling addresses, edge and retry behaviour.

Even if RabbitMQ's AMQP 1.0 support were sufficient, the trade is bad: we would
put an AWS control-plane API in front of a broker we currently start directly
with one `docker run`. That is more moving parts for the same broker.

`testutil/artemislocal` stays. Same for `mqttlocal`: floci's IoT Data is a
REST-JSON shadow/publish API over `/things/{name}/shadow` and MQTT *topics* —
not an MQTT wire-protocol broker on 1883. It cannot serve the Paho client.

Rule of thumb: **floci replaces AWS APIs, not brokers.** Every non-AWS helper in
`testutil/` stays on `dockerexec`.

### 5.2 `sqslocal` — delete it (decision changed)

Earlier advice here was to keep the package and swap only the image. Overruled:
`sqslocal` is deleted and its 18 call sites move to `testcontainers-floci-go`
directly, same as the `localstack` three.

What that buys: **one container idiom for AWS instead of two**, 962 lines
deleted across `s3local` + `localstack` + `sqslocal`, and no wrapper standing
between a test and the emulator it uses. What it costs: 18 files change their
`TestMain` and imports — mechanical, and the deployment work in §3 needs floci
on those same binaries anyway.

The one thing the wrapper did that the container does not is `UniqueQueue`.
That is a name-generation helper, not container lifecycle; it moves into the
calling package. Do not resurrect a wrapper package to hold it.

`ddblocal` is the exception and stays (§4.2): DynamoDB does not move to floci
at all.

### 5.3 Which DynamoDB does the container hit?

Not floci — and the reason is stronger than the env-var trick.

**`docs/configuration-reference.md` §DynamoDB store options states it outright:**

> The AWS region and endpoint are NOT store options: they come from the standard
> AWS SDK configuration chain (environment, shared config, IAM role). Supplying
> `options.region` or `options.endpoint` is rejected by the strict decoder.

So a DynamoDB store's endpoint has exactly one source — the SDK chain — and no
config file can override it. The adapters call
`awsconfig.LoadDefaultConfig(ctx, …)` and set `cfg.BaseEndpoint` only when a
plugin config supplies an endpoint, which for DynamoDB is impossible by design.

SDK resolution order, highest wins:

| # | Source | Real AWS | Local mode |
|---|---|---|---|
| 1 | `cfg.BaseEndpoint` set in code | unset for DynamoDB (decoder rejects it) | same |
| 2 | `AWS_ENDPOINT_URL_DYNAMODB` | unset | **`http://ddblocal:8000`** |
| 3 | `services.*.endpoint_url` in shared config | unset | unset |
| 4 | `AWS_ENDPOINT_URL` | unset | `http://floci:4566` |
| 5 | regional default | `dynamodb.<region>.amazonaws.com` | — |

Rows 2 and 4 are the only difference between the two columns. The container
runs the identical binary through the identical code path; the environment
differs, exactly as it would when pointing production at a VPC endpoint.

One asymmetry worth knowing: the **SQS transport** *does* expose an `endpoint`
plugin key (`adapters/aws/transport/sqs/config_plugin.go:25`), which sits at
row 1 and beats the env vars. Leave it empty in the local config so row 4
applies — do not hardcode `http://floci:4566` there, or the same config file
stops working against real AWS.

### 5.4 Post-deploy schema mirror

Written out in `testutil/PLAN_DDB_MIRROR.md` — the `MirrorTable` function,
what it deliberately does not copy, and the two caveats T12b must carry.
CloudFormation still creates the table (so the deploy path is exercised and
`AWS::DynamoDB::Table` is proven to provision); the mirror copies the resulting
schema to DynamoDB Local, where the data plane actually runs.
