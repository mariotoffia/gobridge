// Package sqlitedlq implements ports.DLQStore using SQLite.
//
// This adapter provides file-based durable dead-letter queue persistence
// for integration tests and single-process deployments. It is not suitable
// for clustered production use.
package sqlitedlq
