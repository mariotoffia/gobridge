# 0005 — Outbox partition design: claim selection, fence rows, seq allocation

Status: accepted
Date: 2026-07-03
Deciders: GoBridge core

## Context

The durable outbox drains records in order and must not lose or reorder them
across three backends: in-memory, SQLite, and DynamoDB. Two behaviors need a
cross-backend contract:

- **Claim** hands the drainer the next batch of pending records to send.
- **QueryPending** reports outbox depth for backpressure and preview.

A relational backend answers "oldest N pending" with `ORDER BY created_at, seq
LIMIT N`. DynamoDB cannot: a `Query` returns items in sort-key order, and the
outbox sort key is `OUTBOX#<envelope_id>#<binding_id>` — lexicographic by
envelope ID, effectively random with respect to record age
(`adapters/aws/store/dynamodboutbox/acl_store.go:66-71`). Claiming the first N
items DynamoDB returns would starve records whose envelope IDs sort late and
break the ordering the other backends provide.

## Decision

Fix an ordering contract on the port, then accept a bounded approximation in the
DynamoDB backend, with the exact-ordering upgrades recorded as evidence-gated
future work.

- **Claim contract: oldest-N by `(CreatedAt, Seq)`.** `Claim` returns the oldest
  N pending records by ascending `(CreatedAt, Seq)` as a cross-backend contract
  (`ports/stores_outbox.go`). `Seq` is monotonic per partition; `limit <= 0` is a
  fencing no-op; stores must not filter by replay count. The contract test
  `ClaimLimitReturnsOldestN` (`ports/storetest/outbox.go:55`, `:264`) enforces it
  for every backend.

- **QueryPending is preview, not selection.** `QueryPending` returns depth /
  preview / count only, used against `MaxOutboxDepth`, and is explicitly not
  oldest-N selection (`ports/stores_outbox.go`). DynamoDB answers it in SK order.
  Callers that need oldest-N selection use `Claim`, never `QueryPending`.

- **DynamoDB: exhaustive partition scan, bounded retention.** A claim pages the
  partition to exhaustion (until `LastEvaluatedKey == nil`) and RETAINS only the
  oldest `claimRetentionFactor * limit` candidates by `(CreatedAt, Seq,
  EnvelopeID)` (`claimRetentionFactor = 3`, `acl_store.go`), trimming the
  retained set back whenever it grows past `2 * retain`. Because every claimable
  record in the partition is CONSIDERED, the retained oldest-N are the TRUE
  oldest-N — contract parity with the memory/SQLite `ORDER BY created_at, seq
  LIMIT`.

- **Per-partition fence row.** A single fence row per partition holds the
  monotonic `max_claim_version` and an atomic `seq_counter`
  (`attrSeqCounter = "seq_counter"`, `acl_store.go:38-50`). `Persist` allocates
  `Seq` by incrementing the counter (`allocateSeqs`, `acl_store.go:415-473`);
  `Claim` condition-checks the same fence row inside its `TransactWriteItems`.
  Both paths touch one row per partition, so a hot partition contends on it.

- **Ordering-key head-of-line.** A record carrying
  `messaging.HeaderOrderingKey` is claimable only when the partition holds no
  OLDER non-terminal record (pending or claimed) with the same key that the same
  `Claim` will not itself return. Per-key order is a DURABLE property, not a
  per-batch one: the drainer sequences same-key records inside one claimed batch,
  but it cannot see a sibling left `Claimed` by a previous cycle — a failed
  `Release`, an abandoned batch, a crashed owner. Without the rule a younger
  sibling is claimed and delivered while the stranded head still waits, with zero
  errors anywhere. A blocked record is simply not returned; it becomes claimable
  again as soon as its head goes terminal or is released, so the rule delays
  work, never strands it. Keyless records are unaffected and keep full
  concurrency. Enforced by `ClaimBlocksYoungerSiblingOfStrandedHead` and
  siblings in the shared storetest suite, so all three backends prove it.

- **DynamoDB reads the base table when a partition carries ordering keys.** A
  global secondary index cannot be read strongly consistent and its propagation
  is per item and unordered, so `ClaimIndex` can surface a younger same-key
  record before its older sibling. The moment a claim sees ANY ordering key it
  abandons the index — having claimed nothing — and re-runs through the
  `ConsistentRead` base-table scan, which sees every sibling. Keyless partitions
  keep the O(limit) fast path; keyed partitions pay the exhaustive-scan cost for
  correct order, logged once per store and visible as `DynamoDBOutboxClaimScanPages`.
  The alternative — a local secondary index on `(PK, claim_sort)` read
  consistently — was rejected for now: an LSI can only be created with the table,
  so it cannot be added to a live deployment, and it caps an item collection at
  10 GB per partition key.

- **The ordering key is denormalised.** Reading it from the embedded envelope
  would mean unmarshalling every scanned record on the claim path, so SQLite
  persists an `ordering_key` column with a partial index and DynamoDB stamps an
  `ordering_key` attribute at `Persist`; the in-memory store reads it off the
  aggregate. There is no data migration and no backfill — GoBridge has never
  been deployed, so no store holds a record written before the column existed,
  and the attribute is the single source of truth for a record's key.

