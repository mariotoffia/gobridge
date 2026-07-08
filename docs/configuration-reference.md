# Configuration Model Reference

Complete field-by-field reference for the `BridgeConfig` YAML/JSON model. All field names match the YAML/JSON struct tags in `ports/blueprint.go`.

> **Duration format.** Every duration field takes a string with a unit
> (`"30s"`, `"5m"`, `"1h30m"`). A bare number (`30`) is **rejected** by the
> strict decoder -- it would be read as nanoseconds, which is never intended.

## Config Model Overview

```mermaid
classDiagram
    class BridgeConfig {
        bridge: BridgeSettings
        config_watch: ConfigWatchDef
        stores: StoresConfig
        sessions: SessionDef[]
        receivers: ReceiverDef[]
        senders: SenderDef[]
        bindings: BindingDef[]
        routes: RouteDef[]
        http: HTTPConfig
    }

    class BridgeSettings {
        id: string
        instance_id: string
        deployment_mode: string
        shutdown_timeout: string
        drain_timeout: string
        per_record_drain_timeout: string
        max_drain_timeout: string
        log_level: string
        cluster: ClusterConfig
    }

    class RouteDef {
        id: string
        receiver_id: string
        delivery_mode: string
        dispatch_mode: string
        policy: PolicyDef
        bindings: string[]
        processors: string[]
        resolver: ResolverDef
        session: RouteSessionDef
    }

    class ResolverDef {
        type: string
        default_binding: string
        header_key: string
        header_map: map
        rules: RuleDef[]
    }

    class RuleDef {
        binding_id: string
        match: ConditionDef[]
    }

    class ConditionDef {
        field: string
        operator: string
        value: any
    }

    class PolicyDef {
        max_in_flight: int
        ack_after: string
        max_replay_attempts: int
        replay_budget: string
        backoff: BackoffDef
        on_expired: string
        on_permanent_failure: string
        on_filtered: string
    }

    BridgeConfig --> BridgeSettings
    BridgeConfig --> RouteDef
    RouteDef --> PolicyDef
    RouteDef --> ResolverDef
    RouteDef --> RouteSessionDef
    ResolverDef --> RuleDef
    RuleDef --> ConditionDef
    PolicyDef --> BackoffDef
    RouteSessionDef --> DrainStrategyDef
```

## `bridge` -- Bridge Settings

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique bridge identifier |
| `instance_id` | string | no | auto-generated | Instance identifier (useful in clustered mode) |
| `deployment_mode` | string | no | `standalone` | `standalone` or `clustered` |
| `shutdown_timeout` | duration | no | `30s` | Grace period for clean shutdown |
| `drain_timeout` | duration | no | `30s` | Legacy fixed ceiling for a drain batch. Retained for backward compatibility; prefer `per_record_drain_timeout` + `max_drain_timeout` for production workloads. |
| `per_record_drain_timeout` | duration | no | `3s` (when paired) | Per-record budget in the scaled formula `ceiling = min(batchCount * per_record_drain_timeout, max_drain_timeout)`. Setting this or `max_drain_timeout` activates the scaled formula and supersedes `drain_timeout`. |
| `max_drain_timeout` | duration | no | `10s` (when paired) | Upper bound for the scaled drain formula. Must be >= `per_record_drain_timeout`. |
| `log_level` | string | no | `info` | Log level: `debug`, `info`, `warn`, `error` |

### `bridge.cluster` -- Cluster Config

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `endpoints` | map[string]string | no | auto-discovered | Static endpoint override `{instance_id: address}` |

```yaml
bridge:
  id: production-bridge
  instance_id: instance-01
  deployment_mode: clustered
  shutdown_timeout: 45s
  # Prefer the scaled drain formula for production workloads. The batch
  # deadline is computed as min(batchCount * per_record_drain_timeout,
  # max_drain_timeout); drain_timeout is ignored when either new field
  # is set and is retained only for backward compatibility.
  per_record_drain_timeout: 3s
  max_drain_timeout: 30s
  log_level: info
  cluster:
    endpoints:
      instance-01: "10.0.1.10:8080"
      instance-02: "10.0.1.11:8080"
```

## `config_watch` -- File Watch Settings

