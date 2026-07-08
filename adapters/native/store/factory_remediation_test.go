package nativestore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingExporter is a race-safe ports.MetricsExporter that counts Counter
// calls so the factory's metrics-threading path can be exercised.
type recordingExporter struct {
	mu       sync.Mutex
	counters map[string]int64
}

func newRecordingExporter() *recordingExporter {
	return &recordingExporter{counters: make(map[string]int64)}
}

func (r *recordingExporter) Counter(name string, value int64, _ ...shared.Tag) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] += value
}
func (r *recordingExporter) total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, v := range r.counters {
		n += v
	}
	return n
}

func (r *recordingExporter) Gauge(string, float64, ...shared.Tag)       {}
func (r *recordingExporter) Histogram(string, float64, ...shared.Tag)   {}
func (r *recordingExporter) Timer(string, time.Duration, ...shared.Tag) {}
func (r *recordingExporter) Flush(context.Context) error                { return nil }
func (r *recordingExporter) Close(context.Context) error                { return nil }

var _ ports.MetricsExporter = (*recordingExporter)(nil)

// TestSQLiteStoreFactory_NewDLQStore_RetentionWiring proves the factory threads
// SQLiteConfig.Retention into the DLQ store. A 2h-old entry written against a
// 1h window sweeps away (first Write always sweeps); with retention unset the
// same entry is retained. The wide 2h/1h margin keeps it deterministic without
// any sleep or clock injection, and the two sub-cases are each other's
// counterfactual.
func TestSQLiteStoreFactory_NewDLQStore_RetentionWiring(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	ctx := context.Background()
	oldEntry := func() routing.DLQEntry {
		return routing.NewDLQEntry(routing.DLQEntrySpec{
			ID:       "old",
			RouteID:  "r",
			Category: "timeout",
			FailedAt: time.Now().Add(-2 * time.Hour),
		})
	}

	t.Run("retention_enabled_sweeps", func(t *testing.T) {
		s, err := f.NewDLQStore(ctx, &nativestore.SQLiteConfig{Path: ":memory:", Retention: time.Hour})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if err := s.Write(ctx, oldEntry()); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := s.List(ctx, routing.DLQFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("retention=1h must sweep a 2h-old entry on write, got %d", len(got))
		}
	})

	t.Run("retention_disabled_retains", func(t *testing.T) {
		s, err := f.NewDLQStore(ctx, &nativestore.SQLiteConfig{Path: ":memory:"})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if err := s.Write(ctx, oldEntry()); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := s.List(ctx, routing.DLQFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("default (no retention) must retain the entry, got %d", len(got))
		}
	})
}

// TestSQLiteStoreFactory_NewOutboxStore_MetricsThreaded proves the factory
// accepts and threads a runtime MetricsExporter (nil-safe both ways). A healthy
// store emits nothing to the fatal-fault counter — the exporter is wired but
// silent — while the deep fault-emission teeth live in the sqliteoutbox package
// test. Constructing with a nil exporter must not panic or install a nil meter.
func TestSQLiteStoreFactory_NewOutboxStore_MetricsThreaded(t *testing.T) {
	f := nativestore.NewSQLiteStoreFactory()
	ctx := context.Background()
	cfg := &nativestore.SQLiteConfig{Path: ":memory:"}

	exp := newRecordingExporter()
	s, err := f.NewOutboxStore(ctx, cfg, ports.OutboxRuntimeOptions{Metrics: exp})
	if err != nil {
		t.Fatalf("new with exporter: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
	if got := exp.total(); got != 0 {
		t.Fatalf("healthy construction must not emit any counter, got %d", got)
	}

	// nil Metrics path (default no-op meter) must also construct cleanly.
	if _, err := f.NewOutboxStore(ctx, cfg, ports.OutboxRuntimeOptions{}); err != nil {
		t.Fatalf("new with nil exporter: %v", err)
	}
}
