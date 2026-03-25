# Proposed Store Architecture

This document describes the DynamoDB table designs and operational guidelines for the `LeaseStore` and `OutboxStore` implementations.

Related documents:

- [ARCHITECTURE_NEW.md](./ARCHITECTURE_NEW.md)
- [ARCHITECTURE_NEW-CLUSTERING.md](./ARCHITECTURE_NEW-CLUSTERING.md)
- [ARCHITECTURE_NEW-MIDDLEWARE.md](./ARCHITECTURE_NEW-MIDDLEWARE.md)
- [ARCHITECTURE_NEW-MODULES.md](./ARCHITECTURE_NEW-MODULES.md)
- [ARCHITECTURE_RECORDS.md](./ARCHITECTURE_RECORDS.md)

## DynamoDB As Phase-1 Production Store

DynamoDB is the first production store for both `LeaseStore` and `OutboxStore`. It is chosen because:

- single-digit-millisecond reads and writes for lease renewal hot paths
- conditional writes map directly to lease fencing semantics
- TTL-based automatic item expiration aligns with outbox compaction
- on-demand capacity mode handles bursty bridge workloads
- multi-AZ durability by default

SQLite is a local/dev/test adapter only. Postgres may be added later, but is not phase-1.

## LeaseStore Table: `gobridge-leases`

```text
Table: gobridge-leases
Billing: On-Demand (PAY_PER_REQUEST)

Primary Key:
  PK (Partition Key): String  ->  "LEASE#<lease_id>"
  (No Sort Key -- single item per lease)

Attributes:
  owner         String   Bridge instance ID holding the lease
  version       Number   Monotonically increasing fencing token
  acquired_at   Number   Unix epoch millis when lease was acquired
  expires_at    Number   Unix epoch millis when lease expires
  renewed_at    Number   Unix epoch millis of last renewal
  ttl           Number   DynamoDB TTL attribute (expires_at + grace period in epoch seconds)
  metadata      Map      Optional lease metadata (session_id, client_id, etc.)
```

### LeaseStore Operations

**Acquire:**

```text
PutItem with ConditionExpression:
  "attribute_not_exists(PK) OR expires_at < :now"
Sets version=1 on fresh acquire, version=previous+1 on expired takeover.
```

**Renew:**

```text
UpdateItem with ConditionExpression:
  "owner = :my_id AND version = :my_version"
Updates expires_at and renewed_at.
Returns the same version (renewal does not change the fencing token).
```

**Release:**

```text
DeleteItem with ConditionExpression:
  "owner = :my_id AND version = :my_version"
```

**Fencing check (before outbox drain batch):**

```text
GetItem(lease_id) with ConsistentRead=true
Verify version matches in-memory version.
If mismatch, immediately stop sending and disconnect.
```

### Lease Timing Recommendations

- Lease TTL: 30 seconds
- Renewal interval: 10 seconds (lease TTL / 3)
- Renewal jitter: +/- 2 seconds
- Consecutive renewal failures before step-down: 3
- Grace period after step-down before release: 10 seconds

## OutboxStore Table: `gobridge-outbox`

```text
Table: gobridge-outbox
Billing: On-Demand (PAY_PER_REQUEST)

Primary Key:
  PK (Partition Key): String  ->  "<partition_key>"
  SK (Sort Key):      String  ->  "MSG#<created_at_millis>#<envelope_id>"

Partition Key Strategy:
  - For exclusive MQTT sessions: PK = "SESSION#<session_id>"
  - For stateless bindings:      PK = "BINDING#<binding_id>"
  - For route-scoped partitioning: PK = "ROUTE#<route_id>#SESSION#<session_id>"

Attributes:
  envelope_id    String   Unique message ID
  route_id       String   Route that produced this entry
  binding_id     String   Destination binding ID
  session_id     String   Target session ID (empty for stateless)
  address        String   Resolved transport address (topic, queue URL)
  payload        Binary   Serialized envelope
  headers        Map      Dispatch headers
  status         String   "pending" | "claimed" | "completed" | "expired"
  claimed_by     String   Bridge instance that claimed the item
  claimed_at     Number   Unix epoch millis
  claim_version  Number   Fencing token (lease version) for claim ownership
  replay_count   Number   Number of times this record has been claimed for replay
  created_at     Number   Unix epoch millis
  expires_at     Number   Unix epoch millis (from Envelope.ExpiresAt)
  completed_at   Number   Unix epoch millis
  ttl            Number   DynamoDB TTL (expires_at + compaction_grace in epoch seconds)
```

