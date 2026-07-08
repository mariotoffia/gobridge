package transporttest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DeliveryProbe couples a ports.Delivery under test with an observation of the
// broker-side effect its settle calls produce. The conformance suite settles
// through Delivery() and asserts the resulting disposition through the probe,
// so it never has to reach into transport internals.
//
// A probe MUST start in DispositionNone with BrokerOps() == 0 (freshly built,
// unsettled). Delivery() MUST return the same handle on every call.
type DeliveryProbe interface {
	// Delivery is the ports.Delivery under test.
	Delivery() ports.Delivery
	// Disposition reports the broker-side disposition currently in effect. It
	// is DispositionNone until a settle op takes effect, then latches to
	// DispositionAcked or DispositionRetried for the delivery's lifetime.
	Disposition() Disposition
	// BrokerOps reports how many broker-side settle operations (ack/delete or
	// redeliver) have ACTUALLY been performed against the source. A conformant
	// delivery performs at most one, because settlement is latched and further
	// settle calls are no-ops.
	BrokerOps() int
}

// DeliveryFactory builds a fresh, UNSETTLED DeliveryProbe for one test case.
// Every call MUST return an independent delivery backed by independent broker
// state; the suite calls it once per subtest.
type DeliveryFactory func(t *testing.T) DeliveryProbe

// RunDeliveryConformanceTests runs the ports.Delivery settlement state-machine
// conformance suite against deliveries produced by factory. caps declares which
// optional operations the transport supports so the suite inverts the
// ErrNotSupported / no-latch assertions accordingly.
//
// The suite pins the canonical contract documented on ports.Delivery: latched
// settlement, idempotent + mutually-exclusive Ack/Retry, ErrNotSupported never
// latching, and Extend never settling. All subtests are race-detector safe.
func RunDeliveryConformanceTests(t *testing.T, factory DeliveryFactory, caps Caps) {
	t.Helper()

	t.Run("AckSettlesAndIsSingleBrokerOp", func(t *testing.T) {
		deliveryAckSettles(t, factory)
	})
	t.Run("DoubleAckIsNilNoOp", func(t *testing.T) {
		deliveryDoubleAck(t, factory)
	})
	t.Run("RetryAfterAckDoesNotRedeliver", func(t *testing.T) {
		deliveryRetryAfterAck(t, factory, caps)
	})
	t.Run("AckAfterRetryDoesNotAck", func(t *testing.T) {
		deliveryAckAfterRetry(t, factory, caps)
	})
	t.Run("DoubleRetryIsNilNoOp", func(t *testing.T) {
		deliveryDoubleRetry(t, factory, caps)
	})
	t.Run("Extend", func(t *testing.T) {
		deliveryExtend(t, factory, caps)
	})
	t.Run("RetryWhenUnsupported", func(t *testing.T) {
		deliveryRetryUnsupported(t, factory, caps)
	})
	t.Run("ConcurrentSettleElectsOneWinner", func(t *testing.T) {
		deliveryConcurrentSettle(t, factory, caps)
	})
}

func deliveryAckSettles(t *testing.T, factory DeliveryFactory) {
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("Ack: unexpected error: %v", err)
	}
	if got := p.Disposition(); got != DispositionAcked {
		t.Fatalf("after Ack disposition = %s, want acked", got)
	}
	if got := p.BrokerOps(); got != 1 {
		t.Fatalf("after Ack broker ops = %d, want 1", got)
	}
}

func deliveryDoubleAck(t *testing.T, factory DeliveryFactory) {
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("first Ack: %v", err)
	}
	// Second Ack MUST be a nil no-op and MUST NOT perform another broker op.
	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("second Ack must be a nil no-op, got %v", err)
	}
	if got := p.BrokerOps(); got != 1 {
		t.Fatalf("double Ack broker ops = %d, want 1 (no second ack)", got)
	}
	if got := p.Disposition(); got != DispositionAcked {
		t.Fatalf("double Ack disposition = %s, want acked", got)
	}
}

func deliveryRetryAfterAck(t *testing.T, factory DeliveryFactory, caps Caps) {
	if !caps.SupportsRetry {
		t.Skip("transport does not support Retry")
	}
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	// Retry after a latched Ack MUST NOT redeliver — it is a nil no-op.
	if err := p.Delivery().Retry(ctx, 0, errors.New("late retry")); err != nil {
		t.Fatalf("Retry after Ack must be a nil no-op, got %v", err)
	}
	if got := p.Disposition(); got != DispositionAcked {
		t.Fatalf("Retry-after-Ack changed disposition to %s, want acked (no redelivery)", got)
	}
	if got := p.BrokerOps(); got != 1 {
		t.Fatalf("Retry-after-Ack broker ops = %d, want 1 (no redelivery)", got)
	}
}

