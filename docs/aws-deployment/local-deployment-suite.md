# Running the deployment suite locally

The `aws-filebased-config` CDK profile can be deployed and driven with **no AWS
account and no credentials**. The same stack the credentialed suite deploys is
synthesized, handed to `cdklocal`, and stood up on local emulators; the tests
then talk to the running system.

```bash
make test-local-deploy
```

Requirements: Docker and Node. The target installs the CDK CLI wrapper into
`.tools/local-deploy/` and writes the full log to `reports/test-local-deploy.log`.
The harness builds the current checkout once for each distinct embedded
configuration staged by the synthesized stack.

## What it stands on

| Piece | Served by | Why |
|---|---|---|
| Every AWS API except DynamoDB | one emulator container | one endpoint, one lifecycle |
| DynamoDB | Amazon's own DynamoDB Local | the HA slot and lease design is compare-and-swap end to end, and an emulator whose `ConditionExpression` semantics are undocumented could accept a write that must fail |
| MQTT broker | Mosquitto | the emulator replaces AWS APIs, not brokers |

The split is the SDK's own: `AWS_ENDPOINT_URL_DYNAMODB` outranks
`AWS_ENDPOINT_URL`, so the container runs the identical binary through the
identical code path. It is permanent rather than provisional — the reason for it
is that conditional-write semantics have to come from the reference
implementation, which no amount of good behaviour from a general emulator
supplies. All containers join one per-run Docker network so a deployed task
resolves the emulator, DynamoDB Local and the broker by name.

## Driving a run

```bash
# The whole suite.
make test-local-deploy

# One DynamoDB-config scenario, including a fresh runtime image build.
make test-local-deploy LOCAL_DEPLOY_RUN='^TestLocal_DynamoDBConfigHotReload$'

# One topology, with the tools already installed; build its runtime from this checkout.
PATH="$PWD/.tools/local-deploy/node_modules/.bin:$PATH" \
GOBRIDGE_INT_LOCAL=1 \
go -C deployment/aws-filebased-config/cdk test -tags=integration_local -v \
  ./integration/ -run TestLocal_SQSDataPlane
```

`LOCAL_DEPLOY_RUN` accepts Go's `-run` expression, including slash-separated
subtest selectors. It defaults to the whole suite; a selector matching no tests
fails the target rather than reporting a proof that never ran.

`GOBRIDGE_INT_KEEP=1` leaves the deployed stack and everything it runs on in
place for a post-mortem. Without it every container, network and temporary
directory the run created is removed when the last test finishes; a run that
crashed is reclaimed by the next one through its own network's membership.

Reading a failure: the harness prints the deployed containers' own logs whenever
a member never becomes ready, and fails immediately — rather than waiting out
the budget — when a member reports that its configuration was refused.

## How the initial configuration reaches the runtime

Local fixtures use the same `ImageFromGoBuild` materialization as production.
CDK serializes the parsed bridge configuration into the staged image's
`initial-config-<sha256-of-raw-config>.base64` file. It contains only standard
Base64, not linker flags or environment settings. The runtime's
`main.initialConfigBase64` string reads the fixed `initial-config.base64`
placeholder through Go's native `go:embed`. Local builds supply that file through
a file overlay; production CDK builds supply it in a writable copy of the
published Go module, without requiring a Git checkout.

The payload stays in files throughout compilation, avoiding operating-system
limits on individual command arguments and environment variables. This is
build-time embedding, not an environment-based runtime initializer.

Before deployment, the harness reads each runtime image asset's embedded bytes,
checks their SHA-256 against the Base64 asset filename,
places them in a private `.gobridge-initial-*.yaml` file in the repository root,
and builds the root Dockerfile with `INITIAL_CONFIG_FILE` and the asset's
platform. The runtime's `-initial-config-digest` command must report the SHA-256
of those exact bytes. The harness then replaces the task definition's image
reference and removes only that runtime asset's publication entry. Lambda and
other deployment assets keep their normal publication paths.

The local fixture's `v0.0.0` module version is a synthesis placeholder, not a
published module fetched for the build. Only the current checkout is built. No
registry push or Docker Hub digest is needed for this never-published image.
Each generated input file is registered for cleanup as soon as it is created;
the harness removes only files and runtime images it owns.

