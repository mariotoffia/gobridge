package circuitbreaker

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func BenchmarkBreaker_BeforeAfter_Closed(b *testing.B) {
	br := NewBreaker("bench", Config{
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
	br := NewBreaker("bench", Config{
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
	cfg := Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     time.Second,
	}
	p := New("bench", cfg)

	next := func(_ context.Context, _ *domain.Envelope) error {
		return nil
	}

	ctx := context.Background()
	env := &domain.Envelope{ID: "1", Subject: "bench-subject"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Process(ctx, env, next)
	}
}

func BenchmarkProcessor_MultiKey(b *testing.B) {
	cfg := Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     time.Second,
	}
	p := New("bench", cfg, WithKeyExtractor(SubjectKey))

	next := func(_ context.Context, _ *domain.Envelope) error {
		return nil
	}

	subjects := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env := &domain.Envelope{ID: "1", Subject: subjects[i%len(subjects)]}
		_ = p.Process(ctx, env, next)
	}
}

var _ ports.Processor = (*Processor)(nil)
