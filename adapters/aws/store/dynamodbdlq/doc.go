// Package dynamodbdlq implements the ports.DLQStore interface using
// Amazon DynamoDB. It uses conditional writes for idempotent entry
// creation and GSI-backed queries for efficient listing by route
// and category.
//
// Table: gobridge-dlq (configurable)
// Key: PK = "DLQ#<entry_id>" (no sort key)
//
// GSIs:
//   - RouteIndex: PK=route_id, SK=failed_at (for List by route)
//   - CategoryIndex: PK=category, SK=failed_at (for List by category)
//
// Operational guidance: docs/processors-and-stores.md, plus
// docs/runbooks/poison-message-dlq-growth.md for DLQ growth incidents.
//
// # Authoritative key schema (verified by factory preflight)
//
// This is the single source of truth for the DLQ table shape. The AWS store
// factory runs a DescribeTable preflight at build time and FAILS the build with
// shared.ErrInvalidConfig if the target table does not match — naming the table,
// the expected schema, and the actual schema. EnsureTable/CreateTable provision
// exactly this shape.
//
//	Primary key : PK (S, HASH)
//	GSI RouteIndex    : route_id (S, HASH), failed_at (N, RANGE)
//	GSI CategoryIndex : category (S, HASH), failed_at (N, RANGE)
//
// route_id and category are sparse GSI partition keys: they are written only
// when non-empty (DynamoDB rejects empty index key attributes), so a metadata-
// only entry simply stays out of those indexes. A missing GSI, a wrong key
// attribute/type, or an unexpected range key fails the preflight; extra
// (unexpected) GSIs are tolerated.
//
// # IAM requirements (upgrade note)
//
// The build-time preflight calls dynamodb:DescribeTable, a NEW required action
// as of the schema-preflight change. Grant it to the store's role. If the role
// cannot DescribeTable (AccessDenied) or the call fails transiently, the factory
// does NOT brick boot: it logs a loud WARN and proceeds fail-open. A confirmed
// schema mismatch, however, is always fatal.
//
// # Ordering and unbounded operations
//
// List returns entries oldest-first by failed_at (ports.DLQReader contract).
// For unfiltered listing the store pages the Scan to exhaustion (bounded by
// WithMaxScanPages) and selects the globally oldest before truncating to the
// requested limit — DynamoDB Scan order is hash-based and uncorrelated with
// failed_at, so selecting the first page's items would not be globally oldest.
// DeleteByFilter with Limit <= 0 means "delete every match": an index-less
// unbounded delete scans to true exhaustion (ignoring WithMaxScanPages, since
// the caller explicitly asked for all), so no match survives behind a run of
// non-matching entries.
//
// # Metadata-only entries (empty envelope)
//
// A DLQ entry may carry only failure metadata and no envelope (e.g. a message
// that could not be decoded). Write stores the empty sentinel "" for such an
// entry rather than a zero-envelope JSON, so read-back does not trip the
// mandatory-ID guard in Envelope.UnmarshalJSON. Entries that carry a real
// envelope are marshalled as usual.
package dynamodbdlq
