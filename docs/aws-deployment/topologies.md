# AWS Deployment Topologies

The three shipped topologies — single task, replicated cluster, and the
DynamoDB-coordinated HA profile — and what each one guarantees. The HA sections
cover the rules that make failover safe: stable identities, least-privilege task
roles, honest alarms, and the credentialed proof that a standby really took
over.

Part of the [AWS Deployment Overview](overview.md).

---

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
least two warm candidates. Every service uses a 0/100 non-overlapping replacement
with Availability Zone rebalancing disabled, and the worker services are deployed
after the control service so the config seeder precedes the workers that read its
output.

`0/100` caps total tasks at the desired count, so a second cohort never runs
beside the first. It constrains counts, not ORDER: on the autoscaled shape, at a
desired count of two or more, the ECS scheduler may still replace in batches, so
a revision that changes durable session identity or store targets still needs the
scale-to-zero procedure in
[the cluster config rollout runbook](../runbooks/cluster-config-rollout.md).
Expect an ingress gap and a breaching warm-standby alarm for the duration of an
autoscaled deploy. The static member-slot shape does not have that gap: each slot
is a single-task service and the slots are chained, so a deploy replaces one slot
at a time and the rest of the cohort keeps serving — at the price of a deploy
whose duration grows with the roster.

### Two worker shapes

`GoBridgeDynamoDBHA` deploys its workers in one of two shapes, and the choice
decides whether the cohort can take a **live** config change:

| Shape | Selected by | Worker tasks | Live coordinated rollout |
|---|---|---|---|
| Autoscaled workers (default) | `MemberSlots` unset | One ECS service, `WorkerDesiredCount` interchangeable tasks | **No.** `bridge.cluster.rollout: coordinated` and a non-empty `bridge.cluster.members` are both **rejected at synth**. |
| Static member slots | `MemberSlots` set | One single-task ECS service per roster member, each with its own task definition and `member_id` | **The shape the barrier needs** — see [the cluster guide](../cluster/README.md) and the limitation below. |

> **A deployed cohort does not converge today.** Standing this shape up and
> committing a live-safe change is proven to reach the barrier — every slot
> comes up under its own restart-stable `member_id`, agrees on one membership
> epoch and one baseline — and then to ABORT rather than commit: each member
> computes a different candidate digest for the change than the member that
> proposed it, so none of them can join the proposal and the rollout ends at
> one acknowledgement. The cause is that a config document does not re-read as
> the config that was written (a nil list is persisted as an empty one), so the
> digest is not stable across a save. Until that is fixed, plan a live
> coordinated change on this profile as unavailable and use whole-cohort
> replacement.

The rejection is not a policy preference, it is the identity model. The rollout
barrier freezes `bridge.cluster.members` as its membership epoch and counts
acknowledgements against it, so a member must announce the same `member_id` after
a restart. An autoscaled task gets a fresh ECS task id on every placement, so it
can never re-enter the roster it left; a cohort of such tasks could never reach a
quorum, and a half-satisfied cohort would commit generations no member applies.
Rejecting the shape at synth beats deploying a stack that can only fail at boot.

With `MemberSlots` the construct additionally creates the retained, deletion-
protected rollout coordination table (named `<bridge.id>-rollouts` from the shared
config document, the same source as the three store table names), grants every task
role exactly `dynamodb:GetItem` and `dynamodb:PutItem` on it — the only two calls
the rollout store makes — stamps each slot's `member_id` and the generation-zero
baseline digest into that slot's bootstrap document, and orders the control slot
first and then each worker slot after the previous one, so a deploy replaces at
most one slot at a time. `WorkerDesiredCount` is rejected alongside `MemberSlots`: the roster is the
slot count, and scaling a slot past one task would run two processes under one
`member_id`.

A config change that the barrier classifies as replacement-required — and every
change to the deployment profile itself, including the image and the roster — is
still a CloudFormation deploy in both shapes.

A deploy that changes a **fingerprinted** field needs the shared config document
on EFS to change with it, and the default control seeder mode (`SeedOnce`) will
not do that: it keeps whatever document is already there and logs
`hash_mismatch_kept_existing`. Every member then refuses to boot, because the
document's deployment-profile fingerprint is not the one stamped into its task
definition. Set `ControlSeederMode: "Overwrite"` for that deploy, or run the
scale-to-zero procedure in
[the cluster config rollout runbook](../runbooks/cluster-config-rollout.md).
`SeedOnce` is the right default the rest of the time: it is what lets a live
coordinated rollout (or an Admin-API commit) survive a control-task restart
instead of being reverted to the last deployed document.

