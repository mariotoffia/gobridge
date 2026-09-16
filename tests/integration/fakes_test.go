package integration_test

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

type bestEffortSender func(context.Context, ports.OutboundMessage) error

func (s bestEffortSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	return s(ctx, msg)
}

type bestEffortDLQ struct {
	fakeDLQStore
	failure error
	before  func()
	writes  atomic.Int32
}

func (s *bestEffortDLQ) Write(ctx context.Context, entry routing.DLQEntry) error {
	s.writes.Add(1)
	if s.before != nil {
		s.before()
	}
	if s.failure != nil {
		return s.failure
	}
	return s.fakeDLQStore.Write(ctx, entry)
}

type bestEffortHook struct {
	mu       sync.Mutex
	outcomes []ports.DeliveryOutcome
}

func (*bestEffortHook) OnAttempt(context.Context, ports.DeliveryAttempt) {}
func (h *bestEffortHook) OnSettled(_ context.Context, outcome ports.DeliveryOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outcomes = append(h.outcomes, outcome)
}
func (h *bestEffortHook) settled() []ports.DeliveryOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.DeliveryOutcome(nil), h.outcomes...)
}

var _ ports.Sender = bestEffortSender(nil)
var _ ports.DLQStore = (*bestEffortDLQ)(nil)
var _ ports.DeliveryHook = (*bestEffortHook)(nil)
