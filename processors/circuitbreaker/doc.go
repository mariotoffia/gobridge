// Package circuitbreaker (processors/circuitbreaker) implements the
// processor-chain element that wraps downstream processors with a
// circuit breaker so a flapping downstream cannot starve the runtime.
//
// Responsibility:
//   - intercept envelopes flowing through a route's processor chain and
//     gate them on a per-route or per-binding breaker state machine
//   - emit short-circuit errors (typed BridgeError) when the breaker is
//     open so the runtime can route the envelope to DLQ without invoking
//     the downstream processor
//
// Key types:
//   - Processor: the ports.Processor implementation wrapping a wrapped
//     processor with breaker semantics
//   - Config: thresholds, cooldown, and identity (per-route key)
//
// Dependencies: ports (Processor), domain/messaging (Envelope),
// domain/shared (BridgeError), and the root circuitbreaker package for
// the state-machine implementation.
package circuitbreaker
