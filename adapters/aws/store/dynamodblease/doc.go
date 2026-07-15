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
// # Persisted row compatibility and migration
//
// Every existing lease item must carry the complete base tuple: exact PK,
// string owner, positive incrementable uint64 version, positive renewed_at epoch milliseconds,
// and non-negative expires_at epoch milliseconds. An active row has a non-empty
// owner and expires_at > renewed_at. An explicitly released row has an empty
// owner and expires_at == 0; renewed_at and the positive fencing version remain.
// Any other shape is corrupt and every read/acquire fails closed with
// shared.ErrInvalidConfig. The store never treats missing fields as release and
// never recreates an existing fencing counter at version 1.
//
// The only supported legacy omission is the complete set of takeover-observation
// attributes. If fingerprint, elapsed duration, and generation are all absent,
// observation starts at zero. Partial, negative, or overflowing evidence is
// corrupt. Deployments upgrading from rows that predate renewed_at must migrate
// those rows offline before starting this version: quiesce all lease users,
// preserve each positive version, write a valid renewed_at/expires_at pair (or
// the explicit released shape), verify the rows, then restart. Automatic healing
// is intentionally forbidden because it cannot distinguish a dead owner from a
// live foreign writer safely.
//
// # TTL invariant: DynamoDB TTL MUST be DISABLED on the lease table
//
// Lease rows ARE the fencing counter of record. If DynamoDB TTL is enabled on
// the lease table a reaper can delete a lease row, and the next acquire recreates
// it at version 1 while the outbox high-water mark still sits at v >> 1 — every
// subsequent claim then fails with ErrStaleFencingToken and the partition stalls
// (a split-brain window). The build-time Preflight therefore ENFORCES this
// invariant (finding c13-lease-ttl-warn): it calls dynamodb:DescribeTimeToLive
// and, if TTL is ENABLED/ENABLING on the lease table, FAILS the build with
// shared.ErrInvalidConfig naming the table and status. A DescribeTimeToLive that
// itself fails (missing permission, throttle, or an emulator gap) proves nothing
// about the TTL state and is surfaced FAIL-CLOSED as shared.ErrInvalidConfig (the
// classified transport cause is wrapped for diagnostics). Returning the
// factory's always-fatal marker is deliberate: it guarantees the schema-level
// WithSchemaPreflightAdvisory cannot silently relax this TTL check — only the
// TTL-specific WithTTLPreflightAdvisory option downgrades both the enabled-TTL
// and the unverifiable-TTL cases to a loud WARN. Provisioning must leave TTL
// disabled.
//
// # IAM requirements (upgrade note)
//
// The build-time preflight calls dynamodb:DescribeTable AND
// dynamodb:DescribeTimeToLive as of the schema-preflight / TTL-invariant change.
// DescribeTable is recommended: if the role cannot DescribeTable (AccessDenied) or
// the call fails transiently, the factory does NOT brick boot — it logs a loud
// WARN and proceeds fail-open; a confirmed schema mismatch, however, is always
// fatal. DescribeTimeToLive is REQUIRED by default: the lease store's Preflight
// enforces the TTL-DISABLED invariant fail-closed, so a missing
// dynamodb:DescribeTimeToLive permission (or an emulator that cannot answer it)
// blocks boot unless WithTTLPreflightAdvisory is set as an explicit dev/emulator
// opt-out.
//
// # BREAKING upgrade / migration (dynamodb:DescribeTimeToLive now required)
//
// This is a fail-closed escalation: dynamodb:DescribeTimeToLive changes from
// best-effort (previously swallowed) to a REQUIRED lease-role permission. An
// existing lease role that lacks it will FAIL BOOT after upgrade (AccessDenied →
// shared.ErrInvalidConfig). Before rolling out this version:
//
//  1. Grant dynamodb:DescribeTimeToLive on the lease table to the lease role
//     (alongside the existing dynamodb:DescribeTable), THEN deploy; or
//  2. As a temporary, dev-only bridge, downgrade the check to a WARN with the
//     WithTTLPreflightAdvisory() opt-out — available BOTH on this store
//     (dynamodblease.WithTTLPreflightAdvisory) and, for factory / config-driven
//     deployments, on the AWS store factory (awsstore.WithTTLPreflightAdvisory,
//     at parity with the schema advisory). It is a COMPILE-TIME option (not a
//     runtime config flag) and is NOT a production posture, since an unverifiable
//     TTL state on the fencing table is a real split-brain hazard.
//
// This package ships no IAM/IaC of its own; grant the permission in whatever
// provisioning (CDK/Terraform/console) manages the lease role.
//
// A present-but-unparseable fencing version is surfaced as a read error rather
// than silently coerced to 0 (which would also reset the fence), so fence
// corruption fails loudly instead of masquerading as a fresh lease.
package dynamodblease
