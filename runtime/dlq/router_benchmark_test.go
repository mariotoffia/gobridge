package dlq_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// Route is on the settle-critical path: it runs inside the route runner's
// per-delivery goroutine while that goroutine holds a global in-flight slot.
// The hold timer added for the DLQ-outage signal is emitted once per Route, so
// this benchmark measures the confirmed-write path with the timer in place.
func BenchmarkRouter_Route_Confirmed(b *testing.B) {
	router := dlq.NewFromConfig(dlq.Config{
		Store:            NewFakeStore(),
		Metrics:          &ports.NoopExporter{},
		WriteTimeout:     dlq.RuntimeWriteTimeout,
		WriteMaxAttempts: dlq.RuntimeWriteMaxAttempts,
	})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "bench-1",
		Subject: "bench/dlq",
		Payload: []byte("payload"),
	})
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		_ = router.Route(ctx, env, "route-bench", "bind-1", "devices/1/state",
			"sess-bench", "src-bench", shared.ErrUnavailable, 1)
	}
}