The deployed control process initializes a missing file or DynamoDB item through
the real config loader. Workers only read it. The harness does not prepopulate
either target. An existing document wins over the embedded value; initialization
must never overwrite it. There is no configuration seeder container, separate
configuration image, or S3 configuration asset.

`GOBRIDGE_LOCAL_IMAGE` remains an explicit prebuilt-image override. It skips the
local build only if the image's `-initial-config-digest` matches the synthesized
fixture's embedded payload. An empty, mismatched, or unsupported digest fails
before deployment with instructions to unset the override. A generic unconfigured
runtime image is not sufficient, and one override cannot cover fixtures whose
embedded configurations differ. The harness does not rebuild, retag, or delete
an image supplied through this override.

Credentialed fixtures instead require `GOBRIDGE_INT_VERSION`, naming a published
module version that supports embedded initialization and the digest command.
CDK builds and publishes their config-bearing runtime assets normally.
`GOBRIDGE_INT_IMAGE` is rejected rather than silently ignored: those fixtures
cannot put their newly synthesized configuration into an already-built generic
registry image. The existing AWS sandbox and HA opt-in gates still apply.

## What a local run proves, and what it does not

It proves the **runtime contract on a deployed stack**: the config the
deployment embeds is initialized by the runtime and is the config the tasks run,
the routes carry messages, the
cohort protocol reaches agreement, the task role exists and can make the calls
its transports make. Because the emulator runs each ECS task definition as a
real container, it also proves the synthesized shape **wires** identity
correctly.

It does **not** prove AWS's own behaviour. The emulator drops task-definition
volumes and mount points, serves no task metadata, and cannot back EFS; the
harness restores these on the deployed stack and says so where it does. Runtime
initialization does not depend on a separate container's startup ordering.
An unavailable configuration target keeps the process live but unready until
the target can be read or initialized.

Any published claim must name which half it rests on.

## The matrix

Every topology and behaviour the suite covers, and for the ones it cannot cover
locally, the measured reason.

### Topologies

| # | Topology | Local status |
|---|---|---|
| D1 | Single task, SQS in/out, config on shared storage | `TestLocal_SQSDataPlane` |
| D2 | Single task, MQTT↔SQS; the MQTT ingress on a durable session holding the source, with its SQLite managed-subscription store on the mount | `TestLocal_MQTTSubjectAndAddressMapping` |
| D3 | Control task read-write + worker tasks read-only | `TestLocal_ClusterSharedConfigAndScaling` |
| D4 | DynamoDB HA, static member slots and leases | `TestLocal_StaticSlotCohort` |
| D5 | Go producer/consumer Lambdas either side of the bridge | `TestLocal_LambdaProducerAndConsumer` |
| D6 | Alarms and SNS | `TestLocal_DeadLetterAndAlarms` — the alarms are deployed, their queries replayed against real volume, and the topic's subscription proved; only the alarm→action step is not observable here (see *Emulation gaps*) |
| D7 | Load balancer attachment | synth only — see *Emulation gaps* |
| D8 | Config rollout over a static-slot cohort | `TestLocal_StaticSlotCohort` |
| D9 | DynamoDB HA with the DynamoDB config source, no EFS or file config | `TestLocal_DynamoDBConfigHotReload` |

### Behaviours

