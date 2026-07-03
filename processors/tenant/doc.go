// Package tenant implements the multi-tenant processor that resolves a
// tenant identifier from an envelope header, validates it against an
// optional TenantValidator, and tracks per-tenant usage through an
// optional TenantUsageTracker.
//
// Responsibility:
//   - read the tenant identifier from the configured app-visible header
//     (default DefaultTenantHeader, "x-tenant-id"); the header value is
//     used verbatim — no normalisation and no rewriting into reserved
//     x-bridge.* headers is performed (those are stripped at ingress)
//   - when RequireTenant is set, reject envelopes without a tenant ID
//   - when a validator is configured, reject inactive tenants and
//     payloads exceeding the tenant's MaxMessageSizeBytes, each with a
//     typed BridgeError so the runtime classifies the failure
//     consistently (rejected, not retried)
//   - when a usage tracker is configured, record in-flight and processed
//     message counts. Tracking is observational only: the tracker port is
//     increment-only, so no message-count quota ceiling is enforced here
//     (the only enforced per-tenant limit is MaxMessageSizeBytes above)
//
// Key types:
//   - Processor: the ports.Processor implementation
//   - Config: tenant source header, RequireTenant flag, and the
//     in-flight decrement timeout used for cleanup after cancellation
//
// Dependencies: ports (Processor, TenantValidator, TenantUsageTracker,
// MetricsExporter), domain/messaging (Envelope and Headers), and
// domain/shared (BridgeError). No transport or storage dependencies.
package tenant
