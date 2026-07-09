# Runbook: DynamoDB Outbox GSI Migration

**Applies to:** the `dynamodb` outbox store (`adapters/aws/store/dynamodboutbox`).
**Audience:** operators upgrading a bridge whose outbox table was created by an
earlier build.
**Risk:** medium -- the outbox holds undelivered messages; a wrong step can
strand or duplicate deliveries.

## What changed

The outbox table's secondary indexes were reshaped, and the base-table sort-key
VALUE encoding was made injective. Current schema
(`adapters/aws/store/dynamodboutbox/acl_store.go:326-395`; preflight and the
authoritative schema comment: `acl_preflight.go:35-96`, `doc.go`):

| Index | Old build | Current build |
|-------|-----------|---------------|
| `StatusIndex` (PK `status`, SK `created_at`) | present | **removed** |
| `ClaimedByIndex` (PK `claimed_by`, SK `claimed_at`) | present | **removed** |
| `ExpiryIndex` (PK `has_expiry` `S`, SK `expires_at` `N`, `KEYS_ONLY`) | -- | **added, sparse** |
| `ClaimIndex` (PK `PK` `S`, SK `claim_sort` `S`, **`Projection: ALL`**) | -- | **added, sparse, optional** |
| `RecordIDIndex` (PK `record_id` `S`, `KEYS_ONLY`) | present | unchanged |