| Area | Covered locally | Test |
|---|---|---|
| Data plane | SQS↔SQS round trip, batch of ten without duplicates | `TestLocal_SQSDataPlane` |
| Data plane | MQTT→SQS and SQS→MQTT with `Subject` preserved and the binding's `Address` honoured | `TestLocal_MQTTSubjectAndAddressMapping` |
| Delivery mode | an MQTT ingress on a durable session runs `direct_hold` — no outbox, no lease, no outbox partition — and its SQLite managed-subscription store lives on the deployed mount | `TestLocal_MQTTSubjectAndAddressMapping` — the deployed task reports the mode and a lease-less, connected ingress session, messages cross the route, and the store file is on the mount |
| Data plane | a Go producer Lambda, the bridge and a Go consumer Lambda carry messages end to end | `TestLocal_LambdaProducerAndConsumer` — the producer is invoked directly, the consumer is never invoked and runs only from its event source mapping, and the results queue it writes is outside the bridge's queue registry so nothing else in the stack can reach it |
| Deployment shape | a `provided.al2023` Go function deploys from a CDK file asset and is driven by an `AWS::Lambda::EventSourceMapping` | `TestLocal_LambdaProducerAndConsumer` — the mapping is asserted to read the bridge's outbound queue and be `Enabled`, and the producer's deployed environment is asserted to name the bridge's INBOUND queue, which is what makes a message on the results queue proof that it crossed the bridge rather than skipped it |
| Deployment shape | outputs well-formed, health-check path parity, task role assumable and scoped | `TestLocal_DeploymentShape` |
| Deployment shape | destroy leaves nothing | `TestLocal_DestroyLeavesNothing` |
| Deployment shape | control-written config visible to a worker (the shared-storage proof) | `TestLocal_ClusterSharedConfigAndScaling` |
| Resilience | task restart loses no in-flight message | `TestLocal_TaskRestartLosesNoInFlightMessage` |
| Resilience | worker scale 1→3→1 with no duplicate delivery | `TestLocal_ClusterSharedConfigAndScaling` |
| Resilience | dead-letter entry and redrive | `TestLocal_DeadLetterAndAlarms` |
| Rollout | propose, commit, converge, member restart, rollback | `TestLocal_StaticSlotCohort` |
| Config source | runtime initialization of embedded config into DynamoDB, agreed generation-zero baseline, direct CAS table-write hot reload and return to the original log level on every member | `TestLocal_DynamoDBConfigHotReload` — verifies actual applied config, row/JSON versions and the immutable deployment fingerprint, not only rollout generations |
| Rollout | a change one member cannot answer for is applied by nobody | `TestLocal_StaticSlotCohort` |
| Rollout | a subscription change is agreed by the WHOLE cohort, not only by the member that proposed it | `TestLocal_StaticSlotCohort` |
| Rollout | the confirm window: a change every member accepts and none can run takes the cohort back | `TestLocal_StaticSlotCohort` — the lever is a subscription asking for a QoS the broker caps below it: every member builds and acks it, no member's subscriptions are ever satisfied, and the cohort reverts to its last confirmed generation |
| Observability | runtime metrics reach CloudWatch and the alarm's own query crosses its threshold on them | `TestLocal_DeadLetterAndAlarms` |
| Observability | an alarm driven into ALARM reaches its subscription | **not covered locally** — `TestLocal_DeadLetterAndAlarms` proves the topic's subscription carries messages, then skips: `SetAlarmState` does not run the alarm's actions on this emulator |

## Emulation gaps, and what each one costs

Each of these was measured, not assumed.

| Gap | What is done instead | What stays unproven locally |
|---|---|---|
| **The load balancer does not route to ECS tasks.** | The attachment is deployed as part of the shipped shape, and the health-check path and port the target group declares are probed against the container directly, which is the failure the gap would otherwise hide — a profile that health-checks a path the runtime does not serve. | ELB routing, which is AWS's. |
| **Alarms never evaluate.** | The alarm's own query is replayed through `GetMetricData` — against volume the deployment itself produced for the dead-letter alarm, and against published datapoints for the alarms whose inputs are ECS container-insights metrics the emulator has none of. | CloudWatch's evaluation state machine, which is AWS's. |
| **`SetAlarmState` does not run an alarm's actions.** The SNS subscription itself works — a plain publish to the topic reaches the subscribed queue — but putting the alarm into ALARM by hand notifies nobody. | The test proves the subscription with a probe publish first, so the two failures cannot be confused, and then skips with the measured reason. The day the emulator fires actions, the assertion starts running. | That an alarm action reaches its subscriber. |
| **IAM is not evaluated.** A call the assumed task role has no grant for still succeeds. | The granted half is executed as the task role. For the denied half, the policy CloudFormation attached to the deployed role is read back and every SQS grant in it must name this deployment's own queues. | That AWS refuses the non-granted call. |
| **CloudFormation cannot update an `AWS::ECS::Service`.** It reports the service it created as not found, then cannot roll back. | The idempotent-redeploy test skips with that reason rather than reporting a deployment defect that does not exist. | Whether re-deploying the same template is a no-op. Synth and the credentialed suite own it. |
| **EFS has no NFS data plane** and CloudFormation drops task-definition volumes. | The harness rewrites each EFS volume to a host bind mount before deploy, and re-registers filesystem-backed task definitions with the declared volumes and mount points. Explicit DynamoDB-only tasks are checked to have no volumes or mounts and remain on the deployed revision; no artificial bind mount is injected. | That a filesystem-backed task definition reaches ECS intact. |
| **~~The config mount's ownership is not reproducible.~~ Closed.** The harness used to bind-mount a host directory `0777`, which a SQLite store correctly refuses — it will not put a database under a parent it does not own, or one that is group- or other-writable. That was an accident of convenience, not a limit: the shipped EFS access point creates the mount `755` owned by the container user, and the harness now does the same. | Each stack's config directory is chowned and chmodded to match the access point from a throwaway root container, which covers both a uid-mapping Docker host and a plain Linux one, and handed back before cleanup removes it. | Nothing. |
| **DynamoDB tables are mirrored only after CloudFormation deploys.** | Runtime calls already address DynamoDB Local. Early tasks can encounter an absent table or config item; they stay live but unready while the target is unavailable. Once the table exists, the control runtime initializes its embedded document using strict create-if-absent. The test never injects an initial config and checks the initial item and every member's applied config, not a helper's stdout. | AWS table provisioning and task startup timing. |
| **Container stdout does not reach the `awslogs` driver.** | Log assertions read the container's own logs. | Nothing material. |
| **A destroyed stack can leave its log group behind.** Each run deploys under a fresh stack name, so leftovers accumulate in the emulator rather than colliding. | The harness removes the profile's log groups before each deploy. | Nothing. Log-group names carry the stack that owns them, so two live deployments of the same facade in one account and region no longer collide. |
| **SSM `SecureString` parameters are stored in clear.** | Nothing needs doing: no assertion anywhere reads stored ciphertext, and the credential adapter writes `SecureString` and reads back with decryption, which its own unit tests pin. A future assertion that means to prove encryption at rest cannot live here. | Encryption at rest, which is KMS's. |
| **FIFO deduplication has no time window.** | A test that means to exercise dedup enqueues originals and duplicates before any consumer starts. | Amazon's five-minute window. |

