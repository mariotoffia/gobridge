// Package ports — circuit breaker port.
//
// CircuitBreaker is the per-call gate adapters compose around outbound
// calls to a downstream. The port keeps adapters ignorant of the
// concrete state machine and timing primitives that implement the
// breaker; only the call-site contract is exposed.
//
// The two-call shape (BeforeRequest / AfterRequest) is intentional: an
// open-circuit rejection returns a *shared.BridgeError that carries a
// RetryAfter hint, which a bool-returning Allow() variant would drop
// on the floor. Adapters surface that hint to the route runner so
// retries respect the breaker's cool-down window.
package ports

// CircuitBreaker is a per-call gate that opens after enough failures
// to protect a downstream from continued pressure. Adapters compose
// a circuit breaker around outbound calls without knowing the
// implementation.
type CircuitBreaker interface {
	// BeforeRequest returns nil when the request may proceed, or an
	// error (typically shared.ErrUnavailable with a RetryAfter hint)
	// when the circuit is open and the request must be rejected.
	BeforeRequest() error

	// AfterRequest records the outcome of a request. nil records a
	// success; a non-nil error is evaluated for failure counting.
	AfterRequest(err error)
}
