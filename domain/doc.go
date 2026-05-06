// Package domain defines the core value types for the GoBridge architecture.
//
// This package contains pure domain types with no interface definitions and
// no imports from other bridge packages. It sits at the innermost ring of the
// Hexagonal Architecture: bridge/ports imports domain, adapters import ports,
// and nothing imports inward from domain.
//
// The domain layer is being decomposed into bounded-context sub-packages
// (FIX-004). Types that previously lived directly in this package have been
// moved as follows:
//   - Envelope, headers, TraceContext      -> domain/messaging
//   - BridgeError, ErrorClass, ErrorCode,
//     Tag, metric constants                -> domain/shared
//   - OutboxRecord, OutboxStatus,
//     OutboxPartitionKey, LeaseToken,
//     LeaseInfo, PeerInfo, DrainStrategy,
//     FixedPoll, AdaptiveBackoff           -> domain/persistence
//
// The remaining transitional residents of this package are RoutePolicy,
// DestinationBinding, DispatchPlan, Credentials, and a few related helpers,
// which will be relocated in subsequent FIX-004 phases.
package domain
