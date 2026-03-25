// Package sqliteoutbox implements ports.OutboxStore using SQLite.
//
// This adapter provides file-based durable outbox persistence for
// integration tests and single-process deployments. It is not suitable
// for clustered production use.
package sqliteoutbox
