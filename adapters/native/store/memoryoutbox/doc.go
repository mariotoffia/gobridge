// Package memoryoutbox implements ports.OutboxStore as an in-memory store.
//
// This adapter is suitable only for unit tests where outbox durability
// is not under test. It is not safe for production deployments.
package memoryoutbox
