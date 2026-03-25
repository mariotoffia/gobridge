// Package domain defines the core value types for the GoBridge architecture.
//
// This package contains pure domain types with no interface definitions and
// no imports from other bridge packages. It sits at the innermost ring of the
// Hexagonal Architecture: bridge/ports imports domain, adapters import ports,
// and nothing imports inward from domain.
//
// Key types:
//   - Envelope: the normalized message being moved through the bridge
//   - LeaseToken / LeaseInfo: fencing tokens for cluster lease ownership
//   - RoutePolicy: per-route delivery, retry, and backpressure configuration
//   - DestinationBinding / DispatchPlan: egress resolution model
//   - OutboxRecord / OutboxStatus: durable outbox state machine
//   - DLQEntry: dead-letter queue record
//   - Header constants: well-known x-bridge.* header keys
//   - Error classification: Transient, Permanent, Expired, Rejected
package domain
