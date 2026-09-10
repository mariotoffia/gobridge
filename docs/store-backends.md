# Store backends

Four store roles handle different coordination concerns, each configured
independently in the `stores` section.

| Role | Interface | Purpose |
|------|-----------|---------|
| **Lease** | `ports.LeaseStore` | Distributed lease acquisition for exclusive sessions. |
| **Outbox** | `ports.OutboxStore` | Durable outbox for at-least-once delivery with shared sessions. Only configure one when the route needs it -- see the note below. |
| **DLQ** | `ports.DLQStore` | Dead-letter queue for permanently failed messages. |
| **Managed subscriptions** | `ports.ManagedSubscriptionStore` | Durable exact MQTT filter history for persistent/exclusive sessions. |

> **An outbox is not a crash-safety mechanism.** The bridge withholds the source
> acknowledgement until the route settles, so a crash before the destination
> accepts is already recovered by source redelivery — and a crash before the
> OUTBOX write is recovered exactly the same way, no better. An outbox does not
> add a durable copy; it MOVES the copy out of the source into this store and
> adds a hop that can fail. Configure one for fan-out, back-pressure or
> fencing — see
> [Delivery Mode Selection](deployment-scaling.md#delivery-mode-selection) — not
> for durability you already have.

### YAML Structure

```yaml
stores:
  lease:
    # A durable outbox requires a durable lease -- see Store durability and
    # pairing rules below. The in-memory lease is only valid alongside an
    # in-memory outbox.
    type: dynamodb
    options:
      table_name: my-lease-table
  outbox:
    type: sqlite
    options:
      path: /data/outbox.db
  dlq:
    type: dynamodb
    options:
      table_name: my-dlq-table
```

### Store durability and pairing rules

A store is **crash-durable** when a successful write survives the loss of the
process. A nil result from an outbox persist or a DLQ write means exactly that,
because the runtime settles the SOURCE on it -- an at-least-once ingress
acknowledges upstream as soon as the persist returns. Two rules follow, both
enforced at startup:

| Lease | Outbox | Result |
|---|---|---|
| `dynamodb` | `dynamodb` / `sqlite` | Accepted. The production posture. |
| `dynamodb` / `memory` | `memory` | Accepted with `acknowledge_volatile: true`; warns, naming the routes. |
| `memory` | `dynamodb` / `sqlite` | **Rejected at startup** when any route uses `shared_outbox`. |

1. **A process-volatile lease may not back a crash-durable outbox.** The durable
   outboxes persist a per-partition fencing high-water-mark and reject any claim
   below it; the in-memory lease numbers fencing versions from a per-process
   counter that restarts at zero. After a restart the new owner claims below the
   mark, is fenced out, and the partition never drains again while ingress keeps
   acknowledging into it. No acknowledgement is offered: this is a permanent
   loss of progress, not a tradeoff.
2. **A volatile outbox or DLQ requires `acknowledge_volatile: true`.** Losing
   accepted work, or the terminal evidence of dropped work, on restart is an
   acceptable development tradeoff only when it is stated. The memory store
   refuses to build either role without it, and the builder warns at startup
   naming every affected route.

The rejection is scoped to blueprints that actually drain the outbox: a fencing
token only ever reaches the store from a `shared_outbox` route's drainer, so a
durable outbox nothing drains cannot wedge. A reload that adds such a route is
judged again and rejected before it commits.

`sqlite` is node-local, so a SQLite outbox is single-replica even under a
DynamoDB lease -- a second replica ingests into its own database file and cannot
drain it until it wins the lease. Multi-instance operation needs `dynamodb` for
both.

Volatile stores are excluded from the production profile: a deployment that must
not lose acknowledged work runs `sqlite` (single instance) or `dynamodb`
(clustered) for both the outbox and the DLQ.

### Memory Store

- **Type:** `memory`
- **Options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `acknowledge_single_replica` | bool | `false` | **Required to build the LEASE store.** The in-memory lease keeps ownership in a per-process map and cannot coordinate across replicas, so more than one instance silently splits the brain. Construction fails without it. No effect on the outbox or DLQ. |
| `acknowledge_volatile` | bool | `false` | **Required to build the OUTBOX or DLQ store.** Both hold their records in the process heap, so a restart, crash, or OOM kill loses accepted work and the terminal evidence of dropped work -- after the source was already acknowledged. Construction fails without it. No effect on the lease. |

- In-process only. Not distributed. **Not crash-durable:** data is lost on restart.
- Suitable for development and single-instance setups without durability needs.
  It is not a production store, and it may not back a durable outbox as a lease
  (see Store durability and pairing rules above).

### SQLite Store

- **Type:** `sqlite`
- **Required option:** `path` (string) -- file path for the database
- **Optional options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `stale_claim_duration` | duration | runtime-derived | Outbox only -- same semantics as the DynamoDB row below. |
| `retention` | duration | `1h` outbox / disabled DLQ | One key, per-role default. **Outbox:** window completed/expired rows are kept before piggybacked compaction deletes them; negative disables compaction (rows kept forever). Keep it above any upstream redelivery window. **DLQ:** a positive value opts the DLQ into a throttled purge of entries older than the window; zero/unset (the default) keeps every entry forever. |

- Persistent across restarts. Single-instance only.
- Suitable for single-instance production with disk durability.
- WAL journalling is always on; a single writer connection plus `busy_timeout` serialises in-process writers safely. Managed-subscription databases enforce `0600` on the database/WAL/SHM, create owned parent directories as `0700`, and reject insecure existing files or symlinks.
- Outbox honours the runtime-derived `stale_claim_duration`: a claim stranded by a crashed owner is reclaimed once it goes stale (in addition to immediate higher-version reclaim).
- **Crash-durable.** It therefore requires a crash-durable lease store: the `memory` lease renumbers its fencing versions from zero on restart and is rejected against a SQLite outbox (see Store durability and pairing rules above).
- No SQLite lease store exists -- use DynamoDB for leases when the outbox is durable.

**Fatal storage faults are a distinct alertable signal.** A disk-full, corrupt,
read-only, or not-a-database fault is classified PERMANENT and increments the
`SQLiteStoreUnhealthy` counter (Go const `sqliteoutbox.MetricStoreUnhealthy`),
tagged `entity=outbox`, alongside an error log. It is distinct from transient
throttling noise: retrying will not clear it -- the fix is to free disk or restore
the file. The classification does **not** halt the drain loop; the loop keeps
polling and records stay durable, so the counter exists purely for
observability. Watch it (see
[Monitoring](aws-deployment/monitoring.md#key-metrics)) rather than inferring the
fault from a stalled queue.

### DynamoDB Store

- **Type:** `dynamodb`
- **Options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `table_name` | `string` | `"gobridge-leases"` / `"gobridge-outbox"` / `"gobridge-dlq"` / `"gobridge-managed-subscriptions"` | DynamoDB table name (per role). |
| `stale_claim_duration` | `string` (duration) | runtime-derived (outbox only) | How long before an uncompleted outbox claim is reclaimed. Explicit value wins; when omitted, derived as `maxStepDownGrace + max(2 × maxStepDownGrace, 15s)` across all sessions (see [Configuration Reference](configuration-reference.md)). Failover reclaim via a higher fencing version is always immediate. |
| `compaction_grace` | duration | store default (`1h`) | Outbox only -- window terminal items are kept before the DynamoDB item TTL deletes them. |
| `retention` | duration | none (keep forever) | DLQ only -- TTL on dead-letter entries (`failed_at + retention`). |
| `max_scan_pages` | int | `100` | DLQ only -- bounds index-less List/Purge scans; negative disables. |
| `operation_timeout` | duration | `5s` | Managed subscriptions only -- adapter-owned deadline for each DynamoDB call. |

- Distributed and persistent. Uses conditional writes for fencing safety.
- Required for clustered deployments with lease-based coordination.
- **Boot-time schema preflight.** Each store validates its table key schema at
  build time. A confirmed schema mismatch is fatal at boot; a `DescribeTable`
  call that cannot verify the table (missing IAM permission, throttle, or backend
  gap) also fails closed. The lease role additionally requires
  `DescribeTimeToLive` and fails closed unless TTL is verified disabled on the
  fencing table. See
  [IAM Least Privilege](aws-deployment/iam.md) for the
  exact actions and posture.
- **Retention is the deduplication window.** The outbox keeps completed and
  expired rows for `retention` / `compaction_grace` before piggybacked compaction
  (or the DynamoDB item TTL) deletes them. Deleting a terminal row releases its
  duplicate-detection identity, so retention IS the duplicate-suppression cover:
  shrinking it shrinks how far back the outbox can suppress a redelivered
  message. Keep it comfortably above any upstream redelivery window.

> **Embedder-only knobs.** Some store options are programmatic by design and
> have no YAML key: `WithClock`, `WithLogger` (all stores); `WithMetrics`,
> `WithCompleteResolveRetry` (dynamodboutbox); `WithGracePeriod` (deprecated
> alias of `WithRetention`, both DynamoDB stores); and every
> `memoryoutbox`/`memorydlq` option (dev store). `WithMaxReplayCount` no longer
> exists -- the store-side replay cap was removed; poison detection is
> drainer-owned per the `ports.OutboxStore` contract.

### Outbox Replay Budget

Poison needs all three: the replay count past `max_replay_attempts`, a non-zero
first-attempt timestamp, and `replay_budget` elapsed since that first attempt:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `replay_budget` | duration | `15m` | Route policy field. Wall-clock budget from a record's first delivery attempt, bounding total redelivery time before poison. `max_replay_attempts` is the attempt floor; `replay_budget` is the time, so a transient egress outage shorter than the budget never poisons a healthy record. |

The first attempt is stored per record: SQLite adds a `first_attempted_at`
column (Unix millis, `0` = zero) via an idempotent migration and the
`CREATE TABLE` DDL; DynamoDB writes a per-item `first_attempted_at` attribute,
omitted when zero. Values absent before this schema read as zero and fall back
to the older `CreatedAt` age gate, so an upgrade never poisons a backlog.

### Decision Table

| Scenario | Lease | Outbox | DLQ |
|----------|-------|--------|-----|
| Development / testing | `memory` | `memory` | `memory` |
| Production, single instance | -- | `sqlite` | `sqlite` |
| Production, clustered | `dynamodb` | `dynamodb` | `dynamodb` |

**Important:** In clustered deployment mode, the builder rejects non-distributed
stores for all configured roles. Memory and SQLite stores are process-local and
will fail validation when `deployment_mode: clustered` is set.

---

## Programmatic Registration

Register processors and store factories on the `bridge.Builder` before
calling `Build()`:

```go
builder := bridge.NewBuilder(cfg, bridge.WithLogger(logger))

// Register processors
filterProc, _ := filter.New(filter.Config{
    Name: "my-filter", Action: filter.ActionDrop,
    Conditions: []filter.Condition{
        {Field: "header.x-env", Operator: filter.OperatorEquals, Value: "staging"},
    },
})
builder.RegisterProcessor("my-filter", filterProc)

transformProc, _ := transform.New(transform.Config{
    Name: "my-transform",
    Mappings: []transform.FieldMapping{transform.SimpleMapping("$.user.name", "username")},
})
builder.RegisterProcessor("my-transform", transformProc)

cbProc := cbproc.New("my-cb", cb.Config{
    FailureThreshold: 10, ResetTimeout: 60 * time.Second,
}, cbproc.WithKeyExtractor(cbproc.SubjectKey))
builder.RegisterProcessor("my-cb", cbProc)

tenantProc, _ := tenant.New(tenant.Config{Name: "my-tenant", RequireTenant: true})
builder.RegisterProcessor("my-tenant", tenantProc)

// Register store factories
builder.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())
builder.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
builder.RegisterStoreFactory("dynamodb", awsstore.NewDynamoDBStoreFactory(ddbClient))

rt, err := builder.Build(ctx)
```

Processor names in the YAML `processors` list must match the names passed to
`RegisterProcessor`. If a name is not found, the builder returns an error.
