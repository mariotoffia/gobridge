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
//     message counts. When that tracker also implements
//     ports.TenantUsageReader and the tenant's MaxInFlight > 0, the
//     processor enforces a per-tenant in-flight ceiling: an over-quota
//     delivery is rejected transiently (retry-policy driven, so it
//     redelivers once the tenant's in-flight drains) and, if the usage
//     read errors, enforcement fails open (the delivery proceeds). The
//     transient reject re-enters the route retry pipeline, so a tenant that
//     stays over-ceiling beyond MaxReplayAttempts backoff cycles has its
//     messages DLQ'd (see docs/processors-and-stores.md). The
//     check-then-increment is not atomic, so overshoot is bounded by the
//     tenant's total concurrent in-flight admissions across all routes and
//     instances sharing the usage store. Message-count tracking stays
//     observational; windowed message-count quotas remain out of scope
//     (Phase 2).
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
