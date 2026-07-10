package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════
// C10 (HIGH-2, c10-cb-metric-cardinality): the breaker key can be
// caller-controlled (WithKeyExtractor(HeaderKey("tenant-id"))), so
// tagging it verbatim on the shared circuit-breaker metrics
// (CircuitBreakerStateChanged / Trips / Rejections) let one producer
// sending unique header values drive UNBOUNDED metric cardinality -- a
// telemetry cost / throttling DoS during the exact outage the breaker
// should make observable.
//
// Contract now: the "key" metric dimension is bounded. The first
// metricKeyLimit distinct keys are tagged verbatim; every further
// distinct key collapses to the "other" bucket, capping the dimension at
// metricKeyLimit+1 series regardless of input.
//
// Mutation killed: revert the emission sites to tag the raw key -- the
// distinct-key-value count below grows with the input and blows the
// bound.
// ═══════════════════════════════════════════════════════════════════

// distinctCBMetricKeys returns the set of distinct "key" tag values seen
// across every circuit-breaker counter recorded by rec.
func distinctCBMetricKeys(rec *ports.RecordingExporter) map[string]struct{} {
	out := map[string]struct{}{}
	for _, e := range rec.Entries() {
		if e.Kind != "counter" {
			continue
		}
		for _, tg := range e.Tags {
			if tg.Key == "key" {
				out[tg.Value] = struct{}{}
			}
		}
	}
	return out
}

// TestMetricKeyCardinality_BoundedUnderUntrustedKeys drives an unbounded
// number of distinct caller-controlled keys through StateChanged, Trips
// AND Rejections and asserts the emitted "key" dimension stays bounded.
func TestMetricKeyCardinality_BoundedUnderUntrustedKeys(t *testing.T) {
	rec := &ports.RecordingExporter{}
	const limit = 4
	// FailureThreshold 1: the first request per key trips it open
	// (StateChanged + Trips); a second request on the now-open key is
	// short-circuited (Rejections). ResetTimeout keeps it open.
	cfg := cb.Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("card-cb", cfg,
		WithMetrics(rec),
		WithKeyExtractor(HeaderKey("tenant-id")),
		WithMetricKeyCardinality(limit),
		// Keep the breaker cache large so eviction never hides the point;
		// the metric bound must hold independently of cache capacity.
		WithMaxBreakers(1_000_000),
	)

	fail := func(_ context.Context, _ *messaging.Envelope) error { return errors.New("downstream boom") }
	const distinctKeys = 200
	for i := 0; i < distinctKeys; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "s",
			Headers: map[string]any{"tenant-id": fmt.Sprintf("tenant-%d", i)},
		})
		_ = p.Process(context.Background(), env, fail) // trip
		_ = p.Process(context.Background(), env, fail) // reject (open)
	}

	keys := distinctCBMetricKeys(rec)
	if len(keys) == 0 {
		t.Fatal("no circuit-breaker metrics recorded; test cannot observe the bound")
	}
	if len(keys) > limit+1 {
		t.Fatalf("unbounded metric key cardinality: %d distinct key values (want <= %d): %v",
			len(keys), limit+1, keys)
	}
	// Not over-collapsed: the first distinct keys stay verbatim (observable),
	// only the overflow shares the "other" bucket.
	if _, ok := keys["tenant-0"]; !ok {
		t.Fatalf("expected the first key tagged verbatim, got %v", keys)
	}
	if _, ok := keys[metricKeyOverflow]; !ok {
		t.Fatalf("expected overflow keys to collapse to %q, got %v", metricKeyOverflow, keys)
	}
}