Base-table KEY SCHEMA is unchanged: `PK` (hash, `S`), `SK` (range, `S`). The SK
VALUE encoding changed: it is now `OUTBOX#<esc(envelope_id)>#<esc(binding_id)>`,
where each component percent-escapes `#` and `%` (`acl_marshal.go:26-111`). A
component with no `#` or `%` encodes byte-for-byte identically to the old raw
`OUTBOX#<envelope_id>#<binding_id>` form, so a rolling deploy keeps existing keys
stable; only previously-ambiguous `#`-bearing IDs change shape. Both schemes keep
the `OUTBOX#` prefix, so the Claim scan fallback drains old and new rows alike.
See [Cross-scheme SK migration](#cross-scheme-sk-migration) below.

### Why

- `StatusIndex` was a **table-wide hot partition**: every record carried one of
  a few `status` values, and each state transition rewrote its index entry.
- `ClaimedByIndex` was created but never queried.
- `ExpiryIndex` is **sparse**: the `has_expiry` attribute (value `"1"`) is
  written only for records that carry a non-zero `expires_at`, and it is removed
  on any terminal transition. Expire sweeps therefore scan exactly the
  expiry-eligible candidate set instead of the whole table
  (`acl_store.go:1354-1420`, `acl_marshal.go:180`).
- `ClaimIndex` is the **per-partition age-ordered claim path** (hash `PK`, range
  `claim_sort` -- a zero-padded `created_at`-millis + `seq` composite,
  `acl_marshal.go:117-160`). A Claim query reads it oldest-first and STOPS after
  `limit`, so a healthy backlog drains in O(limit) Query cost. Without it, Claim
  pages the whole partition to find the oldest-N, which goes O(backlog) after an
  outage. It is **sparse** -- only pending/claimed records carry `claim_sort`
  (stamped at Persist, removed at Complete/Expire, `acl_store.go:477,1238,1405`)
  -- and it projects **ALL** so a claim query returns the full item without a
  base-table read.
- The SK value encoding was made **injective** so distinct `(envelope, binding)`
  pairs can never collide onto one key. Raw concatenation was not injective: a
  binding ID containing `#` could alias a different record's key, and the second
  record was then acked and silently dropped as a false duplicate.

### Impact if not migrated

Persist and Complete keep working on any table shape -- they use the base table
and `RecordIDIndex`, neither of which this migration touches. Two operations
degrade or break:

- **Expire** queries `ExpiryIndex`. If that index is absent the sweep fails or
  finds nothing, so records with a set expiry are not swept to the DLQ on time.
- **Claim** uses `ClaimIndex` when it is present and correct; when it is absent
  Claim degrades to an exhaustive whole-partition scan (`acl_store.go:712-724`,
  classification `acl_errors.go:112-156`). The scan is always correct but
  O(backlog) per batch, so recovery after an outage self-throttles the table and
  failover catch-up is slow. Absence is deliberately tolerated -- an un-migrated
  table is NOT rejected. A present-but-misprojected `ClaimIndex` is a different
  case; see below.

### `ClaimIndex` must be `Projection: ALL`

Startup preflight (`acl_preflight.go:78-95`, projection check `:159-168`) treats
`ClaimIndex` as optional but, when it exists, enforces both its key schema AND
`Projection: ALL`. The claim query filters on the non-key `status` attribute, so a
`KEYS_ONLY` or `INCLUDE`-projected `ClaimIndex` would fail every Claim at runtime.
Preflight rejects that mismatch at boot (fatal `ErrInvalidConfig`) rather than
letting it wedge delivery later. A runtime scan-degrade (`acl_errors.go:124-153`,
latched in `acl_store.go:1030-1038`) is the belt-and-suspenders for a skipped or
fail-open preflight: a `does not project` error latches the scan fallback with one
WARN instead of mis-classifying it as a permanent fault. When you provision
`ClaimIndex` by hand or via IaC, set `ProjectionType: ALL`.

## Preferred path: drain, then recreate

Recreating is the simplest and safest option **once the outbox is empty**.

1. **Stop writers.** Scale the bridge fleet to zero, or route the affected
   routes to `direct_hold` so nothing new is persisted to this outbox.
2. **Drain to empty.** Let the drainers deliver all pending records. Confirm
   with a `Scan` count (or the `OutboxDepth` gauge) reaching 0.
3. **Delete the table**, then let the bridge recreate it: `Store.CreateTable`
   provisions the full current schema -- base table plus all three GSIs
   (`ExpiryIndex`, `RecordIDIndex`, `ClaimIndex`) -- in a single call, and is
   idempotent (`acl_store.go:326-395`). Or apply the new schema via your IaC.
4. **Resume writers.**

Because the table is empty at step 3, no messages are lost, and every record the
new code writes carries `claim_sort`, so `ClaimIndex` is populated from the start.

## In-place path: `UpdateTable` (no drain)

Use this only when you cannot drain (large backlog, strict availability).
DynamoDB permits **one GSI create or delete per `UpdateTable` call**, and a GSI
create runs an async backfill -- sequence the calls and wait for each to reach
`ACTIVE`.

> **Order matters for `ClaimIndex`.** Records written by the old build have no
> `claim_sort` attribute, so they are INVISIBLE to a `ClaimIndex` query. If you
> create `ClaimIndex` while an old-build backlog is still pending, the new code
> switches to the age-ordered fast path and **strands those old records** -- they
> are never claimed and never delivered. Create `ClaimIndex` LAST, only after the
> pre-upgrade backlog has drained (or you have backfilled `claim_sort` onto it).
> Until then, the scan fallback claims old and new rows alike, so leaving
> `ClaimIndex` absent is the safe state (`doc.go:113-122`).

1. **Delete `StatusIndex`** (`UpdateTable` `GlobalSecondaryIndexUpdates` ->
   `Delete`). Wait until it is gone.
2. **Delete `ClaimedByIndex`**. Wait.
3. **Create `ExpiryIndex`** with the new attribute definitions:
   - `has_expiry` `S` (hash), `expires_at` `N` (range), projection `KEYS_ONLY`,
     billing `PAY_PER_REQUEST` (match the table).
   Wait until the index reports `ACTIVE` (backfill complete).
4. **Deploy the new code now, with no `ClaimIndex` yet.** Claim uses the scan
   fallback and drains old raw-key rows and new escaped rows together via
   `begins_with(SK, "OUTBOX#")`. Let the pre-upgrade backlog drain to empty (or
   backfill `claim_sort`, see below).
5. **Create `ClaimIndex`** once the old backlog is gone:
   - `PK` `S` (hash), `claim_sort` `S` (range), projection **`ALL`** (not
     `KEYS_ONLY` -- the claim query filters on the non-key `status` attribute, so
     a narrower projection is rejected at startup and fails every Claim).
   Wait until it reports `ACTIVE`.

`RecordIDIndex` is left untouched. Creating `ClaimIndex` is optional for
correctness -- Claim scan-degrades without it -- but recommended: it removes the
O(backlog) claim scan that slows outage recovery.

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

`ClaimIndex` is sparse the same way: it indexes only items carrying `claim_sort`,
and old-build records do not have it. This is the stranding hazard called out in
the in-place steps above -- an old record missing `claim_sort` is INVISIBLE to a
`ClaimIndex` query, so once the code claims via the index it is never delivered.
The scan fallback has no such blind spot. Prefer draining the old backlog before
creating `ClaimIndex`. If you must provision it while old records remain, backfill
first: `Scan` for pending/claimed items with no `claim_sort` and `UpdateItem` the
encoded value (zero-padded `created_at`-millis, then `seq`; see `claimSortKey`,
`acl_marshal.go:117-160`).

## Cross-scheme SK migration

Upgrading from a build that wrote raw-concatenation sort keys
(`OUTBOX#<envelope_id>#<binding_id>`, no per-component escaping) to the current
injective encoding needs no key rewrite for the common case, and never drops a
message. The two schemes share the `OUTBOX#` prefix on purpose, so the Claim scan
fallback's `begins_with(SK, "OUTBOX#")` finds and drains both old raw rows and new
escaped rows during the transition (`acl_marshal.go:26-111`, `doc.go:95-122`).

Old and new keys diverge only for a component containing `#` or `%`. For those,
a new escaped SK can equal an old raw SK only in a narrow case -- e.g. new
`sortKey("a#b","c")` equals old raw `OUTBOX#a%23b#c` written for envelope `a%23b`.
When a new distinct record's `attribute_not_exists(SK)` put hits an old raw row on
such a key, Persist does NOT blind-count it a duplicate (which would ack and drop
the distinct message). It reads the occupying row strongly-consistently and, on an
envelope/binding MISMATCH, returns a TRANSIENT error so the record is retried
(`acl_store.go:484-560`). It lands once the legacy row is claimed, completed and
TTL-compacted -- self-healing, no silent drop.

No operator action is required for the SK migration itself. It matters here only
because it is the reason the in-place path drains the old backlog before adding
`ClaimIndex`: the same old raw rows that lack escaping also lack `claim_sort`.

## Rollback

The base table (persist / claim / complete) keeps working throughout either
path -- those operations use only the base keys and `RecordIDIndex`, neither of
which this migration touches. Safe abort points:

**Preferred (drain) path.**

- Before step 3 (table delete) nothing has changed: abort freely and resume
  writers.
- After step 3 the table is gone; the only "rollback" is letting the bridge
  recreate it (idempotent) or restoring from a backup. Take an on-demand backup
  or enable PITR **before** deleting if the outbox contents matter.

**In-place (`UpdateTable`) path.** Each GSI edit is individually reversible, but
mind the ordering:

- A half-created `ExpiryIndex` or `ClaimIndex` can be deleted at any point; the
  new binary recreates it on the next `CreateTable`, and Claim scan-degrades while
  `ClaimIndex` is absent.
- Once you have deleted `StatusIndex` / `ClaimedByIndex` you have removed the
  indexes the **old** binary queried. Reverting the binary to a build that reads
  `StatusIndex` therefore requires **recreating `StatusIndex` first** (former key
  schema: PK `status`, SK `created_at`, projection matching the old build) and
  waiting for its backfill to reach `ACTIVE` before starting the old binary.
- Do not run the old and new binaries against the same table concurrently: the
  new binary maintains the `has_expiry` and `claim_sort` attributes the old one
  ignores, and the old binary's Expire sweep fails outright when `ExpiryIndex` is
  absent.

## Verification

- `DescribeTable` shows three GSIs: `ExpiryIndex` and `RecordIDIndex` (both
  `KEYS_ONLY`) and `ClaimIndex` (**`Projection: ALL`**), all `ACTIVE`. A
  fully-migrated table has `ClaimIndex`; a table intentionally left on the scan
  fallback has only the first two, which is valid but slower to claim.
- Persist a test message with a short TTL and confirm the Expire sweep moves it
  to the DLQ after expiry.
- Confirm no old-build backlog remains before `ClaimIndex` went live (or that you
  backfilled `claim_sort`): a flat `DynamoDBOutboxClaimScanPages` counter
  (`MetricClaimScanPages`, `metrics.go:22`) and a drained `OutboxDepth` gauge both
  indicate the fast path is claiming everything.
