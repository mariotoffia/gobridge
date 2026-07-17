package circuitbreaker

import "github.com/mariotoffia/gobridge/ports"

// Compile-time guarantee that *Breaker satisfies the ports.CircuitBreaker
// contract and its concurrency-safe CircuitBreakerAdmitter capability.
// Adapters depend only on the ports; this package supplies the concrete
// state-machine implementation.
var (
	_ ports.CircuitBreaker         = (*Breaker)(nil)
	_ ports.CircuitBreakerAdmitter = (*Breaker)(nil)
)