- **A short claim batch is legal.** DynamoDB claims one record per
  `TransactWriteItems`, so a later transaction can fail after earlier records are
  already durably claimed. Those records belong to the caller and are excluded
  from `CountPending`, so returning them alongside an error — which the drainer
  discards — strands them until the wall-clock stale window and charges a replay
  attempt per recovery cycle, eventually poisoning them to the dead-letter queue
  without a single send. `Claim` therefore returns `(claimed, nil)` on a
  transient mid-batch failure and counts `DynamoDBOutboxClaimTruncated`. Only
  `ErrStaleFencingToken` still surfaces with no records: the owner has lost the
  partition and must stop.

- **Contention is measured.** `Claim` conflicts increment
  `MetricOutboxClaimConflicts = "OutboxClaimConflicts"`
  (`domain/shared/metrics.go:45`), tagged with the partition. It counts
  DynamoDB `TransactionConflict` (real contention) and distinguishes it from
  `ConditionalCheckFailed` (a claimer that legitimately lost the race),
  `acl_store.go:96-100`.

## Consequences

- All three backends satisfy the same `Claim` ordering contract, verified by the
  shared storetest suite.
- No starvation: every Claim considers every claimable record in the partition,
  so an old record whose envelope ID sorts late can no longer be permanently
  skipped — the liveness bug the bounded window had. Client memory stays
  bounded (~`2 * retain`) because the retained set is trimmed as it pages, and
  cancellation is honoured between pages so a Claim never outlives its context.
- Tradeoff: on the strongly consistent scan path — every partition carrying
  ordering keys — each Claim reads the WHOLE non-terminal backlog of the
  partition, so draining N records costs ~`N / limit` scans (quadratic in a deep
  backlog). This is observable: once a single Claim crosses `deepBacklogPageWarn`
  pages the store emits a loud WARN and increments
  `DynamoDBOutboxClaimScanPages` (tagged by partition). Keyless partitions never
  reach it, so a rising counter always means ordering keys.
- Ordering keys concentrate throughput: a partition whose traffic shares one key
  drains strictly sequentially and stalls entirely behind a stranded head. That
  is the point — it is what per-key order means — but it makes
  `OutboxClaimedDepth` standing non-zero with `OutboxDepth` at zero the signature
  to alarm on.
- The fence row is a known hot spot on high-throughput partitions.
  `OutboxClaimConflicts` is the signal that would justify acting on it.

## Deferred upgrade paths (future work, evidence-gated)

All are gated on `OutboxClaimConflicts` / scan-page / age-skew evidence — none is
implemented, and none should be until the metrics show it is warranted.

1. **Strongly consistent age-ordered read for keyed partitions.** The
   `ClaimIndex` GSI already gives keyless partitions the O(limit) read. Keyed
   partitions fall back to the exhaustive `ConsistentRead` scan because a GSI
   cannot answer "is there an older sibling I have not seen". A local secondary
   index on `(PK, claim_sort)` read with `ConsistentRead: true` would give them
   the same bound — gated on a table migration (an LSI can only be created with
   the table) and on the 10 GB item-collection limit per partition key. Until
   then, a keyed partition with a deep backlog is the case to watch:
   `DynamoDBOutboxClaimScanPages` rising on a table that HAS `ClaimIndex` is the
   signal.

2. **Age-ordered query (historical).** Add a `created_at` range key (or a
   per-partition `created_at` GSI) so the query returns records in age order, and
   shrink retention from `claimRetentionFactor * limit` to `limit`. This removes
   the O(backlog) per-Claim SCAN COST — the scan can stop after `limit` instead of
   reading the whole partition — at the cost of an extra index to write and
   migrate. Any such index must be provisioned by `CreateTable`/`EnsureTable` and
   verified by the factory schema preflight, never hand-provisioned — see the
   [DynamoDB outbox table schema runbook](../runbooks/dynamodb-outbox-table-schema.md).

3. **Resharded seq allocation.** Split the single per-partition `seq_counter`
   into shards to cut fence-row contention on hot partitions. Reduces
   `TransactionConflict` at the cost of more complex ordering reconstruction.

## Rejected alternatives

- **Claim DynamoDB items in raw SK order.** Random with respect to age; starves
  late-sorting envelope IDs and breaks the ordering contract. The exhaustive
  age-sorted scan exists to avoid exactly this.
- **Add the `created_at` GSI now.** An index carries write cost and a migration
  on every deployment. Deferred until the deep-backlog scan cost
  (`DynamoDBOutboxClaimScanPages` / the deep-backlog WARN) shows the per-Claim
  O(backlog) scan matters in practice.
- **Use `QueryPending` for selection.** It is count/preview semantics and not
  oldest-N on DynamoDB. Overloading it would silently reintroduce the starvation
  the exhaustive scan prevents.
