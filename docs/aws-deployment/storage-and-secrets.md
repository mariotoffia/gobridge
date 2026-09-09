# Storage and Secrets on AWS

## Overview

Where a GoBridge deployment keeps its three kinds of state: the configuration
document on EFS or in a DynamoDB config table, the credentials in SSM Parameter Store, and the durable
message state in DynamoDB — with the access design and operator
responsibilities each one carries.

Part of the [AWS Deployment Overview](overview.md).

---

## DynamoDB for Configuration

Select `Bootstrap.ConfigSource = infra.ConfigSourceDynamoDB` on `GoBridgeSingle`
or `GoBridgeDynamoDBHA`, and leave `ConfigFilePath` empty. The facade creates one
config table and replaces any caller-supplied `ConfigDynamoDB.TableName` in a
copied settings value. A nil `ConfigDynamoDB` is allowed as a CDK input; it gets
the owned table name and `watch_mode: poll`.

The table has string `PK` and `SK` keys, on-demand billing, point-in-time
recovery and AWS-managed encryption. Deletion and replacement retain it; there
is no TTL. The adapter stores the versioned JSON config at
`PK = config#<bridge_id>`, `SK = current`. All HA tasks share one config table,
which is separate from their message-state and rollout tables. Streams mode
adds a `KEYS_ONLY` stream; the watcher reads the current item after notification.
See [config-source IAM grants](iam.md#config-source-grants).

An optional embedded initial document lets the control process create an absent
item at version 1. Creation uses `ports.ConfigInitializer.CreateIfAbsent`, not
version-zero compare-and-swap: a versionless legacy item is still present and
must not be replaced. The process rereads the winner after a creation race.
Worker config access stays read-only.

There is no init container, JSON S3 asset, or download grant. Go-built images
embed the facade's parsed logical document; registry images remain owned by the
consumer. Use stable physical resource names or supported selectors inside
embedded config. The config table name travels separately in bootstrap.
Switching sources does not migrate EFS admin edits; choose the initial document
deliberately. See [initialization and absence](config-initialization.md).

EFS is created and mounted only for a file config source or parsed SQLite store
paths. A DynamoDB config source with DynamoDB data stores has no EFS resources,
mounts, NFS ingress or EFS grants. A retained filesystem from an earlier stack
revision is not deleted by switching sources and can still incur charges.

## EFS for Configuration

### Why EFS Over Alternatives

| Alternative | Limitation |
|-------------|-----------|
| **Environment variables** | 4 KiB per variable, 32 KiB total. Bridge configs routinely exceed this. |
| **Embedded initial document** | Creates an absent target at initial startup; not a watched backend or an update mechanism. |
| **EFS** | Shared POSIX filesystem. Poll watcher detects changes within seconds; no restart required. |

GoBridge uses a **poll watcher** (default interval: 1 second) to detect config
file changes on the mounted EFS volume. When the file changes, the runtime
reloads routes, processors, and transports, draining in-flight messages within a
bounded window. What survives the reload is conditional: a Persistent/Exclusive
QoS 1/2 source redelivers anything not settled before the drain completes, so
those messages are not lost. An Ephemeral session, a QoS 0 source, or a drain
that aborts on timeout can drop in-flight messages. See
[MQTT — settlement semantics](../transports/mqtt-behavior.md#settlement-semantics) for the
drain bound and loss windows.

Confirmed document absence is different from a read failure. After activation,
absence stops new intake. Standalone runtimes drain to idle and can rebuild
when valid config returns. Clustered deletion or uncertain teardown signals
process exit and replacement. A timeout or authorization failure retains the
last successful config as degraded. The existing watcher reports these outcomes
explicitly. An S3 configuration adapter is deferred.

### Access Point Design

The CDK `GoBridgeEfsConfig` construct creates an EFS access point with these
defaults:

| Setting | Default | Source |
|---------|---------|--------|
| POSIX UID | `1000` | `GoBridgeEfsConfigProps.PosixUID` |
| POSIX GID | `1000` | `GoBridgeEfsConfigProps.PosixGID` |
| Access point path | `/gobridge` | `GoBridgeEfsConfigProps.AccessPointPath` |
| Directory permissions | `755` | Set in `CreateAcl` |

The EFS access point enforces this POSIX identity for every file operation
regardless of the container's process UID, so the image need not run as UID
1000 — the production image runs as the distroless nonroot user (65532:65532)
and still reads and writes `bridge.yaml` through the access point.

### Mount Path vs. Access Point Path

These two paths serve different purposes:

- **`AccessPointPath`** (`/gobridge` by default) -- the directory *inside* the
  EFS filesystem where config files live. This is set when the access point is
  created and is fixed for the life of the filesystem.
- **`MountPath`** (`/var/lib/gobridge` by default, `infra.DefaultMountPath`) --
  the directory *inside the container* where EFS is mounted. The container sees
  `/var/lib/gobridge/bridge.yaml`, but EFS stores it at `/gobridge/bridge.yaml`.

The `BootstrapConfig.ConfigFilePath` should reference the container mount path,
for example `/var/lib/gobridge/bridge.yaml`.

### File Size Limit

The file-based config loader enforces a **4 MiB** maximum file size. This is
more than enough for even the most complex multi-route configurations.

---

## SQLite stores on the config mount

A single task has one durable filesystem: the config mount. A SQLite store that
must survive the task -- `stores.managed_subscriptions` for a persistent or
exclusive MQTT session -- therefore lives there, and two rules follow from the
store's own access checks:

- **Give the database a directory of its own under the mount**, for example
  `/var/lib/gobridge/managed-subscriptions/managed-subscriptions.db`. The store
  creates that final directory itself and requires it to be owned by the
  container user with mode `0700`; the mount root the access point creates is
  `755`, so a database placed directly in it is refused. Every directory above
  it must be owned by `root` or by the container user, and must not be writable
  by group or other unless it is root-owned and sticky (`/tmp`-style).
- **Attest the session's managed-subscription baseline.** A durable session does
  not start until its baseline exists -- a missing baseline is "history
  unknown", not "no history" -- and nothing but the task can write this store.
  Declare `ManagedSubscriptionBaselines` on `GoBridgeSingle` (an empty list for
  a new broker identity, or the exact filters an existing identity still holds)
  and the runtime seeds it at every boot, idempotently, before it builds the
  bridge; the value travels as `managed_subscription_baselines` in the
  [bootstrap document](configuration.md#field-reference).

Several tasks sharing one SQLite file over EFS cannot serialize their writes,
which is why this store belongs to the single-task profile; the DynamoDB HA
profile keeps the same history in its own table and seeds the baseline at
deploy time instead.

## SSM Parameter Store for Secrets

GoBridge resolves API keys and credentials from AWS Systems Manager Parameter
Store at startup. All parameters should be stored as **SecureString** type,
which encrypts values at rest using KMS.

The logical bridge document may instead carry literal credentials where the
adapter accepts them. If embedded, those values are recoverable by readers of
the binary, image, build context, or cache. Base64 does not hide them.
`pms://` references remain logical references during initial copying and resolve
only for runtime use.

### Credential URI Mapping

The `BootstrapConfig` maps logical names to SSM parameter paths. At startup
the bootstrap library resolves each parameter and injects the plaintext value
into the runtime configuration.

| Bootstrap field | Example SSM parameter | Runtime use |
|----------------|----------------------|-------------|
| `AdminAPIKeyParam` | `/gobridge/admin-key` | Admin HTTP API `X-API-Key` header |
| `MonitorAPIKeyParam` | `/gobridge/monitor-key` | Monitor HTTP API `X-API-Key` header |
| `HTTPReceiverAPIKeyParams["rx-1"]` | `/gobridge/rx-1-key` | HTTP receiver authentication |
| `HTTPSenderAPIKeyParams["tx-1"]` | `/gobridge/tx-1-key` | HTTP sender authentication |

`AdminAPIKeyParam` is **required**; the bootstrap validator rejects configs
without it. All other parameters are optional.

The `AdminAPIKeyParam` value is either a single key (folded under the name
`admin`) or a JSON object of named keys (`{"alice":"<key>","bob":"<key>"}`).
Named keys attribute each admin action to the operator's key name in the audit
log; a plain string keeps the legacy single-key behaviour. See
[configuration](configuration.md#admin-key-parameter-value).

### KMS Encryption

The default AWS-managed SSM key (`alias/aws/ssm`) works without additional
IAM configuration. If you use a **customer-managed KMS key (CMK)**, you must
add an explicit `kms:Decrypt` grant for the ECS task role on that key.

## DynamoDB Stores

For `GoBridgeSingle` and `GoBridgeCluster`, when the bridge config uses imported
DynamoDB-backed stores (`lease`, `outbox`, `dlq`, or `managed_subscriptions`),
the stack grants each ECS task role only the adapter-specific data operations
on each table and required index the store names. Two operator responsibilities follow:

- **Pre-provision only imported tables.** `GoBridgeSingle` and
  `GoBridgeCluster` import every configured DynamoDB store, so provision those
  tables out-of-band with the adapter schema before deploying. Do **not**
  pre-provision the three names owned by `GoBridgeDynamoDBHA`: that facade creates
  its mandatory lease, outbox, and managed-subscription tables and a same-name
  resource would collide. Only an optional HA DynamoDB DLQ remains
  operator-provisioned/imported.
- **Use a resolved physical `table_name` only to override a role default.** HA
  rejects unresolved CDK tokens because token strings cannot be substituted in
  the embedded config document. When omitted, runtime
  preflight and the stack resolve and grant the same exact default:
  `gobridge-leases`, `gobridge-outbox`, `gobridge-dlq`, or
  `gobridge-managed-subscriptions`.

The adapter's expected key schemas -- what an out-of-band table must match, and
what the store's own table-creation helper provisions -- are below.
All four tables are created `PAY_PER_REQUEST` (on-demand).

**Lease table** (default `gobridge-leases`)

| Attribute | Type | Role |
|---|---|---|
| `PK` | `S` | Partition key |

No sort key and no GSIs. **DynamoDB TTL must be DISABLED** on this table: the
lease row is the fencing counter of record, and a TTL reaper that deletes it
resets the fencing version and opens a split-brain window. The lease preflight
enforces this (see [IAM Least Privilege](iam.md)).

**Outbox table** (default `gobridge-outbox`)

| Attribute | Type | Role |
|---|---|---|
| `PK` | `S` | Partition key |
| `SK` | `S` | Sort key |

| GSI | Key schema | Projection | Notes |
|---|---|---|---|
| `ExpiryIndex` | `has_expiry` (S) HASH, `expires_at` (N) RANGE | `KEYS_ONLY` | Sparse; drives expiry sweeps |
| `RecordIDIndex` | `record_id` (S) HASH | `KEYS_ONLY` | `Complete` record lookup |
| `ClaimIndex` | `PK` (S) HASH, `claim_sort` (S) RANGE | `ALL` | Sparse, age-ordered claim path |

`ClaimIndex` is **required**, and required to be `Projection: ALL`: the claim
query filters on the non-key `status` attribute, so an under-projected index
passes a key-only check and then fails every claim at runtime. `CreateTable`
provisions it and preflight rejects a table that is missing it or has it
under-projected. Ordering-keyed partitions read the base table with
`ConsistentRead` instead — no index can prove a keyed record has no older unseen
sibling — which is why `DynamoDBOutboxClaimScanPages` can rise on a correctly
provisioned table. See
[DynamoDB outbox table schema](../runbooks/dynamodb-outbox-table-schema.md).

**DLQ table** (default `gobridge-dlq`)

| Attribute | Type | Role |
|---|---|---|
| `PK` | `S` | Partition key |

| GSI | Key schema | Projection | Notes |
|---|---|---|---|
| `RouteIndex` | `route_id` (S) HASH, `failed_at` (N) RANGE | `ALL` | List/purge by route |
| `CategoryIndex` | `category` (S) HASH, `failed_at` (N) RANGE | `ALL` | List/purge by category |

DLQ entries carry no TTL by default. Setting a `retention` window enables
DynamoDB TTL on the `ttl` attribute through the store's `EnsureTable` helper.

**Managed-subscriptions table** (default `gobridge-managed-subscriptions`)

| Attribute | Type | Role |
|---|---|---|
| `storage_identity` | `S` | Partition key |

No sort key and no GSIs. It stores one baseline and an exact filter String Set
per secret-safe durable MQTT identity.

### DevMode Guard

The `BootstrapConfig.SSMEndpoint` field allows overriding the SSM endpoint for
local development against tools like LocalStack. To prevent accidental use of
custom endpoints in production, the bootstrap validator **rejects**
`SSMEndpoint` unless `DevMode` is explicitly set to `true`:

```go
// This passes validation -- DevMode enables the custom endpoint.
bootstrap := infra.BootstrapConfig{
    SSMEndpoint: "http://localhost:4566",
    DevMode:     true,
}

// This FAILS validation -- SSMEndpoint without DevMode is rejected.
bootstrap := infra.BootstrapConfig{
    SSMEndpoint: "http://localhost:4566",
}
```

When `DevMode` is `true` and `SSMEndpoint` is set, the bootstrap library
automatically configures static test credentials (`test`/`test`) so that
LocalStack access works without real AWS credentials.

---
