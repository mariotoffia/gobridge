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
// See ARCHITECTURE_NEW-STORES.md for table schema and operational guidance.
package dynamodbdlq