Controls how file-based config changes are detected. Only relevant when using file watchers.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `mode` | string | no | `notify` | `notify` (fsnotify) or `poll` (SHA-256 comparison) |
| `poll_interval` | duration | no | `30s` | Polling interval when mode is `poll` |
| `debounce` | duration | no | `100ms` | Debounce period for `notify` mode |

```yaml
config_watch:
  mode: poll
  poll_interval: 30s
```

## `stores` -- Backing Store Configuration

Configures the backing stores for lease coordination, outbox persistence, and dead-letter queue.

### Store Roles

| Role | Purpose | Required When |
|------|---------|---------------|
| `lease` | Distributed lease ownership | Exclusive sessions in clustered mode |
| `outbox` | Durable message outbox | `delivery_mode: shared_outbox` |
| `dlq` | Dead-letter queue | `on_expired: dlq`, `on_permanent_failure: dlq`, or `on_filtered: dlq` |

### Store Config Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | **yes** | Store backend: `memory`, `sqlite`, `dynamodb` |
| `options` | map | no | Backend-specific options |

**Memory**: no options required.

**SQLite** (`type: sqlite`):

| Option | Type | Applies to | Default | Description |
|--------|------|------------|---------|-------------|
| `path` | string | all roles | -- (**required**) | Database file path (`:memory:` for an in-process DB) |
| `stale_claim_duration` | duration | outbox | runtime-derived | How long a same-owner stranded claim waits before another claim attempt may take it. Failover reclaim via a higher fencing version is always immediate and independent of this. |
| `retention` | duration | outbox | `1h` | Window completed/expired outbox rows are kept before piggybacked compaction deletes them. Negative disables compaction (rows kept forever). Keep comfortably above any upstream redelivery window, since deleting a terminal row releases its duplicate-detection identity. |

**DynamoDB** (`type: dynamodb`):

| Option | Type | Applies to | Default | Description |
|--------|------|------------|---------|-------------|
| `table_name` | string | all roles | store built-in | Overrides the DynamoDB table name. |
| `stale_claim_duration` | duration | outbox | runtime-derived | Same semantics as the SQLite outbox key above. |
| `compaction_grace` | duration | outbox | store default | Window completed/expired outbox items are kept before the DynamoDB item TTL deletes them. Keep above any upstream redelivery window. |
| `retention` | duration | DLQ | none (kept until deleted) | TTL on dead-letter entries (`ttl = failed_at + retention`). Use a days-scale value so investigators have time to inspect dead-lettered messages. |
| `max_scan_pages` | int | DLQ | `100` | Bounds the number of DynamoDB pages an index-less List/Purge reads before stopping, preventing an unbounded full-table scan. Negative disables the bound. |

> The AWS region and endpoint are NOT store options: they come from the standard
> AWS SDK configuration chain (environment, shared config, IAM role). Supplying
> `options.region` or `options.endpoint` is rejected by the strict decoder.

**DLQ read ordering** is oldest-first (`failed_at ASC`) across all backends, so
operators triage the earliest failures first.

**Fence retention floor (30 days).** Independently of `retention` /
`compaction_grace`, the outbox keeps each partition's *fence* row (its dedup /
ordering high-water-mark) for at least `max(retention_or_compaction_grace, 30d)`
past the last write that touches the partition. Setting a shorter `retention`
does not shorten this floor: it bounds how aggressively terminal *record* rows
are compacted, not the fence. The floor stops ephemeral/rotating session
partitions from accreting one immortal fence row each, while 30 days of
abandonment is deemed safe because such a partition has no competing owner left
to fence (`sqliteoutbox/outbox.go:35`, `dynamodboutbox/acl_store.go:72`).

**DynamoDB outbox GSI change (breaking for existing tables).** The outbox table's
secondary indexes changed: the former `StatusIndex` (a table-wide hot partition
rewritten on every state transition) and the never-queried `ClaimedByIndex` were
removed, and a sparse `ExpiryIndex` (hash `has_expiry`, range `expires_at`,
`KEYS_ONLY`) was added alongside the existing `RecordIDIndex`. A freshly created
table (`Store.CreateTable`) provisions the new shape automatically; a table
created by an earlier build must be migrated. See
[DynamoDB outbox GSI migration](runbooks/dynamodb-outbox-gsi-migration.md).

Set `stale_claim_duration` above your worst-case drain-batch timeout;
`step_down_grace + 15s` (~20s) is a safe rule of thumb.

