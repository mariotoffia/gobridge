package bridge

import (
	"fmt"
	"io"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// instrumentedClosableLeaseStore decorates a lease store with the runtime's
// metric instrumentation while preserving the inner store's io.Closer.
//
// The runtime's *runtime.InstrumentedLeaseStore implements only the
// metric-bearing store methods; it does NOT forward Close (unlike
// NewInstrumentedOutboxStoreCapabilityPreserving, which dynamically re-exports
// io.Closer and OutboxReleaser). Yet runtime.Stop releases durable lease-store
// handles (e.g. SQLite files) via an io.Closer type assertion on the store instance it was
// given, so wrapping a closable lease store with the bare decorator would hide
// its Close and leak the OS handle on every reconfiguration. This adapter
// promotes the decorator's store methods via embedding and re-exposes Close,
// forwarding to the inner store when it holds OS resources.
type instrumentedClosableLeaseStore struct {
	*runtime.InstrumentedLeaseStore
	inner ports.LeaseStore
}

// Close forwards to the wrapped store when it holds OS resources; in-memory
// stores do not implement io.Closer and Close is a no-op, mirroring runtime.Stop.
func (s instrumentedClosableLeaseStore) Close() error {
	if c, ok := s.inner.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("close instrumented lease store: %w", err)
		}
	}
	return nil
}
