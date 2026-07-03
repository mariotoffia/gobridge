package circuitbreaker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestDefaultOnStateChange_LogsTransitionsAtWarn: with no
// WithOnStateChange override, a breaker state transition must be
// visible by default — logged at Warn via the processor's logger.
func TestDefaultOnStateChange_LogsTransitionsAtWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg := cb.Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("obs-cb", cfg, WithLogger(logger))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"})
	fail := func(_ context.Context, _ *messaging.Envelope) error { return errors.New("boom") }

	// One countable failure (threshold 1) trips closed -> open; the
	// state-change notification runs synchronously inside Process.
	_ = p.Process(context.Background(), env, fail)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected Warn-level log for state transition, got %q", out)
	}
	for _, want := range []string{"circuit breaker state change", "processor=obs-cb", "key=global", "from=closed", "to=open"} {
		if !strings.Contains(out, want) {
			t.Fatalf("state-change log missing %q, got %q", want, out)
		}
	}
}

// TestWithOnStateChange_OverridesDefaultLogging: an explicit callback
// replaces the default logging handler entirely.
func TestWithOnStateChange_OverridesDefaultLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	var got []string
	cfg := cb.Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("obs-cb", cfg,
		WithLogger(logger),
		WithOnStateChange(func(key string, from, to cb.State) {
			got = append(got, key+":"+from.String()+"->"+to.String())
		}))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"})
	fail := func(_ context.Context, _ *messaging.Envelope) error { return errors.New("boom") }
	_ = p.Process(context.Background(), env, fail)

	if len(got) != 1 || got[0] != "global:closed->open" {
		t.Fatalf("custom callback transitions = %v, want [global:closed->open]", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("default logging handler ran despite WithOnStateChange override: %q", buf.String())
	}
}

// TestEviction_PrefersLeastRecentlyUsed: with the O(1) LRU eviction, a
// full cache drops the least-recently-used idle closed breaker — a key
// touched again is more recent than an older untouched key and must
// survive.
func TestEviction_PrefersLeastRecentlyUsed(t *testing.T) {
	const capacity = 3
	cfg := cb.Config{FailureThreshold: 100, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("lru", cfg, WithKeyExtractor(SubjectKey), WithMaxBreakers(capacity))

	next := func(_ context.Context, _ *messaging.Envelope) error { return nil }
	process := func(subject string) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: subject})
		_ = p.Process(context.Background(), env, next)
	}

	process("a")
	process("b")
	process("c")
	process("a") // refresh "a": "b" is now the LRU tail

	process("d") // full cache: must evict "b", the least recently used

	m := p.Metrics()
	if _, ok := m["b"]; ok {
		t.Fatalf("expected LRU key %q evicted, cache = %v", "b", keysOf(m))
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("expected key %q to survive, cache = %v", k, keysOf(m))
		}
	}
	if s := p.Stats(); s.Evictions != 1 || s.OpenEvictions != 0 {
		t.Fatalf("unexpected stats: %+v", s)
	}
}

// TestEviction_ScanIsBounded: eviction on a full cache must not walk
// the whole cache — it examines at most maxEvictionScan LRU-tail
// entries. Pinning every scannable tail entry within the bound forces
// the bounded-overshoot path: no eviction, insert proceeds anyway.
func TestEviction_ScanIsBounded(t *testing.T) {
	const capacity = 4
	cfg := cb.Config{FailureThreshold: 100, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("bounded", cfg, WithKeyExtractor(SubjectKey), WithMaxBreakers(capacity))

	next := func(_ context.Context, _ *messaging.Envelope) error { return nil }
	for i := 0; i < capacity; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: fmt.Sprintf("k-%d", i)})
		_ = p.Process(context.Background(), env, next)
	}

	// Pin every cached entry as if a Process call were mid-flight on it.
	p.mu.Lock()
	for _, e := range p.breakers {
		e.inFlight.Add(1)
	}
	p.mu.Unlock()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "overflow"})
	if err := p.Process(context.Background(), env, next); err != nil {
		t.Fatalf("Process with all entries pinned: %v", err)
	}

	// Nothing evictable: bounded overshoot, no eviction counted.
	s := p.Stats()
	if s.Evictions != 0 {
		t.Fatalf("evicted a pinned breaker: %+v", s)
	}
	if s.Size != capacity+1 {
		t.Fatalf("Size = %d, want bounded overshoot %d", s.Size, capacity+1)
	}

	// Unpin; the next overflow insert must evict again.
	p.mu.Lock()
	for _, e := range p.breakers {
		if e.key != "overflow" {
			e.inFlight.Add(-1)
		}
	}
	p.mu.Unlock()

	env = messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "overflow-2"})
	_ = p.Process(context.Background(), env, next)
	if s := p.Stats(); s.Evictions != 1 {
		t.Fatalf("expected eviction after unpinning, stats = %+v", s)
	}
}

func keysOf(m map[string]cb.BreakerMetrics) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestMetrics_EmittedOnTripAndRejection: the shared circuit-breaker
// counters must be emitted — StateChanged + Trips when the breaker
// opens, Rejections when an open breaker short-circuits a request —
// and emission must not depend on the state-change handler installed.
func TestMetrics_EmittedOnTripAndRejection(t *testing.T) {
	rec := &ports.RecordingExporter{}
	cfg := cb.Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("metrics-cb", cfg,
		WithMetrics(rec),
		// A custom handler must NOT suppress metric emission.
		WithOnStateChange(func(string, cb.State, cb.State) {}),
	)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"})
	fail := func(_ context.Context, _ *messaging.Envelope) error { return errors.New("boom") }

	// Trip: closed -> open (threshold 1).
	_ = p.Process(context.Background(), env, fail)
	// Rejection: breaker is open, next never runs.
	nextRan := false
	_ = p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		nextRan = true
		return nil
	})
	if nextRan {
		t.Fatal("next ran while the breaker was open")
	}

	counts := map[string]int64{}
	tags := map[string]map[string]string{}
	for _, e := range rec.Entries() {
		if e.Kind != "counter" {
			continue
		}
		counts[e.Name] += e.IValue
		m := map[string]string{}
		for _, tg := range e.Tags {
			m[tg.Key] = tg.Value
		}
		tags[e.Name] = m
	}

	if counts[shared.MetricCircuitBreakerStateChanged] != 1 {
		t.Fatalf("StateChanged = %d, want 1 (entries: %+v)", counts[shared.MetricCircuitBreakerStateChanged], rec.Entries())
	}
	if counts[shared.MetricCircuitBreakerTrips] != 1 {
		t.Fatalf("Trips = %d, want 1", counts[shared.MetricCircuitBreakerTrips])
	}
	if counts[shared.MetricCircuitBreakerRejections] != 1 {
		t.Fatalf("Rejections = %d, want 1", counts[shared.MetricCircuitBreakerRejections])
	}

	sc := tags[shared.MetricCircuitBreakerStateChanged]
	if sc["processor"] != "metrics-cb" || sc["key"] != "global" || sc["to"] != "open" {
		t.Fatalf("StateChanged tags = %v, want processor=metrics-cb key=global to=open", sc)
	}
	rj := tags[shared.MetricCircuitBreakerRejections]
	if rj["processor"] != "metrics-cb" || rj["key"] != "global" {
		t.Fatalf("Rejections tags = %v, want processor=metrics-cb key=global", rj)
	}
}
