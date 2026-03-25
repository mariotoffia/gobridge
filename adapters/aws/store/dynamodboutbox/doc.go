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
//   - StatusIndex: PK=status, SK=created_at (for expire sweeps)
//   - ClaimedByIndex: PK=claimed_by, SK=claimed_at (for crash recovery)
//   - RecordIDIndex: PK=record_id (for Complete record lookup)
//
// See ARCHITECTURE_NEW-STORES.md for table schema, GSI design, and
// operational guidance.
package dynamodboutbox
