package integration_test

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	amqp10sdk "github.com/Azure/go-amqp"

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

type failedAMQP10Settler struct {
	failure  error
	accepts  atomic.Int32
	releases atomic.Int32
	modifies atomic.Int32
}

func (s *failedAMQP10Settler) AcceptMessage(context.Context, *amqp10sdk.Message) error {
	s.accepts.Add(1)
	return s.failure
}

func (s *failedAMQP10Settler) ReleaseMessage(context.Context, *amqp10sdk.Message) error {
	s.releases.Add(1)
	return s.failure
}

func (s *failedAMQP10Settler) ModifyMessage(context.Context, *amqp10sdk.Message, *amqp10sdk.ModifyMessageOptions) error {
	s.modifies.Add(1)
	return s.failure
}

type failedAMQP091Acknowledger struct {
	failure error
	acks    atomic.Int32
	nacks   atomic.Int32
}

func (a *failedAMQP091Acknowledger) Ack(uint64, bool) error {
	a.acks.Add(1)
	return a.failure
}

func (a *failedAMQP091Acknowledger) Nack(uint64, bool, bool) error {
	a.nacks.Add(1)
	return a.failure
}

func (a *failedAMQP091Acknowledger) Reject(uint64, bool) error { return a.failure }

type observedRetryDelivery struct {
	ports.Delivery
	acks    atomic.Int32
	retries atomic.Int32
}

func (d *observedRetryDelivery) Ack(ctx context.Context) error {
	d.acks.Add(1)
	return d.Delivery.Ack(ctx)
}

func (d *observedRetryDelivery) Retry(ctx context.Context, after time.Duration, reason error) error {
	d.retries.Add(1)
	return d.Delivery.Retry(ctx, after, reason)
}

var _ ports.Delivery = (*observedRetryDelivery)(nil)
