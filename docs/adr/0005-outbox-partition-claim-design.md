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

- **DynamoDB: bounded candidate window.** A claim gathers up to
  `claimCandidateWindowFactor * limit` candidates (`claimCandidateWindowFactor =
  3`, `acl_store.go:82`) in SK order, then sorts them client-side oldest-first.
  This restores oldest-first selection within each window.

- **Accepted residual.** A partition whose ready backlog exceeds `3 * limit` can
  leave an old record — one whose envelope ID sorts beyond the window — waiting a
  few claim cycles. Progress stays oldest-first within each window
  (`acl_store.go:75-81`). This is accepted, not a bug.

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
- The DynamoDB residual is a few extra claim cycles for a starved old record on a
  deep partition, not data loss and not permanent starvation. `OutboxDepth` and
  `OutboxClaimConflicts` make the condition observable.
- The fence row is a known hot spot on high-throughput partitions.
  `OutboxClaimConflicts` is the signal that would justify acting on it.

## Deferred upgrade paths (future work, evidence-gated)

Both are gated on `OutboxClaimConflicts` / age-skew evidence — neither is
implemented, and neither should be until the metrics show it is warranted.

1. **Exact global oldest-N.** Add a `created_at` range key (or a per-partition
   `created_at` GSI) so the query returns records in age order, and shrink the
   candidate window to `limit` (`acl_store.go:79-81`). Removes the residual
   entirely at the cost of an extra index to write and migrate — see the
   [DynamoDB outbox GSI migration runbook](../runbooks/dynamodb-outbox-gsi-migration.md).

2. **Resharded seq allocation.** Split the single per-partition `seq_counter`
   into shards to cut fence-row contention on hot partitions. Reduces
   `TransactionConflict` at the cost of more complex ordering reconstruction.

## Rejected alternatives

- **Claim DynamoDB items in raw SK order.** Random with respect to age; starves
  late-sorting envelope IDs and breaks the ordering contract. The candidate
  window exists to avoid exactly this.
- **Add the `created_at` GSI now.** An index carries write cost and a migration
  on every deployment. Deferred until `OutboxClaimConflicts` / age-skew evidence
  shows the residual matters in practice.
- **Use `QueryPending` for selection.** It is count/preview semantics and not
  oldest-N on DynamoDB. Overloading it would silently reintroduce the starvation
  the `Claim` window prevents.
