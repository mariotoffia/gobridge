# Runbook: DynamoDB Outbox Table Schema

**Applies to:** the `dynamodb` outbox store.
**Audience:** operators provisioning the outbox table by hand or through IaC, and
anyone diagnosing a startup that fails with `INVALID_CONFIG` naming this table.
**Risk:** medium -- the outbox holds undelivered messages; a wrong schema fails
the bridge closed rather than losing them, but it does stop delivery.

> GoBridge provisions this table itself. `Store.CreateTable` creates the base
> table and all three indexes in one idempotent call, and the
> factory preflight verifies the shape at build time. This runbook exists for
> deployments that provision through IaC and for reading a preflight rejection.

## Required schema

| Component | Key schema | Projection | Purpose |
|---|---|---|---|
| Base table | `PK` `S` (hash), `SK` `S` (range) | -- | Records and the per-partition `FENCE` row |
| `ExpiryIndex` | `has_expiry` `S` (hash), `expires_at` `N` (range) | `KEYS_ONLY` | Expiry sweeps read only expiry-eligible records |
| `RecordIDIndex` | `record_id` `S` (hash) | `KEYS_ONLY` | `Complete` resolves a record ID to its base-table keys |
| `ClaimIndex` | `PK` `S` (hash), `claim_sort` `S` (range) | **`ALL`** | Age-ordered claim path |

All three indexes are **required**. Preflight rejects a table that is missing one
or whose key schema differs, naming the table, the expected shape, and the actual
shape.

### Why `ClaimIndex` must be `Projection: ALL`

The claim query filters on the non-key `status` attribute. A `KEYS_ONLY` or
`INCLUDE`-projected `ClaimIndex` passes a key-schema check and then fails
**every** claim at runtime with a `does not project one or more filter
attributes` `ValidationException`. Preflight therefore checks the projection too
and rejects the table at boot rather than letting it wedge delivery later. If a
misprovisioned index somehow reaches a claim, the error is surfaced naming the
index — it is never degraded around.

### Sparse indexes

`ExpiryIndex` and `ClaimIndex` are both sparse, which is what bounds their cost:

- `has_expiry` (value `"1"`) is written only for records with a non-zero
  `expires_at`, and removed on any terminal transition. A sweep reads exactly the
  expiry-eligible set, never the whole table.
- `claim_sort` (a zero-padded `created_at`-millis + `seq` composite) is stamped
  at `Persist` and removed at `Complete`/`Expire`, so the index holds exactly the
  pending/claimed working set. A claim query reads it oldest-first and stops
  after `limit`, so a healthy backlog drains in O(limit) Query cost.

## Ordering keys bypass the index

Records carrying `x-bridge.ordering-key` are claimed through the base table with
`ConsistentRead`, never through `ClaimIndex`. A global secondary index is
eventually consistent and propagates per item, so it cannot prove a record has no
older unseen sibling on the same key — the claim would deliver the younger one
first with no failure anywhere. A claim that sees any ordering key abandons the
index and re-runs on the base table (see
[ADR 0005](../adr/0005-outbox-partition-claim-design.md)).

Nothing about that needs provisioning. What it changes is **cost**: a keyed
partition pays an O(backlog) scan per claim even on a correct table.
`DynamoDBOutboxClaimScanPages` rising on a table that HAS `ClaimIndex` therefore
means ordering keys, not a missing index; the store logs the reason once per
process.

## Provisioning by hand or IaC

Match the table above exactly, then start the bridge and let preflight confirm
it. Notes for a hand-rolled `UpdateTable` sequence:

- DynamoDB permits **one GSI create or delete per `UpdateTable` call**, and a
  create runs an async backfill. Sequence the calls and wait for each index to
  report `ACTIVE` before issuing the next.
- Set `ProjectionType: ALL` on `ClaimIndex`. `KEYS_ONLY` is the common mistake
  and is rejected at boot.
- DynamoDB item TTL should be **enabled** on the `ttl` attribute. Only terminal
  records and fence rows carry it; pending and claimed records never do, so TTL
  can never reap undelivered work.

## Diagnosing a rejection

A preflight failure is a fatal `INVALID_CONFIG` at build time and names what it
found. The two common causes:

| Symptom | Cause | Fix |
|---|---|---|
| `required global secondary index "ClaimIndex" is missing` | Table provisioned without it | Create it (hash `PK`, range `claim_sort`, `Projection: ALL`) |
| `GSI "ClaimIndex" must project ALL attributes` | Created `KEYS_ONLY` or `INCLUDE` | A projection cannot be altered in place — delete and recreate the index |

A `ClaimIndex` delete/recreate is safe at any time: it is sparse over the
pending/claimed working set, and `Persist` keeps writing `claim_sort` throughout,
so the rebuilt index is complete once it reports `ACTIVE`. Claims fail while it
is absent, which stops delivery but loses nothing — records stay pending.

## Verification

- `DescribeTable` shows the base table plus `ExpiryIndex`, `RecordIDIndex`
  (both `KEYS_ONLY`) and `ClaimIndex` (**`Projection: ALL`**), all `ACTIVE`.
- The bridge starts without a preflight rejection.
- Persist a test message with a short TTL and confirm the expiry sweep moves it
  to the DLQ after expiry.
- `OutboxDepth` drains and `DynamoDBOutboxClaimScanPages` stays flat on a
  keyless route.

## Related

- [ADR 0005 — outbox partition claim design](../adr/0005-outbox-partition-claim-design.md)
- [DynamoDB store outage and throttling](dynamodb-store-outage-throttling.md)
- [Outbox backlog / stuck drain](outbox-backlog-stuck-drain.md)
