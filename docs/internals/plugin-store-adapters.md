# Store adapters

Store adapters provide persistence for leases, outbox records, DLQ entries, and exact managed-subscription history.

### Port Interfaces

From `ports/stores.go` (the outbox contract lives in `ports/stores_outbox.go`):

```go
type LeaseStore interface {
    Acquire(ctx context.Context, leaseID string, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error)
    Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error)
    Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error
    Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error)
}

type OutboxStore interface {
    Persist(ctx context.Context, records []*persistence.OutboxRecord) error
    Claim(ctx context.Context, partitionKey string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error)
    Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error
    Expire(ctx context.Context, before time.Time, partition string, token persistence.LeaseToken) (int, error)
    QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error)
}

type DLQStore interface {
    Write(ctx context.Context, entry routing.DLQEntry) error
    List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error)
    Replay(ctx context.Context, entryIDs []string) error
    Purge(ctx context.Context, before time.Time) (int, error)
}

type ManagedSubscriptionStore interface {
    List(ctx context.Context, storageIdentity string) ([]string, error)
    Remember(ctx context.Context, storageIdentity string, filters []string) error
    Forget(ctx context.Context, storageIdentity string, filters []string) error
}
```

An `OutboxStore` MAY additionally implement these OPTIONAL capabilities. Skipping
one costs a feature, never a build or behaviour break:

```go
// Return a transiently-failed claim to pending immediately, so it is
// re-claimable on the next drain without a fencing bump or a stale timeout.
type OutboxReleaser interface {
    Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error
}

// The true pending backlog behind shared.MetricOutboxDepth.
type OutboxDepthReporter interface {
    CountPending(ctx context.Context, partitionKey string) (int, error)
}

// Records currently CLAIMED, behind shared.MetricOutboxClaimedDepth. CountPending
// excludes them, so without this a record stranded by a failed release is
// invisible: the backlog gauge reads zero while messages sit undelivered.
type OutboxClaimedDepthReporter interface {
    CountClaimed(ctx context.Context, partitionKey string) (int, error)
}
```

Two `Claim` rules bind every implementation and are pinned by the shared
conformance suite (`ports/storetest`) — read the contract in `ports/stores_outbox.go`
before writing one:

- **Ordering-key head-of-line.** A record carrying `x-bridge.ordering-key` is
  claimable only when the partition holds no OLDER non-terminal record on the
  same key that the same `Claim` will not also return. Order is a durable
  property: the drainer sequences same-key records inside one batch but cannot
  see a sibling left `Claimed` by an earlier cycle.
- **A short batch is legal.** If a per-record claim fails transiently after
  earlier records were durably claimed, return `(claimed, nil)` — never discard
  them. Only `shared.ErrStaleFencingToken` surfaces with no records.

### Store Factory (ports-first)

Implement `ports.StoreFactory` (from `ports/stores.go`):

```go
type StoreFactory interface {
    NewLeaseStore(ctx context.Context, cfg ports.PluginConfig) (LeaseStore, error)
    NewOutboxStore(ctx context.Context, cfg ports.PluginConfig, runtime OutboxRuntimeOptions) (OutboxStore, error)
    NewDLQStore(ctx context.Context, cfg ports.PluginConfig) (DLQStore, error)
}
```

Each method receives the typed `ports.PluginConfig` the adapter
registered on `*ports.Registry` (see
[Typed Plugin Config](../../PLUGIN.md#typed-plugin-config) below). The factory does
its own type assertion on the concrete config type — it never sees
`map[string]any`.

Optional companion interfaces:

- `ports.ManagedSubscriptionStoreFactory.NewManagedSubscriptionStore(...)` — exact durable MQTT filter history; separate from `StoreFactory` so unrelated plugins are not widened.
- `ports.DistributedStoreFactory.IsDistributed() bool` — returns true
  when the store provides cross-process coordination. Required for
  clustered deployments.

Return `(nil, nil)` for store types the factory does not support.

### Registration

```go
builder.RegisterStoreFactory("mybackend", myStoreFactory)
```

Config:

```yaml
stores:
  lease:
    type: mybackend
    options:
      connection_string: "..."
```

### Conformance Testing

Use the built-in conformance test suites in `ports/storetest/`:

```go
func TestMyStore(t *testing.T) {
    store := mybackend.NewStore(/* ... */)
    storetest.RunDLQStoreTests(t, store)
    storetest.RunOutboxStoreTests(t, store)
    storetest.RunLeaseStoreTests(t, store, nil)
}
```

These suites verify all required behaviors (idempotency, filtering, fencing, etc.).

### Reference Implementations

- **Memory**: `adapters/native/store/memory*/` -- sync.Mutex + maps, good for tests
- **SQLite**: `adapters/native/store/sqlite*/` -- WAL mode, modernc.org/sqlite, JSON marshaling
- **DynamoDB**: `adapters/aws/store/dynamodb*/` -- conditional writes, GSIs, TTL compaction, and atomic managed-filter sets
