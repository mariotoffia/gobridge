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

// CircuitBreakerAdmitter is the CONCURRENCY-SAFE admission surface a
// CircuitBreaker implementation may additionally provide (an optional
// capability interface, like NonDurableEgressReporter). The token-less
// BeforeRequest/AfterRequest pair cannot tell that the circuit changed
// state while a request was in flight, so with several requests in flight
// on one breaker a late outcome is mis-accounted against the CURRENT
// generation — a stale half-open probe release or a spurious re-open
// (MQTT-O3). AdmitRequest closes that: the returned settle callback is
// bound to the generation the request was admitted in, and the
// implementation discards an outcome settled after a state transition.
// Adapters that may have concurrent requests in flight (route
// max_in_flight > 1) must prefer this surface whenever the breaker
// provides it and fall back to the legacy pair otherwise.
type CircuitBreakerAdmitter interface {
	// AdmitRequest admits one request. On rejection it returns a nil settle
	// and the rejection error (typically shared.ErrUnavailable with a
	// RetryAfter hint). On admission it returns a settle callback that MUST
	// be invoked exactly once with the request outcome (nil = success),
	// ideally via defer; an admitted half-open probe that never settles
	// holds its probe slot until the implementation's probe timeout.
	AdmitRequest() (settle func(error), err error)
}

// AdmitCircuitBreaker admits one request through cb, preferring the
// generation-safe CircuitBreakerAdmitter capability when cb provides it and
// falling back to the legacy BeforeRequest/AfterRequest pair otherwise. It
// is the single admission helper every adapter call site routes through, so
// a breaker that grows the capability upgrades all of them at once. The
// returned settle MUST be invoked exactly once with the request outcome.
func AdmitCircuitBreaker(cb CircuitBreaker) (settle func(error), err error) {
	if admitter, ok := cb.(CircuitBreakerAdmitter); ok {
		return admitter.AdmitRequest()
	}
	if err := cb.BeforeRequest(); err != nil {
		return nil, err
	}
	return cb.AfterRequest, nil
}