// TestMetricKey_BoundedSet pins the bounding primitive directly: verbatim
// up to the limit, "other" past it, and a previously-seen key stays
// verbatim even after the set is full.
func TestMetricKey_BoundedSet(t *testing.T) {
	p := New("unit", cb.Config{}, WithMetricKeyCardinality(3))

	for _, k := range []string{"a", "b", "c"} {
		if got := p.metricKey(k); got != k {
			t.Fatalf("metricKey(%q) = %q within limit, want verbatim", k, got)
		}
	}
	// Set is now full (3): any new key collapses.
	if got := p.metricKey("d"); got != metricKeyOverflow {
		t.Fatalf("metricKey(\"d\") past limit = %q, want %q", got, metricKeyOverflow)
	}
	if got := p.metricKey("e"); got != metricKeyOverflow {
		t.Fatalf("metricKey(\"e\") past limit = %q, want %q", got, metricKeyOverflow)
	}
	// Already-admitted keys remain verbatim (stable series for the trusted
	// working set).
	if got := p.metricKey("a"); got != "a" {
		t.Fatalf("previously-seen metricKey(\"a\") = %q, want verbatim", got)
	}
}

// TestMetricKey_DefaultLimit: with no override the default cardinality cap
// applies, and a non-positive override keeps it (WithMaxBreakers-style
// convention).
func TestMetricKey_DefaultLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"no option", nil},
		{"zero keeps default", []Option{WithMetricKeyCardinality(0)}},
		{"negative keeps default", []Option{WithMetricKeyCardinality(-7)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New("d", cb.Config{}, tc.opts...)
			if p.metricKeyLimit != defaultMetricKeyCardinality {
				t.Fatalf("metricKeyLimit = %d, want default %d", p.metricKeyLimit, defaultMetricKeyCardinality)
			}
			for i := 0; i < defaultMetricKeyCardinality; i++ {
				k := fmt.Sprintf("k-%d", i)
				if got := p.metricKey(k); got != k {
					t.Fatalf("key %d within default limit not verbatim: %q", i, got)
				}
			}
			if got := p.metricKey("one-too-many"); got != metricKeyOverflow {
				t.Fatalf("key beyond default limit = %q, want %q", got, metricKeyOverflow)
			}
		})
	}
}

// TestMetricKey_ConcurrentBounded runs metricKey from many goroutines to
// prove the bound and the map access are race-free (run under -race).
func TestMetricKey_ConcurrentBounded(t *testing.T) {
	const limit = 8
	p := New("conc", cb.Config{}, WithMetricKeyCardinality(limit))

	const goroutines = 32
	const perG = 500
	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < perG; i++ {
				_ = p.metricKey(fmt.Sprintf("g%d-k%d", g, i))
			}
		}(g)
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}

	p.metricKeyMu.Lock()
	size := len(p.metricKeys)
	p.metricKeyMu.Unlock()
	if size != limit {
		t.Fatalf("bounded set size = %d, want exactly the limit %d", size, limit)
	}
}

// ═══════════════════════════════════════════════════════════════════
// C10 review follow-ups: the "key" dimension must be MEMORY-bounded (not
// just count-bounded), and the overflow sentinel must be unambiguous.
// ═══════════════════════════════════════════════════════════════════

