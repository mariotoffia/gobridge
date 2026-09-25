package route

import (
	"context"
	"errors"
	"testing"
)

// BenchmarkRouteRunner_RestartAfterClose measures one supervised restart cycle
// on the same runner: Run attaches the receiver, the receiver fails, and Run
// closes it before returning. "no_delivery" is the bare Run→Close cycle a
// flapping route pays per restart; "one_delivery" adds a delivery that settles
// through the sender before the close.
func BenchmarkRouteRunner_RestartAfterClose(b *testing.B) {
	errDropped := errors.New("receiver link dropped")

	for _, bc := range []struct {
		name string
		del  *stubDelivery
	}{
		{name: "no_delivery"},
		{name: "one_delivery", del: &stubDelivery{env: generatedIDEnv("bench-restart")}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			rcv := &closingReceiver{runErr: errDropped}
			if bc.del != nil { // a nil *stubDelivery would be a non-nil ports.Delivery
				rcv.del = bc.del
			}
			// benchRunner takes no receiver, so attach it after construction.
			r := benchRunner(b, stubSender{})
			r.receiver = rcv

			b.ReportAllocs()
			for b.Loop() {
				if bc.del != nil {
					bc.del.acked = false
				}
				if err := r.Run(context.Background()); !errors.Is(err, errDropped) {
					b.Fatalf("Run error = %v, want the receiver error", err)
				}
				if bc.del != nil && !bc.del.acked {
					b.Fatal("the delivery must be acked before Run closes the receiver")
				}
			}
			if runs, closes := rcv.runCalls.Load(), rcv.closes.Load(); runs != closes {
				b.Fatalf("receiver runs = %d, closes = %d; Run must close its receiver on every exit", runs, closes)
			}
		})
	}
}
