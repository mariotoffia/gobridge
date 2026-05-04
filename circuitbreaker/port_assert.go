package circuitbreaker

import "github.com/mariotoffia/gobridge/ports"

// Compile-time guarantee that *Breaker satisfies the ports.CircuitBreaker
// contract. Adapters depend only on the port; this package supplies the
// concrete state-machine implementation.
var _ ports.CircuitBreaker = (*Breaker)(nil)
