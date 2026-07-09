// Package dynamodboutbox implements the ports.OutboxStore interface using
// Amazon DynamoDB. It provides partitioned persistence, atomic fan-out
// writes via TransactWriteItems, claim/reclaim with fencing tokens,
// completion, expiry sweep, and TTL-based compaction.
//
// Table: gobridge-outbox (configurable via WithTableName)
// Key: PK = "<partition_key>", SK = "OUTBOX#<esc(envelope_id)>#<esc(binding_id)>"
//
// The SK design ensures idempotent persist via attribute_not_exists(SK)
// and supports fan-out (same envelope to different bindings). The envelope
// and binding components are individually escaped (# -> %23 after % -> %25)
// before joining with the '#' separator, so the composite SK is INJECTIVE:
// distinct (envelope_id, binding_id) pairs can never alias onto the same SK.
// Envelope IDs are producer-controlled, so a raw concatenation would let a
// crafted ID collide two distinct messages onto one SK and silently drop one
// as a false duplicate (finding c13-sk-collision).
//
// GSIs:
//   - ExpiryIndex: PK=has_expiry (sparse), SK=expires_at (for expire sweeps;
//     only expiry-carrying records enter the index)
//   - RecordIDIndex: PK=record_id (for Complete record lookup)
//   - ClaimIndex: PK=PK, SK=claim_sort (sparse, age-ordered; for Claim to
//     drain a partition oldest-first in O(limit) — see "Claim ordering")
//
// See docs/runbooks/dynamodb-outbox-gsi-migration.md for migrating
// tables created with the former StatusIndex/ClaimedByIndex layout.
//
// # Authoritative key schema (verified by factory preflight)
//
// This is the single source of truth for the outbox table shape. The AWS store
// factory runs a DescribeTable preflight at build time and FAILS the build with
// shared.ErrInvalidConfig if the target table does not match — naming the table,
// the expected schema, and the actual schema. A misprovisioned outbox (e.g. a
// PK-only lease table copy-pasted into the outbox config) would otherwise accept
// the first record per partition and classify every subsequent record a
// duplicate, silently acking and dropping it. EnsureTable/CreateTable provision
// exactly this shape.
//
//	Primary key : PK          (S, HASH)
//	              SK          (S, RANGE)
//	GSI ExpiryIndex   : has_expiry (S, HASH), expires_at (N, RANGE)   [sparse]
//	GSI RecordIDIndex : record_id  (S, HASH)
//	GSI ClaimIndex    : PK (S, HASH), claim_sort (S, RANGE), Projection: ALL [sparse, OPTIONAL]
//
// A missing GSI, a wrong key attribute/type, or an unexpected range key fails
// the preflight. Extra (unexpected) GSIs are tolerated. ClaimIndex is OPTIONAL:
// a table provisioned before it existed still passes preflight and Claim
// degrades to an exhaustive partition scan (see "Claim ordering"). When the
// ClaimIndex IS present it MUST be Projection: ALL — the claim query filters on
// the non-key status attribute, which a KEYS_ONLY or under-projected INCLUDE
// index does not carry, so DynamoDB would reject every claim query at runtime.
// Preflight therefore REJECTS a present-but-under-projected ClaimIndex at
// startup ("ClaimIndex must be Projection: ALL") rather than letting it wedge
// Claim fleet-wide. As a runtime backstop, a projection-mismatch query error
// also degrades to the scan fallback with one WARN (never a permanent reject).
//
// # IAM requirements (upgrade note)
//
// The build-time preflight calls dynamodb:DescribeTable, a NEW required action
// as of the schema-preflight change. Grant it to the store's role. If the role
// cannot DescribeTable (AccessDenied) or the call fails transiently, the factory
// does NOT brick boot: it logs a loud WARN and proceeds fail-open — a genuine
// key-schema mismatch is only guaranteed to be caught when the role can read the
// table description. A confirmed schema mismatch, however, is always fatal.
//
// # Claim ordering
//
// Claim honours the ports.OutboxStore contract of returning the oldest N pending
// records per partition by (CreatedAt, Seq).
//
// Fast path (ClaimIndex GSI): each pending/claimed record carries claim_sort, a
// zero-padded (created_at-millis, seq) composite stamped at Persist and removed
// at Complete/Expire, so the sparse ClaimIndex GSI (PK=PK, SK=claim_sort) holds
// exactly the not-yet-terminal working set in age order. Claim queries it
// oldest-first with Limit and STOPS once N records are claimed, so a healthy
// backlog drains in O(limit) Query cost regardless of how deep the partition
// is. Because the SK is keyed on envelope/binding ID — lexicographically
// uncorrelated with age — this age-ordered access path is what keeps a
// deep-backlog drain (an egress outage on an exclusive session) from going
// O(backlog) per batch and self-throttling DynamoDB (c13-claim-quadratic). The
// GSI is eventually consistent (a GSI cannot be read strongly consistent); the
// per-record claim transaction re-validates status and the fence, so a stale
// index entry only ever costs a skipped candidate, never a double claim.
//
// Fallback path (no ClaimIndex): on a table provisioned before ClaimIndex
// existed — or one whose ClaimIndex cannot serve the age-ordered claim query
// (missing OR not Projection: ALL, detected at runtime) — the Query fails;
// Claim latches that (one WARN naming the reason) and falls back to paging the
// partition to EXHAUSTION, selecting the oldest N across every page. This is
// always correct but O(backlog) — a deep backlog on the fallback path emits a
// throttled WARN and a MetricClaimScanPages counter so the quadratic cost is
// observable. Provision the ClaimIndex GSI as Projection: ALL (EnsureTable/
// CreateTable do so) to regain the O(limit) fast path.
//
// # SK escaping migration (upgrading from raw-concat keys)
//
// Pre-upgrade rows used a RAW concatenation SK ("OUTBOX#<envelope_id>#<binding_id>"
// with no per-component escaping). New rows escape each component. The two
// schemes share the "OUTBOX#" prefix on purpose so the scan fallback's
// begins_with(SK, "OUTBOX#") still finds and drains BOTH old raw rows and new
// escaped rows during migration — old pending rows are never orphaned.
//
// Residual (self-healing, narrow): a new escaped SK can equal an old raw SK
// only when a producer/env id LITERALLY contains "%23"/"%25" — e.g. new
// sortKey("a#b","c") == old raw "OUTBOX#a%23b#c" written for env "a%23b". If an
// old raw row still occupies that key, the new distinct record's
// attribute_not_exists(SK) put fails. Persist does NOT blind-count that a
// duplicate (which would ack and DROP the distinct message); it reads the
// occupying row strongly-consistently and, on an envelope/binding MISMATCH,
// returns a TRANSIENT error so the record is retried — it lands once the legacy
// row is claimed, completed and TTL-compacted. No silent drop occurs.
//
// Migration steps:
//  1. Deploy the new code with NO ClaimIndex provisioned yet. Claim uses the
//     scan fallback and drains old raw rows (no claim_sort) and new escaped
//     rows alike via begins_with(SK, "OUTBOX#").
//  2. Let the pre-upgrade backlog drain (old rows lack claim_sort, so they are
//     invisible to a ClaimIndex query — provisioning it first would strand
//     them on the fast path). Backfill claim_sort onto surviving old rows if
//     you must provision earlier.
//  3. Provision the ClaimIndex GSI as Projection: ALL once the old backlog is
//     drained (or backfilled) to regain the O(limit) fast path.
package dynamodboutbox