### Global Secondary Indexes

```text
GSI-1: StatusIndex
  PK: status
  SK: created_at
  Projection: KEYS_ONLY
  Purpose: Sweep for expired items, operational queries

GSI-2: ClaimedByIndex (for crash recovery)
  PK: claimed_by
  SK: claimed_at
  Projection: KEYS_ONLY
  Purpose: Find orphaned claims after a bridge crash
```

### OutboxStore Operations

**Persist (after processor chain):**

```text
PutItem with ConditionExpression:
  "attribute_not_exists(SK)"
Prevents duplicate writes from redelivered source messages (idempotency).
For fan-out, use TransactWriteItems with all dispatch plans in one transaction.
```

**Claim (by session owner draining):**

```text
UpdateItem with ConditionExpression:
  "status = :pending OR (status = :claimed AND claimed_at < :stale_threshold)"
Sets status="claimed", claimed_by=<bridge_id>, claim_version=<lease_version>.
Increments replay_count.
```

**Complete (after PUBACK/PUBCOMP):**

```text
UpdateItem with ConditionExpression:
  "claimed_by = :my_id AND claim_version = :my_claim_version"
Sets status="completed", completed_at=now().
```

**Query pending for drain:**

```text
Query(PK = "SESSION#<session_id>", SK begins_with "MSG#")
FilterExpression: "status = :pending OR (status = :claimed AND claimed_at < :stale)"
ConsistentRead: true
```

Strongly consistent reads are required for outbox drain queries to avoid missing recently written items.

### Outbox Compaction

DynamoDB TTL handles async physical deletion:

- For completed items: `ttl = completed_at + compaction_grace_period`
- For expired items: `ttl = expires_at + compaction_grace_period`
- Default compaction grace period: 1 hour

DynamoDB TTL deletion is not instantaneous (items may persist up to 48 hours after TTL). Application queries must always include `expires_at > :now` or `status` filters to exclude stale items.

### Hot Partition Mitigation

If a single MQTT session handles disproportionate traffic, its outbox partition becomes a DynamoDB hot partition. Mitigation strategies in order of preference:

1. **Route-scoped partitioning**: `PK = "ROUTE#<route_id>#SESSION#<session_id>"` provides natural spread if many routes target the same session.
2. **Write sharding**: `PK = "SESSION#<session_id>#SHARD#<hash(envelope_id) % shard_count>"`. Drain must query all shards with parallel queries.
3. **Burst-absorbing SQS buffer**: high-volume sessions write to an SQS queue first, then a worker batch-writes to DynamoDB.

Start with strategy 1. Monitor CloudWatch `SuccessfulRequestLatency` and `ThrottledRequests` metrics. Switch to strategy 2 only if throttling occurs.

## DLQ Store

For bridge-level DLQ entries, store in a DynamoDB table for queryability:

```text
Table: gobridge-dlq
PK: "ROUTE#<route_id>"
SK: "DLQ#<failed_at_millis>#<envelope_id>"
Attributes: envelope, reason, category, error_code, attempts, source_id,
            binding_id, session_id, correlation_id, last_error
TTL: failed_at + retention_period (e.g., 30 days)
```

## SQS Native DLQ Integration

SQS native dead-letter queues and the bridge DLQ serve different purposes:

- **SQS native DLQ**: catches infrastructure failures that prevent the bridge from processing the message at all (deserialization errors, malformed SQS messages). Configure `maxReceiveCount` on the source queue to a value higher than the bridge max retries plus a safety margin.
- **Bridge DLQ**: catches application-level permanent failures (validation, transformation errors), expired messages, and policy rejections.

Recommended: `SQS maxReceiveCount = bridge_max_retries + 3`.

## Capacity Planning

