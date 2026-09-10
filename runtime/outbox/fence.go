package outbox

// Fence prevents subsequent batches from claiming work, including finalDrain.
func (d *Drainer) Fence() { d.fenced.Store(true) }