## Coordinated HA data plane

`GoBridgeDynamoDBHA` creates three encrypted, point-in-time-recoverable,
delete-protected, retained `PAY_PER_REQUEST` tables — four with `MemberSlots`.
The key/index shapes are the adapter contracts, not deployment inventions:

| Table | Schema | TTL invariant |
|---|---|---|
| Lease (`gobridge-leases` default) | `PK` string hash key; no sort key or indexes | **Disabled.** The row carries the permanent monotonic fencing version. Deleting it can reset fencing and permit split brain. |
| Shared outbox (`gobridge-outbox` default) | `PK`/`SK`; `ExpiryIndex` KEYS_ONLY, `RecordIDIndex` KEYS_ONLY, `ClaimIndex` ALL | Enabled on `ttl` only for terminal records and old fence metadata. Pending work is never TTL-reaped. |
| Managed subscriptions (`gobridge-managed-subscriptions` default) | `storage_identity` string hash key | Disabled; exact MQTT filter history is durable. |
| Rollout coordination (`<bridge.id>-rollouts`, **only with `MemberSlots`**) | `PK` string hash key; no sort key or indexes -- the rollout aggregate is one row | **Disabled.** The row holds the cohort's last committed config artifact, the point every restarting member recovers to. |

The data API is `DynamoDBHAData`, returned by `bridge.Data()`. It is the only HA
facade surface exposing table objects, names, and ARNs.

On-demand billing is appropriate for bursty takeover and outage recovery, but it
does not eliminate hot keys. A single Exclusive MQTT session concentrates the
outbox on one `SESSION#...` partition. Split unrelated workloads across session
IDs before that partition approaches DynamoDB limits. Preserve `ClaimIndex` to
avoid the adapter O(backlog) compatibility scan, and monitor the sparse
`ExpiryIndex` guidance for expiry-heavy traffic.

## Identity and endpoint rules

The shared bridge YAML must use `deployment_mode: clustered`, DynamoDB lease,
outbox, and managed-subscription stores, `delivery_mode: shared_outbox`,
`ack_after: outbox_persist`, and explicit `failover_slo` plus
`startup_allowance`. Every Exclusive MQTT standby uses the same broker domain,
`client_id`, clean-start/session-expiry behavior, and managed-subscription
storage identity. `client_id_suffix` is rejected for Exclusive sessions because
a per-task MQTT identity strands queued broker state after holder loss.

The facade also stamps two identities plus the exact table names into
deployment-owned bootstrap, and every process validates the EFS logical config
against them before store or transport planning, so a stale or tampered
SeedOnce/AdoptValid file cannot bypass synth-time admission:

- **Deployment-profile fingerprint** (`dynamodb_ha_config_fingerprint`) — a hash
  of only the fields the deployment provisions: `deployment_mode`, the
  `bridge.cluster` shape (`rollout`, `members`, `endpoints`), and the durable
  identity of every deployment-owned store (lease, outbox, DLQ, managed
  subscriptions). It is checked
  before a member votes on a config and again before it applies one. It
  deliberately does **not** cover routes, receivers, senders, sessions, or
  processors: those are operator content that a live config change is supposed to
  change, and gating them here would make every committed change fail on every
  member after the cohort had already agreed to it. Changing an existing durable
  session identity or an exclusive route's `session_id` is still refused — by the
  live-reload preflight, which owns that rule.
- **Baseline config digest** (`dynamodb_ha_baseline_config_digest`) — the full
  content identity of the exact document this deployment seeded. A coordinated
  member uses it to establish the cohort's generation-zero committed artifact at
  startup; see [Cluster config rollout](../runbooks/cluster-config-rollout.md).

Static `bridge.cluster.endpoints` are rejected by this profile. The bootstrap
registers the existing ECS metadata endpoint resolver and each holder writes its
own reachable endpoint into the lease row. This endpoint also lets the
credentialed proof map the lease to one exact ECS task without guessing.

## Least-privilege task roles

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

## Alarms and objective honesty

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

## Credentialed failover proof

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