```text
Lease Store:
  Write pattern: 1 acquire + N renews per lease per TTL cycle
  Typical: 1 renew every 10 seconds per lease
  10 leases  -> ~1 WCU sustained
  100 leases -> ~10 WCU sustained
  On-Demand is correct for lease store (low, predictable traffic).

Outbox Store:
  Write pattern per message: 1 PutItem (persist) + 1 UpdateItem (claim) +
                             1 UpdateItem (complete) = 3 WCUs
  Read pattern: 1 Query per drain cycle per session (consistent reads = 2x RCU)

  At 1,000 msg/sec:  ~3,000 WCU, ~1,000 RCU
  At 10,000 msg/sec: ~30,000 WCU, ~10,000 RCU

  On-Demand scales to 40,000 WCU with auto-scaling above.
  For sustained high throughput (>5,000 msg/sec), consider Provisioned
  with auto-scaling to save ~25-30% cost.

  Item size estimate: ~500 bytes metadata + payload size.
  1 KB average -> 1 WCU per write.
  4 KB max for single WCU -> larger messages cost 2+ WCUs.
```

## CloudWatch Monitoring Strategy

Emit these custom CloudWatch metrics from the bridge:

```text
Namespace: GoBridge/Runtime

Lease Metrics:
  LeaseAcquireLatency     (ms)  Per lease ID
  LeaseRenewLatency       (ms)  Per lease ID
  LeaseAcquireFailures    (count) Per lease ID
  LeaseExpiries           (count) Per lease ID
  LeaseTransfers          (count) Per lease ID

Outbox Metrics:
  OutboxPersistLatency    (ms)  Per route
  OutboxDrainLatency      (ms)  Per session
  OutboxDepth             (count) Per partition (session/binding)
  OutboxClaimRecoveries   (count) Per session (stale claim reclaims)
  OutboxCompletions       (count) Per route
  OutboxExpiredBeforeSend (count) Per route
  OutboxReplayCount       (count) Per route (total replay attempts)

SQS Metrics:
  SQSReceiveLatency       (ms)  Per queue
  SQSDeleteLatency        (ms)  Per queue
  SQSVisibilityExtensions (count) Per queue

Delivery Metrics:
  DeliveryE2ELatency      (ms)  Per route (source receive to target accept)
  DLQEntries              (count) Per route, per category

MQTT Metrics:
  MQTTPublishLatency      (ms)  Per session
  MQTTReconnects          (count) Per session

Alarms:
  OutboxDepth > 1000 for 5 minutes        -> WARNING
  OutboxDepth > 10000 for 5 minutes       -> CRITICAL
  LeaseExpiries > 0 in 5 minutes          -> WARNING
  DLQEntries > 0 in 5 minutes             -> WARNING
  LeaseAcquireFailures > 3 in 5 minutes   -> CRITICAL
  SQSVisibilityExtensions > 100 in 5 min  -> WARNING (processing too slow)
```

## SQS FIFO vs Standard Queue Guidance

```text
Use Standard SQS when:
  - The bridge is the source (ingesting from SQS)
  - At-least-once is sufficient
  - Ordering is not required
  - Throughput > 3,000 msg/sec per queue is needed

Use FIFO SQS when:
  - Strict ordering within a message group is required
  - Exactly-once processing within the deduplication window (5 min) is needed
  - Throughput <= 3,000 msg/sec per queue (300 without batching)
  - The bridge is sending TO SQS and the consumer requires ordering

For bridge-internal DLQ queues:
  - Always use Standard (ordering does not matter for DLQ)
```

## IAM Least-Privilege Guidance

Bridge instances should use scoped IAM policies:

- SQS: `ReceiveMessage`, `DeleteMessage`, `ChangeMessageVisibility`, `SendMessage` on specific queue ARNs.
- DynamoDB LeaseStore: `PutItem`, `UpdateItem`, `DeleteItem`, `GetItem` on the leases table ARN.
- DynamoDB OutboxStore: `PutItem`, `UpdateItem`, `DeleteItem`, `GetItem`, `Query` on the outbox table ARN and index ARNs.
- DynamoDB DLQ: `PutItem`, `Query` on the DLQ table ARN.
- CloudWatch: `PutMetricData` scoped to the `GoBridge/Runtime` namespace.
