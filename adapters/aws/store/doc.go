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
// first use.
//
// The preflight is FAIL-CLOSED. A confirmed schema mismatch is fatal, and so is
// an inability to VERIFY the schema: when DescribeTable itself fails — a
// control-plane throttle, a least-privilege role lacking dynamodb:DescribeTable
// (AccessDenied), or an emulator that does not implement DescribeTable — the
// build FAILS rather than proceeding as if the table were valid, because an
// unreadable table gives no evidence that it is well-shaped. Grant
// dynamodb:DescribeTable so the check can enforce the schema. For a dev/emulator
// that genuinely cannot answer DescribeTable, construct the factory with
// WithSchemaPreflightAdvisory to downgrade an unverifiable table to a loud WARN;
// this is an explicit opt-out and must not be used in production.
//
// The lease build additionally emits a loud WARN if DynamoDB TTL is
// enabled on the lease table, since TTL-reaping a lease row resets its fencing
// counter (see dynamodblease).
package awsstore