```yaml
stores:
  lease:
    type: dynamodb
    options:
      table_name: gobridge-leases
  outbox:
    type: dynamodb
    options:
      table_name: gobridge-outbox
      stale_claim_duration: 30s
  dlq:
    type: memory
```

## `sessions` -- Transport Sessions

Sessions represent stateful transport connections (e.g. an MQTT connection). Stateless transports (SQS, Azure SB) do not use sessions.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique session identifier |
| `transport` | string | **yes** | -- | Transport name: `mqtt`, `sqs`, `servicebus`, `http` |
| `session_mode` | string | no | `ephemeral` | `ephemeral`, `persistent`, `exclusive` |
| `options` | map | no | -- | Transport-specific options (see [Transport Reference](transport-configuration.md)) |

**Session modes:**
- **`ephemeral`** -- Default. Clean session, no state between connections.
- **`persistent`** -- Session state preserved across reconnections (MQTT: `clean_start=false`).
- **`exclusive`** -- Lease-based single holder. Only one instance owns this session at a time. Requires a lease store.

```yaml
sessions:
  - id: mqtt-conn
    transport: mqtt
    session_mode: exclusive
    options:
      session:
        broker_urls:
          - tcp://mqtt.example.com:1883
        client_id: bridge-primary
```

## `receivers` -- Message Ingress

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique receiver identifier |
| `transport` | string | cond. | -- | Transport name (required if no `session_id`) |
| `session_id` | string | cond. | -- | Reference to a session (required if no `transport`) |
| `topics` | array | no | -- | Subscription definitions |
| `options` | map | no | -- | Transport-specific receiver options |

### `receivers[].topics[]` -- Subscriptions

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `topic` | string | **yes** | -- | Topic or subject pattern |
| `qos` | int | no | 0 | Quality of service (MQTT: 0, 1, 2) |
| `options` | map | no | -- | Per-subscription options |

```yaml
receivers:
  - id: sensor-in
    session_id: mqtt-conn
    topics:
      - topic: "sensors/temperature/#"
        qos: 1
      - topic: "sensors/humidity/#"
        qos: 1
```

**Note:** Either `transport` or `session_id` must be set. When `session_id` is set, the transport is inferred from the session.

## `senders` -- Message Egress

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique sender identifier |
| `transport` | string | cond. | -- | Transport name (required if no `session_id`) |
| `session_id` | string | cond. | -- | Reference to a session |
| `options` | map | no | -- | Transport-specific sender options |

```yaml
senders:
  - id: sqs-out
    transport: sqs
    options:
      queue_url: https://sqs.us-west-1.amazonaws.com/123456/events
      batch_size: 10
```

## `bindings` -- Route Destinations

Bindings connect routes to senders with a specific address.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique binding identifier |
| `sender_id` | string | **yes** | -- | Reference to a sender |
| `session_id` | string | no | -- | Optional session override |
| `address` | string | **yes** | -- | Destination address -- supports `{header}` templates |
| `options` | map | no | -- | Per-binding options -- **parsed but not consumed at runtime** (`DestinationBinding.Config` is never read); leave bindings bare and configure the transport on the sender. |

### Address Templates

The `address` field supports `{placeholder}` tokens that are resolved at runtime from envelope headers:

```yaml
bindings:
  - id: to-factory-topic
    sender_id: mqtt-out
    address: "factory/{factory_id}/orders/{device_id}"
```

A message with headers `factory_id: "A"` and `device_id: "42"` resolves to topic `factory/A/orders/42`. The MQTT sender uses this as the publish topic. Missing placeholders cause an error. See [Dynamic Destination Routing](scenarios/12-dynamic-destination-routing.md) for full details.

```yaml
bindings:
  - id: to-events-queue
    sender_id: sqs-out
    address: events
  - id: to-archive-topic
    sender_id: mqtt-out
    address: archive/events
```

## Routes, Runtime & Validation

The remainder of the configuration model -- `routes`, the `http` API block, the
ID reference graph, delivery hooks, programmatic builder/lifecycle notes, and
the validation-rules summary -- is documented in
[Routes, Runtime & Validation Reference](routes-and-runtime-reference.md). That
reference also covers the per-route lease-timing knobs under `routes[].session`,
including `renew_call_timeout` and `acquire_poll_interval` and their role in the
failover-latency budget.

