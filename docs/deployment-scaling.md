# Scaling a GoBridge deployment

### Concurrency Control

Each route has a `max_in_flight` setting (default: 100) that limits concurrent
message processing. This acts as a per-route backpressure mechanism:

```yaml
routes:
  - id: high-throughput
    receiver_id: sqs-in
    bindings: [to-mqtt]
    policy:
      max_in_flight: 500
```

Higher values increase throughput but consume more memory and CPU. Lower
values reduce resource usage but may cause backpressure on the source.

### CPU and Memory Sizing

This is the authoritative throughput-to-resource tier table. AWS-specific docs
reference it rather than restating it.

| Workload | vCPU | Memory | `max_in_flight` |
|----------|------|--------|-----------------|
| Low (< 100 msg/s) | 0.25 | 512 MiB | 50-100 |
| Medium (100-1000 msg/s) | 0.5-1.0 | 1-2 GiB | 100-500 |
| High (> 1000 msg/s) | 2.0-4.0 | 4-8 GiB | 500-2000 |

These are starting points. Profile your workload with realistic message sizes
and processor chains to find the right balance.

On ECS Fargate these map to task sizes where **1024 CPU units = 1 vCPU** (so
0.25 vCPU = 256 units / 512 MiB, 0.5 vCPU = 512 units / 1024 MiB, 1 vCPU = 1024
units / 2048 MiB). The `High` row is a single-instance vertical ceiling; the
clustered profile instead scales horizontally, sizing each worker task lower and
multiplying capacity by worker count. For Fargate task-size defaults and the
horizontal-vs-vertical trade-off, see
[AWS Deployment — Sizing Guidance](aws-deployment/compute.md#sizing-guidance).

### Horizontal Scaling

Add more replicas with `filesystem_replicated` topology when a single instance
cannot handle the throughput. Each replica processes messages independently
from the shared config file:

```json
{
  "topology": "filesystem_replicated",
  "config_file_path": "/var/lib/gobridge/bridge.yaml",
  "poll_interval": "5s"
}
```

When using SQS as the source, horizontal scaling works naturally -- each
replica pulls from the same queue and SQS handles message distribution. For
MQTT sources, use `$share/` topic prefixes to distribute messages across
replicas.

### Shared Tenant Usage Store

If you run multiple instances and enforce the tenant in-flight ceiling
(`TenantInfo.MaxInFlight`) through a **shared** usage store -- a Redis or DynamoDB
counter that spans instances -- the store must decay in-flight counts a crashed
instance leaves behind. The tenant processor brackets each delivery with `+1` on
admission and `-1` on settle; a crash between the two (`kill -9`, OOM, node loss)
strands a stale `+1`, and enough leaks throttle the tenant permanently. A
conforming shared store makes each `+1` self-healing -- a TTL-leased item the
store auto-expires, or an implementation of `ports.TenantUsageReconciler` driven
from your instance-lifecycle hook. A plain additive counter with no decay is not
conforming; a per-instance / in-memory tracker needs none of this, since its
counts die with the process. See
[Tenant quota enforcement](processors-and-stores.md#quota-enforcement) for the
full contract.

### Vertical Scaling

Increase CPU and memory allocation for higher throughput per instance. This
is simpler than horizontal scaling and avoids coordination overhead. We
recommend vertical scaling first, then horizontal when a single instance
reaches its limits.

### Delivery Mode Selection

Choose on what the **source** can do, not on how much you care about the
messages.

| Mode | Behavior | Where the durable copy lives |
|------|----------|------------------------------|
| `direct_hold` | Source held open until egress completes | On the source, until the egress succeeds |
| `shared_outbox` | Source ACKed after outbox persist | In the outbox store, from the moment the source is ACKed |

**Use `direct_hold` for any single-destination route.** The bridge does not
acknowledge the source until the egress succeeds -- it extends an SQS
visibility window while it works, and on MQTT it simply does not send the
PUBACK (the adapter runs the client with manual acknowledgement). A crash
before the egress completes therefore loses nothing: the source redelivers.

**An outbox does not improve on that, and this is the point most often got
backwards.** Both modes have exactly one window in which a crash matters, and
the two windows are the same size:

| Mode | Crash window | On a crash inside it |
|------|--------------|----------------------|
| `direct_hold` | receive → destination accepts | Source not acknowledged → redelivered |
| `shared_outbox` | receive → outbox write completes | Source not acknowledged → redelivered |

Crashing before the outbox write is no better than crashing before the
destination send. The outbox does not add a durable copy — with
`ack_after: outbox_persist` the source is settled as soon as the record is
persisted, so it **moves** the durable copy out of the source and into a store
you operate — and it adds a second hop that can fail on its own. The route now
needs the source, the outbox store and the destination, where it needed two of
the three. Availability multiplies down.

**Use `shared_outbox` when one of these is true** — none of which is crash
safety:

- **One message fans out to several destinations** and a partial success has to
  survive a crash. Source redelivery cannot express "three of five accepted";
  the outbox records progress per destination.
- **The destination may be unavailable longer than the source will hold.** You
  are choosing to own the buffer rather than let a visibility window expire or
  a broker session lapse.
- **You need ingress throughput decoupled from egress latency.**
- **Several instances share an exclusive session.** `direct_hold` carries no
  fencing token at the sender boundary, so a route that fails over has a
  bounded duplicate-send window. `shared_outbox` fences it — this is duplicate
  suppression, not durability.