// TestNormalizeMetricKey_LengthCappedAndHashed: a raw key can be
// arbitrarily long (untrusted header), so an oversized key must be folded
// to a bounded, stable, UTF-8-valid label -- prefix + short hash -- that
// still distinguishes distinct long keys.
func TestNormalizeMetricKey_LengthCappedAndHashed(t *testing.T) {
	for _, k := range []string{"", "global", "tenant-42"} {
		if got := normalizeMetricKey(k); got != k {
			t.Fatalf("normalizeMetricKey(%q) = %q, want verbatim", k, got)
		}
	}

	long := strings.Repeat("A", 500)
	got := normalizeMetricKey(long)
	if len(got) > maxMetricKeyLen {
		t.Fatalf("normalized oversized key len = %d, want <= %d", len(got), maxMetricKeyLen)
	}
	if !strings.Contains(got, "#") {
		t.Fatalf("normalized oversized key %q missing hash separator", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("normalized label not valid UTF-8: %q", got)
	}
	if again := normalizeMetricKey(long); again != got {
		t.Fatalf("normalizeMetricKey not stable: %q vs %q", got, again)
	}
	// Distinct long keys differing only in the tail must not collapse: the
	// hash covers the whole key, not just the retained prefix.
	if other := normalizeMetricKey(strings.Repeat("A", 499) + "B"); other == got {
		t.Fatal("distinct oversized keys collapsed to the same label")
	}
	// A multi-byte rune straddling the byte-truncation boundary must not
	// corrupt UTF-8 ('é' is 2 bytes; 111-byte prefix budget lands mid-rune).
	if m := normalizeMetricKey(strings.Repeat("é", 300)); !utf8.ValidString(m) {
		t.Fatalf("multi-byte key normalized to invalid UTF-8: %q", m)
	}
}

// TestMetricKey_OversizedKeyBounded: metricKey must neither retain the raw
// oversized key verbatim nor emit a giant label.
func TestMetricKey_OversizedKeyBounded(t *testing.T) {
	p := New("oversize", cb.Config{}, WithMetricKeyCardinality(4))
	long := strings.Repeat("Z", 1000)

	label := p.metricKey(long)
	if len(label) > maxMetricKeyLen {
		t.Fatalf("emitted label len = %d, want <= %d", len(label), maxMetricKeyLen)
	}

	p.metricKeyMu.Lock()
	defer p.metricKeyMu.Unlock()
	if _, ok := p.metricKeys[long]; ok {
		t.Fatal("raw 1000-byte key retained verbatim in the bounded set")
	}
	if _, ok := p.metricKeys[label]; !ok {
		t.Fatalf("normalized label %q not stored", label)
	}
	for k := range p.metricKeys {
		if len(k) > maxMetricKeyLen {
			t.Fatalf("stored key exceeds cap: %d bytes", len(k))
		}
	}
}

// TestMetricKey_EmittedLabelLengthBounded is the end-to-end proof: a huge
// caller-controlled header value must NOT surface as a giant metric label.
func TestMetricKey_EmittedLabelLengthBounded(t *testing.T) {
	rec := &ports.RecordingExporter{}
	cfg := cb.Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("big-label", cfg, WithMetrics(rec), WithKeyExtractor(HeaderKey("tenant-id")))

	huge := strings.Repeat("q", 4096)
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "s",
		Headers: map[string]any{"tenant-id": huge},
	})
	_ = p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		return errors.New("boom")
	})

	seen := false
	for _, e := range rec.Entries() {
		for _, tg := range e.Tags {
			if tg.Key != "key" {
				continue
			}
			seen = true
			if len(tg.Value) > maxMetricKeyLen {
				t.Fatalf("emitted metric key label = %d bytes, want <= %d", len(tg.Value), maxMetricKeyLen)
			}
		}
	}
	if !seen {
		t.Fatal("no key-tagged circuit-breaker metric emitted")
	}
}

// TestMetricKey_SentinelCollisionFolded: a raw key that literally equals
// the reserved overflow sentinel is folded into the overflow bucket -- it
// consumes no slot and never masquerades as a distinct verbatim series --
// so the overflow bucket stays unambiguous.
func TestMetricKey_SentinelCollisionFolded(t *testing.T) {
	p := New("sentinel", cb.Config{}, WithMetricKeyCardinality(2))

	for i := 0; i < 10; i++ {
		if got := p.metricKey(metricKeyOverflow); got != metricKeyOverflow {
			t.Fatalf("metricKey(sentinel) = %q, want %q", got, metricKeyOverflow)
		}
	}
	p.metricKeyMu.Lock()
	size := len(p.metricKeys)
	p.metricKeyMu.Unlock()
	if size != 0 {
		t.Fatalf("reserved-sentinel key consumed %d set slots, want 0", size)
	}

	// The sentinel did not crowd out real keys: two still get verbatim labels
	// and the third overflows -- unambiguously into the same reserved bucket.
	if got := p.metricKey("real-a"); got != "real-a" {
		t.Fatalf("real-a = %q, want verbatim", got)
	}
	if got := p.metricKey("real-b"); got != "real-b" {
		t.Fatalf("real-b = %q, want verbatim", got)
	}
	if got := p.metricKey("real-c"); got != metricKeyOverflow {
		t.Fatalf("real-c past limit = %q, want overflow %q", got, metricKeyOverflow)
	}
}
