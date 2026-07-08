// Package awsstore provides a DynamoDB-backed ports.StoreFactory that
// aggregates the individual dynamodblease, dynamodboutbox, and dynamodbdlq
// adapters behind a single factory interface.
//
// # Build-time schema preflight
//
// When constructed with a real DynamoDB client, each store build runs a
// DescribeTable preflight that verifies the target table's key schema (and the
// GSIs required by the role) against the authoritative schema documented in the
// respective adapter package. On a mismatch the build FAILS with
// shared.ErrInvalidConfig, naming the table, the expected schema, and the actual
// schema. This runs in the production build path, not just test/dev.
//
// The preflight closes a silent-loss hazard: outbox, DLQ, and lease config
// shapes are identical, so a copy-pasted table name (e.g. an outbox pointed at a
// PK-only lease table) would otherwise accept the first record per partition and
// silently ack-and-drop every subsequent one as a duplicate. A table that does
// not yet exist is tolerated (ResourceNotFoundException is non-fatal), so a
// build-then-provision flow still works; the missing table then fails loudly at
// first use. The lease build additionally emits a loud WARN if DynamoDB TTL is
// enabled on the lease table, since TTL-reaping a lease row resets its fencing
// counter (see dynamodblease).
package awsstore
