package circuitbreaker

import (
	"context"
	"testing"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

func BenchmarkBreaker_BeforeAfter_Closed(b *testing.B) {
	br := cb.NewBreaker("bench", cb.Config{
		FailureThreshold: 100,
		SuccessThreshold: 2,
		ResetTimeout:     time.Second,
	}, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = br.BeforeRequest()
		br.AfterRequest(nil)
	}
}

func BenchmarkBreaker_BeforeAfter_Failures(b *testing.B) {
	br := cb.NewBreaker("bench", cb.Config{
		FailureThreshold: b.N + 1,
		SuccessThreshold: 2,
		ResetTimeout:     time.Second,
	}, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = br.BeforeRequest()
		br.AfterRequest(errTest)
	}
}

func BenchmarkProcessor_PerKey_Lookup(b *testing.B) {
	cfg := cb.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     time.Second,
	}
	p := New("bench", cfg)

	next := func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	}

	ctx := context.Background()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "1", Subject: "bench-subject"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Process(ctx, env, next)
	}
}

func BenchmarkProcessor_MultiKey(b *testing.B) {
	cfg := cb.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     time.Second,
	}
	p := New("bench", cfg, WithKeyExtractor(SubjectKey))

	next := func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	}

	subjects := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "1", Subject: subjects[i%len(subjects)]})
		_ = p.Process(ctx, env, next)
	}
}

var _ ports.Processor = (*Processor)(nil)
