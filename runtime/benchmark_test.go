package runtime_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

func BenchmarkRunChain_NoProcessors(b *testing.B) {
	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "1", Subject: "bench"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = route.RunChain(ctx, nil, env)
	}
}

func BenchmarkRunChain_OneProcessor(b *testing.B) {
	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "1", Subject: "bench"})
	procs := []ports.Processor{&FakeProcessor{
		NameVal: "passthrough",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			return next(ctx, env)
		},
	}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = route.RunChain(ctx, procs, env)
	}
}

func BenchmarkRunChain_FiveProcessors(b *testing.B) {
	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "1", Subject: "bench"})
	var procs []ports.Processor
	for i := 0; i < 5; i++ {
		procs = append(procs, &FakeProcessor{
			NameVal: "p",
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				return next(ctx, env)
			},
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = route.RunChain(ctx, procs, env)
	}
}

func BenchmarkDLQRouter_Route(b *testing.B) {
	store := NewFakeDLQStore()
	dlq := dlq.New(store)
	ctx := context.Background()
	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "bench-msg",
		Headers: map[string]any{messaging.HeaderCorrelationID: "corr-1"},
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = dlq.Route(ctx, env, "route-1", "bind-1", "", "", "", shared.ErrNotFound, 1)
	}
}
