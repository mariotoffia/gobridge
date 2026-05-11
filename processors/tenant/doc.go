// Package tenant implements the multi-tenant header processor that
// stamps or validates a tenant identifier on each envelope flowing
// through a route.
//
// Responsibility:
//   - extract the tenant identifier from the envelope (header, subject
//     prefix, or resolver-provided attribute) and normalise it into the
//     canonical x-bridge tenant header
//   - reject envelopes that violate the configured tenancy policy with
//     a typed BridgeError so the runtime classifies the failure
//     consistently
//
// Key types:
//   - Processor: the ports.Processor implementation
//   - Config: tenancy source, required / optional flag, and validation
//     rules
//
// Dependencies: ports (Processor), domain/messaging (Envelope and
// Headers), and domain/shared (BridgeError). No transport or storage
// dependencies.
package tenant
