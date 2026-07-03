# Runbook: DynamoDB Outbox GSI Migration

**Applies to:** the `dynamodb` outbox store (`adapters/aws/store/dynamodboutbox`).
**Audience:** operators upgrading a bridge whose outbox table was created by an
earlier build.
**Risk:** medium -- the outbox holds undelivered messages; a wrong step can
strand or duplicate deliveries.

## What changed

The outbox table's secondary indexes were reshaped
(`adapters/aws/store/dynamodboutbox/acl_store.go:217-245`):

| Index | Old build | New build |
|-------|-----------|-----------|
| `StatusIndex` (PK `status`, SK `created_at`) | present | **removed** |
| `ClaimedByIndex` (PK `claimed_by`, SK `claimed_at`) | present | **removed** |
| `ExpiryIndex` (PK `has_expiry` `S`, SK `expires_at` `N`, `KEYS_ONLY`) | -- | **added, sparse** |
| `RecordIDIndex` (PK `record_id` `S`, `KEYS_ONLY`) | present | unchanged |

Base table keys are unchanged: `PK` (hash, `S`), `SK` (range, `S`) with
`SK = OUTBOX#<envelope_id>#<binding_id>`.

### Why

- `StatusIndex` was a **table-wide hot partition**: every record carried one of
  a few `status` values, and each state transition rewrote its index entry.
- `ClaimedByIndex` was created but never queried.
- `ExpiryIndex` is **sparse**: the `has_expiry` attribute (value `"1"`) is
  written only for records that carry a non-zero `expires_at`, and it is removed
  on any terminal transition. Expire sweeps therefore scan exactly the
  expiry-eligible candidate set instead of the whole table
  (`adapters/aws/store/dynamodboutbox/acl_store.go:811-860`,
  `acl_marshal.go:62-66`).

### Impact if not migrated

A table left with the old GSIs still functions for persist/claim/complete
(those use the base table and `RecordIDIndex`), but the **Expire sweep queries
`ExpiryIndex`** and will fail or find nothing if that index is absent. Records
with a set expiry will not be swept to the DLQ on time.

## Preferred path: drain, then recreate

Recreating is the simplest and safest option **once the outbox is empty**.

1. **Stop writers.** Scale the bridge fleet to zero, or route the affected
   routes to `direct_hold` so nothing new is persisted to this outbox.
2. **Drain to empty.** Let the drainers deliver all pending records. Confirm
   with a `Scan` count (or `gobridge_outbox_pending` if exported) reaching 0.
3. **Delete the table**, then let the bridge recreate it: `Store.CreateTable`
   is idempotent and provisions the new GSI shape on first start
   (`acl_store.go:197-248`). Or apply the new schema via your IaC.
4. **Resume writers.**

Because the table is empty at step 3, no messages are lost.

## In-place path: `UpdateTable` (no drain)

Use this only when you cannot drain (large backlog, strict availability).
DynamoDB permits **one GSI create or delete per `UpdateTable` call**, and a GSI
create runs an async backfill -- sequence the calls and wait for each to reach
`ACTIVE`.

1. **Delete `StatusIndex`** (`UpdateTable` `GlobalSecondaryIndexUpdates` ->
   `Delete`). Wait until it is gone.
2. **Delete `ClaimedByIndex`**. Wait.
3. **Create `ExpiryIndex`** with the new attribute definitions:
   - `has_expiry` `S` (hash), `expires_at` `N` (range), projection `KEYS_ONLY`,
     billing `PAY_PER_REQUEST` (match the table).
   Wait until the index reports `ACTIVE` (backfill complete).

`RecordIDIndex` is left untouched.

### Backfill caveat for pre-existing records

`ExpiryIndex` is sparse: DynamoDB backfills it **only** with items that already
carry the `has_expiry` attribute. Records persisted by the **old** build do not
carry `has_expiry`, so after an in-place create they will not appear in
`ExpiryIndex` and will not be swept by Expire. New writes populate it correctly.

If you have long-lived expiry-bearing records that must remain sweepable, run a
one-time backfill job: `Scan` the table and, for each item with a positive
`expires_at` and no `has_expiry`, `UpdateItem` to set
`has_expiry = "1"` (string). This is only necessary for items whose expiry has
not yet elapsed; already-past-expiry records are handled by your normal DLQ or
cleanup process.

## Verification

- `DescribeTable` shows exactly two GSIs: `ExpiryIndex` and `RecordIDIndex`,
  both `ACTIVE`, both `KEYS_ONLY`.
- Persist a test message with a short TTL and confirm the Expire sweep moves it
  to the DLQ after expiry.
- The stale package comment
  `adapters/aws/store/dynamodboutbox/doc.go:12-15` still lists the OLD GSIs and
  is a documentation-only discrepancy (tracked separately); the authoritative
  schema is `CreateTable` in `acl_store.go`.
