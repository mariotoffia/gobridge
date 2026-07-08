package ports

import (
	"context"
	"sync"
)

type deliveryScopeKey struct{}

// DeliveryScope collects release callbacks a processor registers during one
// delivery and runs them once the runtime finishes handling that delivery.
// It lets a processor bracket a resource across the WHOLE delivery even though
// Process() returns mid-chain. Safe for concurrent use: chain processors run on
// their own goroutines (runtime/route/chain.go).
type DeliveryScope struct {
	mu       sync.Mutex
	done     bool
	releases []func()
}

// OnRelease registers fn to run when the delivery finishes. If the scope has
// already been released — e.g. the registering processor goroutine was
// abandoned on a chain timeout and lands here after Release — fn runs inline so
// no release is ever stranded.
func (s *DeliveryScope) OnRelease(fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		fn()
		return
	}
	s.releases = append(s.releases, fn)
	s.mu.Unlock()
}

// Release runs each registered callback once (LIFO, mirroring defer) and marks
// the scope done. Idempotent.
func (s *DeliveryScope) Release() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	rel := s.releases
	s.releases = nil
	s.mu.Unlock()
	for i := len(rel) - 1; i >= 0; i-- {
		rel[i]()
	}
}

// WithDeliveryScope installs a fresh scope on ctx. The runtime calls it once per
// delivery and defers scope.Release().
func WithDeliveryScope(ctx context.Context) (context.Context, *DeliveryScope) {
	s := &DeliveryScope{}
	return context.WithValue(ctx, deliveryScopeKey{}, s), s
}

// DeliveryScopeFrom returns the scope carried by ctx, if any.
func DeliveryScopeFrom(ctx context.Context) (*DeliveryScope, bool) {
	s, ok := ctx.Value(deliveryScopeKey{}).(*DeliveryScope)
	return s, ok
}
