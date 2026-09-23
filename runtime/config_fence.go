package runtime

// Fence withdraws configuration authorization immediately. It cancels intake
// and outstanding delivery work, leaving unsettled sources to redelivery, and
// forbids fresh outbox claims including the ordinary shutdown final batch.
// Call Stop next to join work, close transports and release leases. Fence alone
// does not prove quiescence and must never be advertised as a completed stop.
func (rt *Runtime) Fence() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.fenced = true
	rt.healthy = false
	for _, d := range rt.drainers {
		d.drainer.Fence()
	}
	// A drainer a Retire has taken out may still run its final batch.
	for _, u := range rt.retiring {
		for _, d := range u.drainers {
			d.drainer.Fence()
		}
	}
	if rt.cancel != nil {
		rt.cancel()
	}
}
