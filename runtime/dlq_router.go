package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// DLQRouter classifies errors and writes entries to a DLQStore.
// When started, Route enqueues entries to an internal buffer and
// background workers drain them to the store asynchronously, avoiding
// blocking route runner semaphore slots on slow DLQ writes.
type DLQRouter struct {
	store        ports.DLQStore
	buffer       chan domain.DLQEntry
	bufferSize   int
	writeTimeout time.Duration
	enqTimeout   time.Duration
	workers      int
	wg           sync.WaitGroup
	logger       *slog.Logger
	metrics      ports.MetricsExporter
	started      bool
}

// DLQRouterConfig configures the async DLQ router.
type DLQRouterConfig struct {
	Store        ports.DLQStore
	BufferSize   int           // default 1000
	WriteTimeout time.Duration // per-write deadline, default 30s
	EnqTimeout   time.Duration // max wait for buffer space, default 5s
	Workers      int           // background writer goroutines, default 2
	Logger       *slog.Logger
	Metrics      ports.MetricsExporter
}

const (
	dlqDefaultBufferSize   = 1000
	dlqDefaultWriteTimeout = 30 * time.Second
	dlqDefaultEnqTimeout   = 5 * time.Second
	dlqDefaultWorkers      = 2
)

// NewDLQRouter creates a DLQ router. If store is nil, Route is a no-op.
// The router operates synchronously until Start is called.
func NewDLQRouter(store ports.DLQStore) *DLQRouter {
	return NewDLQRouterFromConfig(DLQRouterConfig{Store: store})
}

// NewDLQRouterFromConfig creates a DLQ router with explicit configuration.
func NewDLQRouterFromConfig(cfg DLQRouterConfig) *DLQRouter {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = dlqDefaultBufferSize
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = dlqDefaultWriteTimeout
	}
	if cfg.EnqTimeout <= 0 {
		cfg.EnqTimeout = dlqDefaultEnqTimeout
	}
	if cfg.Workers <= 0 {
		cfg.Workers = dlqDefaultWorkers
	}
	m := cfg.Metrics
	if m == nil {
		m = &ports.NoopExporter{}
	}
	return &DLQRouter{
		store:        cfg.Store,
		bufferSize:   cfg.BufferSize,
		writeTimeout: cfg.WriteTimeout,
		enqTimeout:   cfg.EnqTimeout,
		workers:      cfg.Workers,
		logger:       cfg.Logger,
		metrics:      m,
	}
}

// HasStore returns true if a DLQ store is configured.
func (r *DLQRouter) HasStore() bool {
	return r.store != nil
}

// Start launches background workers that drain the buffer to the store.
// Must be called before Route for async behavior; without Start, Route
// writes synchronously (backward-compatible).
func (r *DLQRouter) Start(ctx context.Context) {
	if r.store == nil {
		return
	}
	r.buffer = make(chan domain.DLQEntry, r.bufferSize)
	r.started = true
	for range r.workers {
		r.wg.Add(1)
		go r.runWorker(ctx)
	}
}

// Close drains remaining buffer entries and waits for workers to finish.
func (r *DLQRouter) Close() {
	if !r.started {
		return
	}
	close(r.buffer)
	r.wg.Wait()
	r.started = false
}

// Route sends a failed envelope to the DLQ. When started, this enqueues
// to the internal buffer (non-blocking up to EnqTimeout). When not
// started, it writes synchronously (backward-compatible).
func (r *DLQRouter) Route(
	ctx context.Context,
	env *domain.Envelope,
	routeID, bindingID, sessionID, sourceID string,
	err error,
	attempts int,
) error {
	if r.store == nil {
		return nil
	}

	entry := r.buildEntry(env, routeID, bindingID, sessionID, sourceID, err, attempts)

	if !r.started {
		return r.writeDirect(ctx, entry)
	}

	select {
	case r.buffer <- entry:
		return nil
	case <-time.After(r.enqTimeout):
		r.metrics.Counter(domain.MetricDLQBufferOverflow, 1)
		return fmt.Errorf("DLQ buffer full after %s", r.enqTimeout)
	}
}

func (r *DLQRouter) buildEntry(
	env *domain.Envelope,
	routeID, bindingID, sessionID, sourceID string,
	err error,
	attempts int,
) domain.DLQEntry {
	category, errorCode := classifyError(err)
	correlationID, _ := domain.GetHeaderString(env.Headers, domain.HeaderCorrelationID)
	reason := safeErrorReason(err)

	return domain.DLQEntry{
		ID:            generateID(),
		Envelope:      *env,
		RouteID:       routeID,
		BindingID:     bindingID,
		SessionID:     sessionID,
		SourceID:      sourceID,
		CorrelationID: correlationID,
		Reason:        reason,
		Category:      category,
		ErrorCode:     errorCode,
		LastError:     reason,
		FailedAt:      time.Now(),
		Attempts:      attempts,
	}
}

func (r *DLQRouter) writeDirect(ctx context.Context, entry domain.DLQEntry) error {
	writeCtx, cancel := context.WithTimeout(ctx, r.writeTimeout)
	defer cancel()
	return r.store.Write(writeCtx, entry)
}

func (r *DLQRouter) runWorker(ctx context.Context) {
	defer r.wg.Done()
	for entry := range r.buffer {
		writeCtx, cancel := context.WithTimeout(ctx, r.writeTimeout)
		if err := r.store.Write(writeCtx, entry); err != nil {
			r.metrics.Counter(domain.MetricDLQWriteFailures, 1)
			if r.logger != nil {
				r.logger.Error("DLQ write failed",
					"entry_id", entry.ID,
					"route_id", entry.RouteID,
					"error", err,
				)
			}
		}
		cancel()
	}
}

// safeErrorReason returns a sanitized error reason suitable for persistence.
func safeErrorReason(err error) string {
	be, ok := domain.AsBridgeError(err)
	if ok {
		return be.Message
	}
	return "internal error"
}

func classifyError(err error) (category string, code string) {
	be, ok := domain.AsBridgeError(err)
	if !ok {
		return "unknown", ""
	}
	return string(be.Class), string(be.Code)
}
