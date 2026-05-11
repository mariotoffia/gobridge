// Package circuitbreaker implements the in-process circuit breaker used
// by the runtime and adapters to short-circuit calls into unreliable
// dependencies (transports, credential stores, downstream services).
//
// Responsibility:
//   - track failure / success windows per protected operation and trip
//     a state machine (closed -> open -> half-open) when the configured
//     thresholds are exceeded
//   - expose the breaker as a port-conformant resilience primitive so
//     adapters can wrap their hot-path calls without coupling to a
//     concrete implementation
//
// Key types:
//   - Breaker: the state machine that gates Execute calls
//   - Config: thresholds, cooldown, and clock injection
//
// Dependencies: depends on ports for the resilience interface contract
// and on domain/clock for deterministic time in tests. No transport,
// storage, or runtime dependencies.
package circuitbreaker
