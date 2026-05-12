// Package runtime implements the GoBridge route execution engine.
//
// It orchestrates message flow through routes using the port interfaces
// defined in ports/ and the value types from domain/. The runtime has
// no transport or storage dependencies -- those are injected via port
// interfaces at construction time.
//
// Key components:
//   - Runtime: top-level coordinator that manages routes and sessions
//   - RouteRunner: per-route execution pipeline supporting DirectHold and SharedOutbox delivery
//   - Manager (in runtime/session): session lifecycle, lease renewal, and three-phase step-down
//   - Drainer (in runtime/outbox): outbox replay loop with fencing, poison detection, and expiry checks
//   - Router (in runtime/dlq): error classification and dead-letter queue routing
package runtime
