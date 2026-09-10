# Compute and Runtime Metrics

Why the shipped profile runs on ECS Fargate, how to size a task, and how the
bootstrap config selects the runtime metrics backend.

Part of the [AWS Deployment Overview](overview.md).

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
[Deployment Guide — CPU and Memory Sizing](../deployment-scaling.md#cpu-and-memory-sizing);
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
