package transporttest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// This file is a minimal, fully-compliant reference transport. It exists to
// (a) prove the conformance suites in this package are correct — reference_test.go
// runs every suite against it — and (b) serve as living documentation of the
// canonical ports.Delivery / ports.Receiver / ports.Sender contracts. An
// adapter author can read it as the smallest thing that passes the kit.

// refDelivery is a reference ports.Delivery implementing the canonical
// settlement state machine: unsettled until the first successful settle,
// latched thereafter, with idempotent + mutually-exclusive Ack/Retry. It models
// the "broker" as an op counter and a latched disposition guarded by mu.
type refDelivery struct {
	env  *messaging.Envelope
	caps Caps

	mu      sync.Mutex
	settled bool
	ops     int         // broker-side settle operations actually performed
	disp    Disposition // latched disposition
}

var _ ports.Delivery = (*refDelivery)(nil)

func (d *refDelivery) Envelope() *messaging.Envelope { return d.env }

// settle records the single broker-side operation. The caller holds d.mu and
// has verified the delivery is not yet settled, so this runs at most once.
func (d *refDelivery) settle(disp Disposition) {
	d.ops++
	d.disp = disp
	d.settled = true
}

func (d *refDelivery) Ack(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settled {
		return nil // idempotent no-op: broker untouched
	}
	d.settle(DispositionAcked)
	return nil
}

func (d *refDelivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	if !d.caps.SupportsRetry {
		// ErrNotSupported never latches: the delivery stays unsettled.
		return shared.ErrNotSupported
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settled {
		return nil // idempotent no-op: no redelivery
	}
	d.settle(DispositionRetried)
	return nil
}

func (d *refDelivery) Extend(_ context.Context, _ time.Time) error {
	if !d.caps.SupportsExtend {
		return shared.ErrNotSupported
	}
	// Extend is not a settlement; there is nothing to record.
	return nil
}

func (d *refDelivery) disposition() Disposition {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.disp
}

func (d *refDelivery) brokerOps() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ops
}

// refDeliveryProbe adapts *refDelivery to DeliveryProbe.
type refDeliveryProbe struct{ d *refDelivery }

var _ DeliveryProbe = refDeliveryProbe{}

func (p refDeliveryProbe) Delivery() ports.Delivery { return p.d }
func (p refDeliveryProbe) Disposition() Disposition { return p.d.disposition() }
func (p refDeliveryProbe) BrokerOps() int           { return p.d.brokerOps() }

func newRefDelivery(caps Caps, id string) *refDelivery {
	return &refDelivery{env: makeEnvelope(id), caps: caps}
}

// ReferenceDeliveryFactory returns a DeliveryFactory that builds fresh,
// fully-compliant reference deliveries honouring the given caps. Use it as the
// factory argument to RunDeliveryConformanceTests.
func ReferenceDeliveryFactory(caps Caps) DeliveryFactory {
	var n int64
	return func(_ *testing.T) DeliveryProbe {
		id := fmt.Sprintf("ref-delivery-%d", atomic.AddInt64(&n, 1))
		return refDeliveryProbe{d: newRefDelivery(caps, id)}
	}
}

// refReceiver is a reference ports.Receiver. It emits each seeded delivery
// exactly once, SERIALLY from Run's own goroutine, NEVER settles a delivery
// itself, and on an emit error surfaces the failure by returning it (as the
// prevailing adapters do) without settling. Once every delivery is emitted it
// blocks until ctx is cancelled.
type refReceiver struct {
	deliveries []*refDelivery
}

var _ ports.Receiver = (*refReceiver)(nil)

func (r *refReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	for _, d := range r.deliveries {
		if err := emit(ctx, d); err != nil {
			// Contract: the delivery MUST NOT be settled here; leave it to
			// transport redelivery. Surface the failure to the supervisor.
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// ReferenceReceiverFactory returns a ReceiverFactory that builds a
// fully-compliant reference receiver seeded with n deliveries honouring the
// given caps. Use it as the factory argument to RunReceiverConformanceTests.
func ReferenceReceiverFactory(caps Caps) ReceiverFactory {
	return func(_ *testing.T, n int) SeededReceiver {
		ds := make([]*refDelivery, n)
		probes := make([]DeliveryProbe, n)
		for i := 0; i < n; i++ {
			d := newRefDelivery(caps, fmt.Sprintf("ref-recv-%d", i))
			ds[i] = d
			probes[i] = refDeliveryProbe{d: d}
		}
		return SeededReceiver{Receiver: &refReceiver{deliveries: ds}, Probes: probes}
	}
}

// refSender is a reference ports.Sender + ports.BatchSender. It records every
// dispatched message and fail-fast rejects a whole batch that contains a nil
// envelope, dispatching nothing in that case.
type refSender struct {
	mu   sync.Mutex
	sent []SentMessage
}

var (
	_ ports.Sender      = (*refSender)(nil)
	_ ports.BatchSender = (*refSender)(nil)
)

func (s *refSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	if msg.Envelope == nil {
		return fmt.Errorf("reference sender: nil envelope: %w", shared.ErrInvalidPayload)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.sent = append(s.sent, SentMessage{Address: msg.Address, EnvelopeID: msg.Envelope.ID()})
	s.mu.Unlock()
	return nil
}

func (s *refSender) SendBatch(ctx context.Context, msgs []ports.OutboundMessage) ([]ports.BatchResult, error) {
	// Fail-fast, whole-batch pre-validation: a nil envelope rejects the entire
	// batch before anything is dispatched.
	for i := range msgs {
		if msgs[i].Envelope == nil {
			return nil, fmt.Errorf("reference sender: message %d has nil envelope: %w", i, shared.ErrInvalidPayload)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	results := make([]ports.BatchResult, len(msgs))
	s.mu.Lock()
	for i, m := range msgs {
		s.sent = append(s.sent, SentMessage{Address: m.Address, EnvelopeID: m.Envelope.ID()})
		results[i] = ports.BatchResult{Index: i}
	}
	s.mu.Unlock()
	return results, nil
}

func (s *refSender) snapshot() []SentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SentMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// refSenderProbe adapts *refSender to SenderProbe.
type refSenderProbe struct{ s *refSender }

var _ SenderProbe = refSenderProbe{}

func (p refSenderProbe) Sender() ports.Sender { return p.s }
func (p refSenderProbe) Sent() []SentMessage  { return p.s.snapshot() }

// ReferenceSenderFactory returns a SenderFactory that builds a fully-compliant
// reference sender (also a ports.BatchSender). Use it as the factory argument
// to RunSenderConformanceTests.
func ReferenceSenderFactory(_ Caps) SenderFactory {
	return func(_ *testing.T) SenderProbe {
		return refSenderProbe{s: &refSender{}}
	}
}
