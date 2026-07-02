// Package circuitbreaker (processors/circuitbreaker) implements a
// processor-chain element that wraps the downstream processors placed
// after it in a route's chain with a circuit breaker, so a flapping
// downstream processor dependency (for example a custom enrichment
// processor that calls an external service) cannot starve the runtime.
//
// Scope boundary (important): the breaker observes only the processors
// that run after it in the chain -- its next(ctx, env). It does NOT and
// cannot observe the sender: the runtime dispatches to the sender AFTER
// the processor chain returns, outside the breaker's next (see
// runtime/route/dispatch.go). Sender and broker failures are handled
// independently by the runtime's own retry, backoff, and DLQ policy.
// Place a breaker before a processor that performs remote I/O; do not
// expect it to trip on send-side failures.
//
// Responsibility:
//   - intercept envelopes flowing through a route's processor chain and
//     gate them on a per-key breaker state machine (the key is derived by
//     the configured KeyExtractor)
//   - when the breaker is open, short-circuit with shared.ErrUnavailable
//     (a transient BridgeError carrying a RetryAfter hint) WITHOUT
//     invoking the downstream processors, so the runtime applies its
//     retry/backoff policy (source retry) rather than hammering the
//     failing dependency
//
// Key types:
//   - Processor: the ports.Processor implementation that gates the rest
//     of the chain with breaker semantics
//   - Config: thresholds, cooldown, and identity (per-key)
//
// Dependencies: ports (Processor), domain/messaging (Envelope),
// domain/shared (BridgeError), and the root circuitbreaker package for
// the state-machine implementation.
package circuitbreaker
