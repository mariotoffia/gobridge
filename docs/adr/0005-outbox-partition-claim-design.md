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
  (`ports/stores.go:45-52`). `Seq` is monotonic per partition; `limit <= 0` is a
  fencing no-op; stores must not filter by replay count. The contract test
  `ClaimLimitReturnsOldestN` (`ports/storetest/outbox.go:55`, `:264`) enforces it
  for every backend.

- **QueryPending is preview, not selection.** `QueryPending` returns depth /
  preview / count only, used against `MaxOutboxDepth`, and is explicitly not
  oldest-N selection (`stores.go:54-62`). DynamoDB answers it in SK order.
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
- Tradeoff: each Claim reads the WHOLE claimable backlog of the partition, so
  draining N records costs ~`N / limit` scans (quadratic in a deep backlog). This
  is observable — once a single Claim crosses `deepBacklogPageWarn` pages the
  store emits a loud WARN and increments `DynamoDBOutboxClaimScanPages` (tagged by
  partition). A sustained deep backlog is the signal to provision the deferred
  `created_at` index below.
- The fence row is a known hot spot on high-throughput partitions.
  `OutboxClaimConflicts` is the signal that would justify acting on it.

## Deferred upgrade paths (future work, evidence-gated)

Both are gated on `OutboxClaimConflicts` / age-skew evidence — neither is
implemented, and neither should be until the metrics show it is warranted.

1. **Age-ordered query.** Add a `created_at` range key (or a per-partition
   `created_at` GSI) so the query returns records in age order, and shrink
   retention from `claimRetentionFactor * limit` to `limit`. This removes the
   O(backlog) per-Claim SCAN COST — the scan can stop after `limit` instead of
   reading the whole partition — at the cost of an extra index to write and
   migrate. Any such index must be provisioned by `CreateTable`/`EnsureTable` and
   verified by the factory schema preflight, never hand-provisioned — see the
   [DynamoDB outbox GSI migration runbook](../runbooks/dynamodb-outbox-gsi-migration.md).

2. **Resharded seq allocation.** Split the single per-partition `seq_counter`
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