## Not yet stood up

Three shapes an operator can choose today that no deployed run had exercised.
All three are closed.

- [x] **SQLite stores on a deployed task.** Stood up by
      `TestLocal_MQTTSubjectAndAddressMapping`: the MQTT ingress session is
      persistent, its route runs `direct_hold`, and its managed-subscription
      store is a SQLite file on the deployed config mount. Two things had to
      land for it. The receiver's own binding to its session now manages a
      plan-driven ingress session that no route `session` block and no binding
      names — a plain manager with no lease and no outbox partition — so the
      route carries messages instead of reporting the right mode over a
      session nothing reconciled. And the durable session's baseline is seeded
      by the task itself at boot, from the `managed_subscription_baselines`
      the `GoBridgeSingle` facade stamps into the bootstrap document, because
      nothing but the task can write a store on that mount. The store keeps
      its database in a directory of its own under the mount
      (`managed-subscriptions/`), which it owns with mode `0700`; the mount
      itself stays `755`.
- [x] **Config held in DynamoDB.** Stood up by
      `TestLocal_DynamoDBConfigHotReload` using the shared HA fixture with one
      control and two static worker slots. The source is the single DynamoDB
      base document, not an overlay. Its bridge ID is scoped to the stack, so
      retained local rollout mirrors cannot supply another scenario's baseline.
      The deployed control runtime creates version 1 from its embedded document
      only if the item is absent; the test checks the initial row and every
      member's nonempty, agreed generation-zero artifact.
      It reads the current item through the real loader, CAS-writes `debug`,
      waits for a newer settled rollout, and reads each member's applied config
      through its admin endpoint. A second CAS write restores the original log
      level and must settle at another newer generation. Row and JSON versions
      advance together (1 → 2 → 3), stale writes are refused, typed plugin
      options survive, and the deployment fingerprint does not change.

      No EFS resources, task volumes, container mounts or config file are
      declared or added by the harness. The deployed definitions remain on
      their original revisions, running containers have no mounts, and the
      harness's unused config directory stays empty. Existing file-backed
      fixtures still get their declared shared mounts restored.

      This proves the polling source's deployed runtime path, not DynamoDB
      Streams, AWS IAM enforcement, or PITR/retention. The runtime image is built
      from the current checkout with the exact initial payload the production
      CDK image builder staged. Both file and DynamoDB fixtures initialize
      through the runtime; no Python helper writes configuration. The separate
      metadata and filesystem-ownership helpers use a pinned upstream AWS CLI
      image and have no configuration initialization role. This local run does
      not certify a published runtime image digest. Overlays remain a
      [programmatic-API pattern](../configuration-overview.md#overlays-and-the-admin-config-api-do-not-compose),
      not a second writer layered over this base document.
- [x] **Lambda either side of the bridge.** Stood up by
      `TestLocal_LambdaProducerAndConsumer`: a Go producer function is invoked
      directly and puts its payload on the bridge's inbound queue, the deployed
      bridge carries it to its outbound queue, and a Go consumer function the
      test never invokes — driven only by an event source mapping on that
      queue — puts what it received on a results queue nothing else in the
      stack can write. The three questions the ECS topologies never raised are
      answered by measurement rather than by the emulator's documentation.

      **How the code is packaged.** One statically linked binary
      (`CGO_ENABLED=0`) named `bootstrap`, mode `0755`, at the root of the
      asset directory, on the `provided.al2023` runtime with `bootstrap` as the
      handler. CDK stages that directory as an S3 file asset — the same
      mechanism the deploy already uses for CDK's own custom-resource handlers
      — and CloudFormation creates `AWS::Lambda::Function` from it. Both ends
      run the same binary and differ only by environment, so CDK publishes one
      asset for the pair. The architecture is read from the **Docker daemon**,
      not from the test process: the function runs in a container the emulator
      launches, and a binary built for the wrong one is answered by an `exec
      format error` from inside a container nobody is watching.

      **How the mapping is asserted.** Structurally and behaviourally, because
      neither alone is enough. `ListEventSourceMappings` must return exactly
      one mapping for the consumer, reading the bridge's outbound queue and
      `Enabled`, and none at all for the producer — and the consumer is never
      invoked by the test, so the mapping is the only thing that can run it.
      Nothing rests on concurrency: the emulator stores `ScalingConfig` without
      enforcing it, so the mapping is declared with a batch size of one and
      every assertion is about what arrived rather than about how the poller
      grouped it.

      **What the closed loop asserts on.** The identity of the messages,
      end to end, by TID, with no duplicates — plus the two facts that make
      that mean something. A producer writing straight to the outbound queue
      would fill the results queue just the same, so the producer's DEPLOYED
      environment is asserted to name the bridge's inbound queue while the
      consumer's mapping is asserted to read its outbound one. Mutation-checked
      both ways: with the mapping disabled nothing reaches the results queue,
      and with the producer pointed at the outbound queue the loop assertion
      still passes and only the producer-target assertion catches it.

      **What the run measured about the emulator.** A `provided.al2023` Go
      binary is accepted both through `CreateFunction` with an inline zip and
      through CloudFormation with S3-hosted code, reaching `Active` without a
      wait. The poller deletes on success — after an invocation the source
      queue reports zero visible and zero in flight, and a further 20-second
      drain returns nothing. A synchronous `Invoke` returns the function's
      result with no `FunctionError`. And the emulator's hostname resolves from
      inside a launched container: `GetQueueUrl` by name followed by
      `SendMessage` succeeds against `AWS_ENDPOINT_URL` alone, both when the
      emulator injects it and when the deployment sets it. That last point is
      why nothing hands a function a queue URL — every URL the emulator returns
      names its gateway host, which a container on the deployment network does
      not necessarily reach under that name, so both functions resolve their
      target by name exactly as the deployed bridge's own transports do.

      The emulator is not pinned — the helper pulls `floci/floci:latest` before
      every run — so these answers belong to the image the run was on, and a
      later break is news about the emulator rather than about the topology.

## Where the code lives

- `deployment/aws-filebased-config/cdk/integration/` — the harness and the
  tests. One harness serves both backends: `integration_aws` deploys to a
  credentialed sandbox, `integration_local` deploys the same stack through
  `cdklocal`, and the local backend is one branch in each shared function.
- `deployment/aws-filebased-config/cdk/integration/lambdafn/` — the Go function
  both ends of the Lambda topology run. One binary; the deployment's environment
  decides which end an instance is and which queue it forwards to.
- `testutil/flocilocal` — the emulator container helper, and why DynamoDB and
  the brokers are deliberately not served from it.

The measured answers behind each emulation decision are this page's *Emulation
gaps* table and the harness's own comments; the design documents that produced
them were scratch and have been deleted.
