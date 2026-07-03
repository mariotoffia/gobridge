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
package dynamodboutbox