func deliveryAckAfterRetry(t *testing.T, factory DeliveryFactory, caps Caps) {
	if !caps.SupportsRetry {
		t.Skip("transport does not support Retry")
	}
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	if err := p.Delivery().Retry(ctx, 0, errors.New("boom")); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got := p.Disposition(); got != DispositionRetried {
		t.Fatalf("after Retry disposition = %s, want retried", got)
	}
	// This is the exact message-loss bug the contract forbids: an Ack after a
	// latched Retry MUST NOT ack/delete the message.
	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("Ack after Retry must be a nil no-op, got %v", err)
	}
	if got := p.Disposition(); got != DispositionRetried {
		t.Fatalf("Ack-after-Retry changed disposition to %s, want retried (no ack/delete)", got)
	}
	if got := p.BrokerOps(); got != 1 {
		t.Fatalf("Ack-after-Retry broker ops = %d, want 1 (no ack/delete)", got)
	}
}

func deliveryDoubleRetry(t *testing.T, factory DeliveryFactory, caps Caps) {
	if !caps.SupportsRetry {
		t.Skip("transport does not support Retry")
	}
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	if err := p.Delivery().Retry(ctx, 0, errors.New("boom")); err != nil {
		t.Fatalf("first Retry: %v", err)
	}
	if err := p.Delivery().Retry(ctx, 0, errors.New("boom-again")); err != nil {
		t.Fatalf("second Retry must be a nil no-op, got %v", err)
	}
	if got := p.BrokerOps(); got != 1 {
		t.Fatalf("double Retry broker ops = %d, want 1", got)
	}
	if got := p.Disposition(); got != DispositionRetried {
		t.Fatalf("double Retry disposition = %s, want retried", got)
	}
}

func deliveryExtend(t *testing.T, factory DeliveryFactory, caps Caps) {
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	until := time.Now().Add(30 * time.Second)
	err := p.Delivery().Extend(ctx, until)

	if !caps.SupportsExtend {
		if !errors.Is(err, shared.ErrNotSupported) {
			t.Fatalf("Extend on a non-supporting transport = %v, want ErrNotSupported", err)
		}
		if got := p.Disposition(); got != DispositionNone {
			t.Fatalf("unsupported Extend settled the delivery (disposition = %s)", got)
		}
		return
	}

	if err != nil {
		t.Fatalf("Extend: unexpected error: %v", err)
	}
	// Extend is NOT a settlement: the delivery must remain settleable.
	if got := p.Disposition(); got != DispositionNone {
		t.Fatalf("Extend settled the delivery (disposition = %s), want none", got)
	}
	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("Ack after Extend: %v", err)
	}
	if got := p.Disposition(); got != DispositionAcked {
		t.Fatalf("Ack after Extend disposition = %s, want acked", got)
	}
}

func deliveryRetryUnsupported(t *testing.T, factory DeliveryFactory, caps Caps) {
	if caps.SupportsRetry {
		t.Skip("transport supports Retry")
	}
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	err := p.Delivery().Retry(ctx, 0, errors.New("boom"))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("Retry on a non-supporting transport = %v, want ErrNotSupported", err)
	}
	// ErrNotSupported MUST NOT latch: a fallback Ack must still settle.
	if got := p.Disposition(); got != DispositionNone {
		t.Fatalf("unsupported Retry latched settlement (disposition = %s)", got)
	}
	if err := p.Delivery().Ack(ctx); err != nil {
		t.Fatalf("fallback Ack after unsupported Retry: %v", err)
	}
	if got := p.Disposition(); got != DispositionAcked {
		t.Fatalf("fallback Ack disposition = %s, want acked", got)
	}
}

// deliveryConcurrentSettle races many settle calls and asserts exactly one
// broker op takes effect: the unsettled→settled transition is atomic and the
// losers observe the settled state as no-ops. Run with -race.
func deliveryConcurrentSettle(t *testing.T, factory DeliveryFactory, caps Caps) {
	ctx := context.Background()
	p := factory(t)
	requireFreshProbe(t, p)

	const goroutines = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			// Interleave Ack and Retry to race both dispositions.
			if caps.SupportsRetry && i%2 == 1 {
				_ = p.Delivery().Retry(ctx, 0, errors.New("race"))
				return
			}
			_ = p.Delivery().Ack(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := p.BrokerOps(); got != 1 {
		t.Fatalf("concurrent settle broker ops = %d, want exactly 1", got)
	}
	if got := p.Disposition(); got == DispositionNone {
		t.Fatalf("concurrent settle left delivery unsettled")
	}
}

func requireFreshProbe(t *testing.T, p DeliveryProbe) {
	t.Helper()
	if p.Delivery() == nil {
		t.Fatal("DeliveryProbe.Delivery() returned nil")
	}
	if p.Delivery().Envelope() == nil {
		t.Fatal("Delivery.Envelope() returned nil")
	}
	if got := p.Disposition(); got != DispositionNone {
		t.Fatalf("fresh probe disposition = %s, want none", got)
	}
	if got := p.BrokerOps(); got != 0 {
		t.Fatalf("fresh probe broker ops = %d, want 0", got)
	}
}
