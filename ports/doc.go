// Package ports defines the port interfaces for the GoBridge architecture.
//
// This package contains interface definitions that adapter modules implement,
// along with small port-associated types (events, health, specs, function
// types) that are part of the port contracts. It imports bridge/domain for
// type references but defines no concrete implementations.
//
// In Hexagonal Architecture terms, these are the driven (secondary) ports:
//   - LeaseStore: cluster lease persistence
//   - OutboxStore: durable outbox persistence
//   - DLQStore: dead-letter queue persistence
//   - Receiver: message ingress from a transport
//   - Sender / BatchSender: message egress to a transport
//   - Session: stateful transport connection lifecycle
//   - Lease: cluster ownership for single-active scenarios
//   - DestinationResolver: runtime egress binding selection
//   - Processor: message transformation chain element
//   - CredentialStore: URI-based credential resolution
//   - CredentialRepository: per-backend credential adapter
//   - CredentialAdmin: credential lifecycle management (CRUD)
//
// Configuration loader/watcher contracts live in the config package
// (config.Loader, config.Watcher, config.Reloader) because their
// signatures intrinsically depend on *config.BridgeConfig. Keeping
// them in config preserves the rule that ports imports only domain.
//
// Adapter modules under adapters/ implement these interfaces.
// The bridge/runtime package (future) depends on these interfaces.
package ports
