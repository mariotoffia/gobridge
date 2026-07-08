// Package dynamodboutbox implements the ports.OutboxStore interface using
// Amazon DynamoDB. It provides partitioned persistence, atomic fan-out
// writes via TransactWriteItems, claim/reclaim with fencing tokens,
// completion, expiry sweep, and TTL-based compaction.
//
// Table: gobridge-outbox (configurable via WithTableName)
// Key: PK = "<partition_key>", SK = "OUTBOX#<envelope_id>#<binding_id>"
//
// The SK design ensures idempotent persist via attribute_not_exists(SK)
// and supports fan-out (same envelope to different bindings).
//
// GSIs:
//   - ExpiryIndex: PK=has_expiry (sparse), SK=expires_at (for expire sweeps;
//     only expiry-carrying records enter the index)
//   - RecordIDIndex: PK=record_id (for Complete record lookup)
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
//
// A missing GSI, a wrong key attribute/type, or an unexpected range key fails
// the preflight. Extra (unexpected) GSIs are tolerated.
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
// records per partition by (CreatedAt, Seq). Because the SK is keyed on
// envelope/binding ID — lexicographically uncorrelated with age — the claim
// query pages the partition to EXHAUSTION and selects the oldest N across every
// page, rather than sorting a fixed candidate window. This guarantees that under
// a deep backlog (an egress outage on an exclusive session) an old record whose
// envelope ID sorts late is still claimed first, so per-partition ordered
// delivery cannot silently starve a record. No age-ordered secondary index is
// required for correctness (exhaustive paging is used instead); if one is added
// later it must be provisioned by EnsureTable/CreateTable and verified by the
// factory preflight above.
package dynamodboutbox
