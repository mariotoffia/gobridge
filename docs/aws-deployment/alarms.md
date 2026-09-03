# CloudWatch alarms on AWS

Which alarms a GoBridge deployment gets for free, which ones it only gets after
opting in, which ones nothing provisions, and what each of them means when it
fires.

For the metric each alarm reads, see
[Monitoring and Observability](monitoring.md#key-metrics). For the incident path
behind an alarm, see the [runbooks](../runbooks/README.md).

---

## Where alarms come from

Three separate mechanisms provision alarms, and they do not overlap. Knowing
which one you are using decides what you still have to author.

| Source | What it is | What it covers |
|--------|-----------|----------------|
| **CDK bundle** (`GoBridgeAlarms`) | A construct in `deployment/aws-filebased-config/cdk`, wired into the shipped deployment profiles. | Deployment health (ECS, EFS, ALB, DynamoDB tables) plus the runtime rollup signals for the profile it is attached to. The complete list is [below](#alarms-the-cdk-bundle-provisions). |
| **`DefaultAlarms()`** | A Go helper in `adapters/aws/metrics/cloudwatch`, applied with `EnsureAlarms`. For deployments that do **not** use the CDK bundle. | Runtime alarms on outbox depth (warning and critical), DLQ entries, DLQ depth, lease expiries, lease-acquire failures, SQS visibility extensions, dropped messages, and sustained expiry. |
| **Hand-authored** | Anything you write yourself, in CDK or the console. | Everything else — including the signals listed under [Alarms you must author yourself](#alarms-you-must-author-yourself). |

The runtime never calls `EnsureAlarms` for you: alarm provisioning is a
deploy-time concern, so an unprovisioned signal is silent, not degraded.

> **Both built-in sources alarm on DIMENSIONLESS series.** Every runtime alarm
> below reads a metric with no dimensions, because a fleet-wide alarm cannot
> enumerate the `route_id` / `session_id` / `partition` values the runtime emits.
> That only works when the exporter is publishing a
> [rollup copy](#rollup-metrics-the-built-in-alarms-require) of the metric into
> the **same namespace** the alarm reads. Miss either half and the alarm never
> leaves `INSUFFICIENT_DATA` — it does not fail loudly, it simply never fires.

---

## Alarms the CDK bundle provisions

Every row is created by `GoBridgeAlarms` for the shape named in the second
column. Defaults are a 1-minute period over 5 evaluation periods; `AlarmsProps`
overrides the period, the evaluation count, and the thresholds marked
*configurable*.

"Missing data" is the CloudWatch `TreatMissingData` setting, and it is the
difference between an alarm that catches a dead process and one that stays green
through an outage: **breaching** means silence is a failure, **not breaching**
means silence is health.

| Alarm | Provisioned by | Metric (statistic) | Threshold | Missing data | Fires when |
|---|---|---|---|---|---|
| `ControlAbsence` | every shape | `ECS/ContainerInsights` `RunningTaskCount` (Maximum) | `< 1` | breaching | The control service has no running task. Silence is treated as absence — a dead cluster stops publishing the metric. |
| `WorkerDegraded` | cluster, HA | metric math over `RunningTaskCount` / `DesiredTaskCount` (Minimum / Maximum) | `>= 1` | not breaching | A worker service is running fewer tasks than it wants. One alarm **per worker service**, so the alarm names the slot that is short; the static member-slot profile creates one per roster slot. |
| `WarmStandbyUnavailable` | HA | metric math over control + worker `RunningTaskCount` (Minimum) | `>= 1` | breaching | Fewer than two tasks are running in total, so no warm standby is guaranteed and a failover would be a cold start. |
| `EfsPercentIOLimit` | every shape | `AWS/EFS` `PercentIOLimit` (Average) | `> 90` *configurable* | not breaching | The config file system is near its I/O ceiling; config reads and hot reloads slow down. |
| `AlbUnhealthyControl` | ALB attachment | `UnHealthyHostCount` on the control target group (Maximum) | `> 0` | not breaching | The load balancer cannot reach a healthy control target. |
| `AlbUnhealthyWorker` | ALB attachment | `UnHealthyHostCount` on the worker target group (Maximum) | `> 0` | not breaching | The load balancer cannot reach a healthy worker target. |
| `Alb5xxControl` | ALB attachment | `HTTPCode_Target_5XX_Count` on the control target group (Sum) | `> 5` *configurable* | not breaching | Control-plane requests are failing at the target, not at the balancer. |
| `Alb5xxWorker` | ALB attachment | `HTTPCode_Target_5XX_Count` on the worker target group (Sum) | `> 5` *configurable* | not breaching | Worker HTTP ingress is failing at the target. |
| `OutboxDepth` | `EnableRollupAlarms` (non-HA) | `OutboxDepth` (Maximum) | `> 1000` *configurable* | **breaching** | The pending outbox backlog exceeded the threshold — or the gauge stopped arriving, which means the drainer or the bridge died. |
| `DLQEntries` | `EnableRollupAlarms` (non-HA) | `DLQEntries` (Sum) | `> 0` | not breaching | A message was dead-lettered. |
| `LeaseExpiries` | `EnableRollupAlarms` (non-HA) | `LeaseExpiries` (Sum) | `> 0` | not breaching | A session lost its exclusive lease instead of renewing it. |
| `LeaseAcquireFailures` | `EnableRollupAlarms` (non-HA) | `LeaseAcquireFailures` (Sum) | `> 3` *configurable* | not breaching | Lease acquisition keeps failing — usually a store outage or a permissions change. |
| `FailureToFullDuration` | HA | `FailureToFullDuration` (Maximum) | `>` the profile's failover objective | not breaching | The external credentialed probe measured a failure-to-`Full` recovery slower than the declared objective. Emitted by the probe, never by a task that may itself be dead, so missing data is not a failure here. The only alarm in the bundle that evaluates over a **single** period rather than the shared evaluation count: one measured breach of the declared objective is enough. |
| `LeaseDynamoDBThrottles` | HA | `AWS/DynamoDB` `ThrottledRequests` summed over seven operations (Sum) | `> 0` | not breaching | The lease table throttled a runtime request; renewals are at risk. |
| `LeaseDynamoDBSystemErrors` | HA | `AWS/DynamoDB` `SystemErrors` summed over seven operations (Sum) | `> 0` | not breaching | The lease table returned server-side errors. |
| `OutboxDynamoDBThrottles` | HA | `AWS/DynamoDB` `ThrottledRequests` (Sum) | `> 0` | not breaching | The outbox table throttled; drain slows and backlog grows. |
| `OutboxDynamoDBSystemErrors` | HA | `AWS/DynamoDB` `SystemErrors` (Sum) | `> 0` | not breaching | The outbox table returned server-side errors. |
| `ManagedSubscriptionsDynamoDBThrottles` | HA | `AWS/DynamoDB` `ThrottledRequests` (Sum) | `> 0` | not breaching | The managed-subscription table throttled; subscription reconcile and migration stall. |
| `ManagedSubscriptionsDynamoDBSystemErrors` | HA | `AWS/DynamoDB` `SystemErrors` (Sum) | `> 0` | not breaching | The managed-subscription table returned server-side errors. |
| `RolloutDynamoDBThrottles` | HA with static member slots | `AWS/DynamoDB` `ThrottledRequests` (Sum) | `> 0` | not breaching | The rollout table throttled. It is both the barrier's coordination store and the boot-resolve gate, so throttling stalls every rollout **and** can stop a replaced task from starting. |
| `RolloutDynamoDBSystemErrors` | HA with static member slots | `AWS/DynamoDB` `SystemErrors` (Sum) | `> 0` | not breaching | The rollout table returned server-side errors. |
| `HAOutboxDepth` | HA | `OutboxDepth` (Maximum) | `> 1000` | **breaching** | The shared-outbox backlog exceeded the HA threshold, or the gauge stopped arriving. |
| `HAOutboxDrainLatency` | HA | `OutboxDrainLatency` (Maximum) | `>` the profile's failover objective | not breaching | Drain batches are taking longer than the objective allows. Inspect the oldest pending records directly for backlog age — this is a latency signal, not an age one. |
| `HAOutboxDepthFailures` | HA | `OutboxDepthFailures` (Sum) | `> 0` | not breaching | The depth query itself is failing. Investigate the store, not the backlog. |
| `HAOutboxRecordFailures` | HA | `OutboxRecordFailures` (Sum) | `> 0` | not breaching | Records failed this drain cycle, including stale-fencing outcomes. |
| `HAOutboxDrainStalled` | HA | `OutboxDrainStalled` (Sum) | `> 0` | not breaching | In-flight sends did not return past the batch deadline — a sender that ignores `ctx`. The runtime does not kill it; this is diagnostic, not recovery. |
| `HALeaseExpiries` | HA | `LeaseExpiries` (Sum) | `> 0` | not breaching | A lease expired or an owner stepped down fail-closed. |
| `HALeaseTransfers` | HA | `LeaseTransfers` (Sum) | `> 1` | not breaching | More than one takeover inside a single evaluation window — the signature of flapping, not of a single clean failover. |
| `HADLQDepth` | HA | `DLQDepth` (Maximum) | `> 0` | not breaching | The DLQ has outstanding entries right now. Requires the [DLQ depth sampler](#the-dlq-depth-sampler). |
| `HADLQEntries` | HA | `DLQEntries` (Sum) | `> 0` | not breaching | A message was dead-lettered. |
| `HADLQWriteFailures` | HA | `DLQWriteFailures` (Sum) | `> 0` | not breaching | A DLQ write failed after retries, or was skipped with no held lease — evidence that should be durable is not. |
| `HAMQTTIngressPoisonDropped` | HA | `MQTTIngressPoisonDropped` (Sum) | `> 0` | not breaching | An inbound publish exceeded a local payload/property cap and was acked and dropped. Every count is acknowledged loss — see the [ingress-poison runbook](../runbooks/mqtt-ingress-poison.md). |
| `HAReconcileFailures` | HA | `ReconcileFailures` (Sum) | `> 0` | not breaching | Subscription reconcile failed; a permanent SUBACK rejection flaps the whole session. |
| `HAMQTTSessionTakeover` | HA | `MQTTSessionTakeover` (Sum) | `> 0` | not breaching | Another client connected with the same `client_id` — an identity collision, or a normal exclusive failover. |
| `HAMQTTQoSDowngraded` | HA | `MQTTQoSDowngraded` (Sum) | `> 0` | not breaching | The broker granted a lower QoS than requested; delivery guarantees are weaker than configured. |
| `HAClusterRolloutDiverged` | `EnableClusterRolloutAlarms` | `ClusterRolloutDiverged` (Maximum) | `> 0` | not breaching | A member is not running the generation the cohort decided on. The barrier is atomic before the commit and per-member after it ([ADR 0013](../adr/0013-coordinated-cluster-config-rollout.md)), so a brief `1` during a rollout is normal — the evaluation periods are what separate it from a split cohort. |
| `HAClusterRolloutTerminal` | `EnableClusterRolloutAlarms` | `ClusterRolloutTerminal` (Maximum) | `> 0` | not breaching | A member has exhausted its own repair. Not a rate: any non-zero value needs an operator. Read `terminal_reason` in `/deephealth` — it says whether to repair the rollout store or replace the member. |
| `HAClusterRolloutObservationAge` | `EnableClusterRolloutAlarms` | `ClusterRolloutObservationAge` (Maximum) | `> 60` seconds | not breaching | Members have not read the rollout row for over a minute, so every rollout field they report is stale. A fleet that cannot see the row does not know its own rollout state; that is not the same as being healthy. |

`EnableRollupAlarms` applies **only** when no `DynamoDBHA` construct is wired: an
HA deployment provisions the `HA…` rollup alarms above instead, which are a
superset, so setting both flags does not double up and setting only the rollup
flag on an HA deployment does nothing.

The fleet convergence alarms, by contrast, are installed for **any** deployment
shape: the barrier runs wherever a composition root drives it, so gating them on
one facade would install them only where they cannot fire.

---

## Rollup metrics the built-in alarms require

The runtime emits most metrics with a dimension (`route_id`, `session_id`,
`partition`, …). A zero-dimension alarm never matches a dimensioned series, so
the exporter publishes a second, **dimensionless** copy of the metrics below when
configured with `WithRollupMetrics(cwmetrics.DefaultRollupMetrics()...)`. Rollup
copies never carry the `instance_id` tag added by `WithInstanceTag`.

Configure the exporter with that list **and** the same namespace the alarms read
(`RollupMetricsNamespace` on the CDK construct, the `namespace` argument to
`DefaultAlarms`), or the alarms sit at `INSUFFICIENT_DATA`.

| Rollup metric | Why it needs a rollup copy |
|---|---|
| `OutboxDepth` | Emitted per `partition`. |
| `LeaseExpiries` | Emitted per `lease_id`. |
| `DLQEntries` | Emitted per `route_id` and `category`. |
| `LeaseAcquireFailures` | Emitted per `lease_id`. |
| `LeaseTransfers` | Emitted per `lease_id`. |
| `OutboxDrainLatency` | Emitted per `session_id`. |
| `OutboxDepthFailures` | Emitted per `partition`. |
| `OutboxRecordFailures` | Emitted per `route_id`. |
| `OutboxDrainStalled` | Emitted per `session_id` and `route_id`. |
| `DLQWriteFailures` | Dimensionless at the source; rolled up so it still matches on an instance-tagged fleet. |
| `CredentialRefreshFailures` | Dimensionless at the source; rolled up for instance-tagged fleets. No built-in alarm — author one if an unreachable secrets backend matters to you. |
| `SQSVisibilityExtensions` | Emitted per `queue_url`. |
| `DLQDepth` | Dimensionless at the source; rolled up for instance-tagged fleets. The rollup takes the fleet `Maximum`. |
| `MessagesDropped` | Emitted per `route_id` and `reason`. |
| `MessagesExpired` | Emitted per `route_id`. |
| `MessagesFiltered` | Emitted per `route_id` and `processor`. Rolled up so an unexpected filter rate CAN be alarmed on; no built-in alarm ships, because a filter discard is policy, not loss. |
| `MQTTIngressPoisonDropped` | Emitted per `session_id`. |
| `ReconcileFailures` | Emitted per `session_id`. |
| `MQTTSessionTakeover` | Emitted per `session_id`. |
| `MQTTQoSDowngraded` | Emitted per `session_id`. |
| `ClusterRolloutDiverged` | Emitted per member; the rollup takes the fleet `Maximum`, so one wrong member alarms. |
| `ClusterRolloutTerminal` | Emitted per member; fleet `Maximum`. |
| `ClusterRolloutObservationAge` | Emitted per member; fleet `Maximum` is the staleness of the worst-informed member. |

Without instance tagging, a metric that is already dimensionless has a base copy
and a rollup copy that coincide — a harmless double count on a counter, and no
effect at all on a gauge alarm reading `Maximum`.

## The DLQ depth sampler

`DLQDepth` is the only built-in alarm whose metric the runtime does not emit on
its own cadence. It is sampled by the composition root calling
`runtime.ReportDLQDepth` periodically against a DLQ store that implements
`ports.DLQDepthReporter`. Until that sampler is wired the gauge is never
published, and because the alarm uses `TreatMissingData=notBreaching` it sits
silent rather than false-alarming.

**Verify the sampler is running before relying on the alarm.** `notBreaching` is
deliberate here: `breaching` would put every fleet without a sampler into alarm
permanently, which trains operators to ignore the console.

---

## Alarms you must author yourself

Nothing provisions these. Each one covers a failure the built-in set does not
see, and each is a plain dimensionless alarm on a rolled-up metric or a
dimensioned alarm on an adapter series.

| Signal | Suggested alarm | Why it is not built in |
|---|---|---|
| `MessagesDropped` (CDK bundle) | `Sum > 0` over 5 minutes, critical | The CDK bundle does not provision it; `DefaultAlarms()` does. On a CDK deployment this is the single silent-loss signal you are missing. |
| `MessagesExpired` | `Sum > 0` sustained over 3 periods, warning | TTL loss is normal in small amounts on some routes; the threshold is deployment-specific. |
| `RouteOwnerUnknown` | `Sum > 0` sustained, warning, split by `reason` | The reason tag separates fleet clock skew (`lease_expired` against a healthy owner) from a dead lease store (`store_unavailable`, `store_breaker_open`). Without it, unexplained 502/503 responses have no cause. |
| `BrokerHealthStepDown` | `Sum > 0`, warning | Only emitted when `broker_health_step_down` is configured, so a default alarm would be permanently blank. |
| `DLQWriteHold` | `Maximum` approaching the router's write budget (10.5 s in the shipped wiring), high | A sustained hold means the DLQ store, not the route, is stalling intake. The right threshold depends on the configured budget. |
| `OutboxStranded` | `Sum > 0`, high | Records left with no drainer after a forced destructive reload. Rare, and always operator-triggered. |
| `ConfigDegraded` | `Maximum >= 1` sustained, high | The bridge is running blind on its last good config, or a reload applied but never converged. Read `/deephealth` for which. |
| `MQTTEgressRejected` | `Sum > 0`, high | A producer or route is generating messages this broker cannot accept. Each one is DLQ'd, not retried, so the DLQ fills at the rejection rate. |
| `MQTTReceiverEmitRejected` (`outcome=lost`) | `Sum > 0` on the `lost` dimension, high | Acknowledged best-effort loss. Whether it matters depends on how significant QoS 0 ingress is to the deployment, so no default threshold is honest. |
| `SQLiteStoreUnhealthy` | `Sum > 0` on `entity=outbox`, critical | Adapter-owned and dimensioned, so no rollup copy exists. Applies to single-instance SQLite deployments only. |
| `DynamoDBOutboxClaimScanPages` | `Sum > 0` over 15 minutes, warning | Adapter-owned and dimensioned. On a table that has `ClaimIndex`, a rising value means ordering keys rather than a missing index. |
| ECS `CPUUtilization` / `MemoryUtilization` | `> 80%` over 5 minutes, warning | Sizing is deployment-specific. |
| `RouteErrors` / `MessagesReceived` | `> 5%` over 5 minutes, high | A ratio needs a metric-math expression and a traffic-shaped threshold. |

---

## Authoring an alarm in CDK

Both patterns below are plain CDK; neither needs the bundle.

```go
import (
    "github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
    "github.com/aws/jsii-runtime-go"
)

// A dimensionless alarm on a rolled-up runtime metric.
awscloudwatch.NewAlarm(stack, jsii.String("MessagesDroppedAlarm"), &awscloudwatch.AlarmProps{
    AlarmName: jsii.String("GoBridge-MessagesDropped-Critical"),
    Metric: awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
        Namespace:  jsii.String("GoBridge/Runtime"),
        MetricName: jsii.String("MessagesDropped"),
        Statistic:  jsii.String("Sum"),
        Period:     awscdk.Duration_Minutes(jsii.Number(5)),
    }),
    Threshold:          jsii.Number(0),
    EvaluationPeriods:  jsii.Number(1),
    ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
    TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
    AlarmDescription:   jsii.String("[CRITICAL] Terminal message loss with no DLQ record"),
})
```

```go
// A ratio needs metric math over two series.
errMetric := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
    Namespace:  jsii.String("GoBridge/Runtime"),
    MetricName: jsii.String("RouteErrors"),
    Statistic:  jsii.String("Sum"),
    Period:     awscdk.Duration_Minutes(jsii.Number(5)),
})
recvMetric := awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
    Namespace:  jsii.String("GoBridge/Runtime"),
    MetricName: jsii.String("MessagesReceived"),
    Statistic:  jsii.String("Sum"),
    Period:     awscdk.Duration_Minutes(jsii.Number(5)),
})

awscloudwatch.NewAlarm(stack, jsii.String("ErrorRateAlarm"), &awscloudwatch.AlarmProps{
    AlarmName: jsii.String("GoBridge-ErrorRate-High"),
    Metric: awscloudwatch.NewMathExpression(&awscloudwatch.MathExpressionProps{
        Expression: jsii.String("(errors / received) * 100"),
        UsingMetrics: &map[string]awscloudwatch.IMetric{
            "errors":   errMetric,
            "received": recvMetric,
        },
        Period: awscdk.Duration_Minutes(jsii.Number(5)),
    }),
    Threshold:          jsii.Number(5),
    EvaluationPeriods:  jsii.Number(1),
    ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
    AlarmDescription:   jsii.String("[HIGH] Error rate exceeds 5%"),
})
```

Attach notifications to any alarm the same way the bundle does:

```go
alarmAction := awscloudwatchactions.NewSnsAction(snsTopic)
alarm.AddAlarmAction(alarmAction)
alarm.AddOkAction(alarmAction)
```

## Provisioning `DefaultAlarms()` without CDK

```go
import cwmetrics "github.com/mariotoffia/gobridge/adapters/aws/metrics/cloudwatch"

alarms := cwmetrics.DefaultAlarms("GoBridge/Runtime",
    "arn:aws:sns:eu-west-1:123456789012:gobridge-alerts",
)

if err := cwmetrics.EnsureAlarms(ctx, cwClient, alarms); err != nil {
    log.Fatalf("alarm setup: %v", err)
}
```

Pass the **same namespace** to the exporter, and configure it with
`WithRollupMetrics(cwmetrics.DefaultRollupMetrics()...)`.

---

## Related

- [Monitoring and Observability](monitoring.md) — the metric each alarm reads.
- [Logging, dashboards and tracing](logging-and-dashboards.md) — charting the same series.
- [Operational runbooks](../runbooks/README.md) — the incident path behind each alarm.
