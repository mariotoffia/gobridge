package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// slowFlushExporter models a metrics exporter whose Flush is slow-but-successful
// (it consumes the whole flush budget, then returns nil) and records whether
// Close was ever called. The runtime is a SHARED-exporter borrower: it must
// Flush buffered data on Stop but must NOT Close the exporter (the composition
// root owns Close). See.
type slowFlushExporter struct {
	ports.NoopExporter
	mu          sync.Mutex
	flushCalled bool
	closeCalled bool
}

func (e *slowFlushExporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	e.flushCalled = true
	e.mu.Unlock()
	// Consume the ENTIRE flush budget: block until the flush ctx deadline
	// fires, then report success (a slow flush, not a failing one).
	<-ctx.Done()
	return nil
}

func (e *slowFlushExporter) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closeCalled = true
	e.mu.Unlock()
	return nil
}

// TestStop_FlushesButDoesNotCloseSharedExporter is the regression guard for
// the metrics exporter (tracer) are SHARED by every runtime
// across config reloads and are owned by the composition root. runtime.Stop must
// FLUSH buffered data on every stop but must NOT Close the exporter — a
// per-runtime Close killed the shared CloudWatch flush goroutine on the FIRST
// reload, silently dropping all later metrics for the process lifetime.
func TestStop_FlushesButDoesNotCloseSharedExporter(t *testing.T) {
	exp := &slowFlushExporter{}

	rt := goruntime.New(
		goruntime.WithInstanceID("critical-2"),
		goruntime.WithMetrics(exp),
		goruntime.WithShutdownTimeout(200*time.Millisecond),
	)

	require.NoError(t, rt.Start(context.Background()))

	// Caller ctx is never cancelled: a clean shutdown must not error.
	err := rt.Stop(context.Background())
	require.NoError(t, err, "clean shutdown must not error")

	exp.mu.Lock()
	defer exp.mu.Unlock()
	assert.True(t, exp.flushCalled, "Flush must be invoked during Stop so buffered data flushes")
	assert.False(t, exp.closeCalled,
		"Stop must NOT Close a shared exporter — the composition root owns Close")
}
