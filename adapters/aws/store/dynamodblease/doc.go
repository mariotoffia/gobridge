// Package dynamodblease implements the ports.LeaseStore interface using
// Amazon DynamoDB. It uses conditional writes with fencing tokens to
// enforce single-active lease ownership semantics.
//
// Table: gobridge-leases (configurable)
// Key: PK = "LEASE#<lease_id>"
//
// See ARCHITECTURE_NEW-STORES.md for table schema and operational guidance.
//
// # Authoritative key schema (verified by factory preflight)
//
// This is the single source of truth for the lease table shape. The AWS store
// factory runs a DescribeTable preflight at build time and FAILS the build with
// shared.ErrInvalidConfig if the target table does not match — naming the table,
// the expected schema, and the actual schema. EnsureTable/CreateTable provision
// exactly this shape.
//
//	Primary key : PK (S, HASH)   — no range key, no GSIs
//
// # TTL invariant: DynamoDB TTL MUST be DISABLED on the lease table
//
// Lease rows ARE the fencing counter of record. If DynamoDB TTL is enabled on
// the lease table a reaper can delete a lease row, and the next acquire recreates
// it at version 1 while the outbox high-water mark still sits at v >> 1 — every
// subsequent claim then fails with ErrStaleFencingToken and the partition stalls.
// The factory preflight makes a best-effort DescribeTimeToLive call at build time
// and emits a loud WARN (never fatal — TTL is legitimate on other tables, just
// dangerous here) if TTL is enabled on the lease table. If that call itself fails
// (missing permission, a throttle, or an emulator that does not support it) the
// check is silently skipped. Provisioning must leave TTL disabled.
//
// # IAM requirements (upgrade note)
//
// The build-time preflight calls dynamodb:DescribeTable AND, best-effort,
// dynamodb:DescribeTimeToLive as of the schema-preflight / TTL-invariant change.
// DescribeTable is recommended: if the role cannot DescribeTable (AccessDenied) or
// the call fails transiently, the factory does NOT brick boot — it logs a loud
// WARN and proceeds fail-open; a confirmed schema mismatch, however, is always
// fatal. DescribeTimeToLive is optional: its result only drives the TTL-enabled
// warning, and a failed call is swallowed silently, so without the permission the
// warning simply never fires.
//
// A present-but-unparseable fencing version is surfaced as a read error rather
// than silently coerced to 0 (which would also reset the fence), so fence
// corruption fails loudly instead of masquerading as a fresh lease.
package dynamodblease
