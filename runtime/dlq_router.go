package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DLQRouter classifies errors and writes entries to a DLQStore.
// When started, Route enqueues entries to an internal buffer and
// background workers drain them to the store asynchronously, avoiding
// blocking route runner semaphore slots on slow DLQ writes.
type DLQRouter struct {
	store             ports.DLQStore
	buffer            chan routing.DLQEntry
	bufferSize        int
	writeTimeout      time.Duration
	enqTimeout        time.Duration
	workers           int
	wg                sync.WaitGroup
	logger            *slog.Logger
	metrics           ports.MetricsExporter
	clk               clock.Clock
	mu                sync.Mutex // guards started, stopped, and sendWg.Add
	started           bool
	stopped           bool
	done              chan struct{}  // closed by Close() to signal Route() to exit select
	sendWg            sync.WaitGroup // tracks goroutines in the Route select
	tokenFn           func() (persistence.LeaseToken, bool)
	writeMaxAttempts  int
	writeRetryBackoff routing.BackoffPolicy
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
	Clock        clock.Clock

	WriteMaxAttempts  int
	WriteRetryBackoff routing.BackoffPolicy
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
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System
	}
	if cfg.WriteMaxAttempts <= 0 {
		cfg.WriteMaxAttempts = 3
	}
	if cfg.WriteRetryBackoff.InitialInterval <= 0 {
		cfg.WriteRetryBackoff.InitialInterval = 500 * time.Millisecond
	}
	if cfg.WriteRetryBackoff.MaxInterval <= 0 {
		cfg.WriteRetryBackoff.MaxInterval = 5 * time.Second
	}
	if cfg.WriteRetryBackoff.Multiplier <= 0 {
		cfg.WriteRetryBackoff.Multiplier = 2.0
	}
	return &DLQRouter{
		store:             cfg.Store,
		bufferSize:        cfg.BufferSize,
		writeTimeout:      cfg.WriteTimeout,
		enqTimeout:        cfg.EnqTimeout,
		workers:           cfg.Workers,
		logger:            cfg.Logger,
		metrics:           m,
		clk:               clk,
		writeMaxAttempts:  cfg.WriteMaxAttempts,
		writeRetryBackoff: cfg.WriteRetryBackoff,
	}
}

// HasStore returns true if a DLQ store is configured.
func (r *DLQRouter) HasStore() bool {
	return r.store != nil
}

// SetTokenFn sets the function used to check lease validity before DLQ writes.
func (r *DLQRouter) SetTokenFn(fn func() (persistence.LeaseToken, bool)) {
	r.mu.Lock()
	r.tokenFn = fn
	r.mu.Unlock()
}

// Start launches background workers that drain the buffer to the store.
// Must be called before Route for async behavior; without Start, Route
// writes synchronously (backward-compatible).
func (r *DLQRouter) Start(ctx context.Context) {
	if r.store == nil {
		return
	}
	r.mu.Lock()
	r.buffer = make(chan routing.DLQEntry, r.bufferSize)
	r.started = true
	r.stopped = false
	r.done = make(chan struct{})
	r.mu.Unlock()
	for range r.workers {
		r.wg.Add(1)
		go r.runWorker(ctx)
	}
}

// Close drains remaining buffer entries and waits for workers to finish.
func (r *DLQRouter) Close() {
	r.mu.Lock()
	if !r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	close(r.done) // signal Route() goroutines to exit select
	r.mu.Unlock()

	r.sendWg.Wait() // wait for all Route() calls to exit select
	close(r.buffer) // safe: no Route() is sending
	r.wg.Wait()     // wait for workers to drain remaining entries
	r.started = false
}

// Route sends a failed envelope to the DLQ. When started, this enqueues
// to the internal buffer (non-blocking up to EnqTimeout). When not
// started, it writes synchronously (backward-compatible).
//
// The address parameter is the transport destination address that was
// the target of the failed delivery (e.g. MQTT topic, SQS queue URL,
// AMQP routing key) on egress, or the source address on ingress. It
// is recorded on the DLQ entry so consumers can route or analyze
// failures by transport address without inspecting Envelope.Subject.
func (r *DLQRouter) Route(
	ctx context.Context,
	env *messaging.Envelope,
	routeID, bindingID, address, sessionID, sourceID string,
	err error,
	attempts int,
) error {
	if r.store == nil {
		return nil
	}

	entry := r.buildEntry(env, routeID, bindingID, address, sessionID, sourceID, err, attempts)

	r.mu.Lock()
	if r.stopped || !r.started {
		r.mu.Unlock()
		return r.writeDirect(ctx, entry)
	}
	r.sendWg.Add(1) // under lock: Close() can't set stopped between check and Add
	r.mu.Unlock()
	defer r.sendWg.Done()

	timer := r.clk.NewTimer(r.enqTimeout)
	defer timer.Stop()

	select {
	case r.buffer <- entry:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return r.writeDirect(ctx, entry)
	case <-timer.C():
		r.metrics.Counter(shared.MetricDLQBufferOverflow, 1)
		return fmt.Errorf("DLQ buffer full after %s", r.enqTimeout)
	}
}

func (r *DLQRouter) buildEntry(
	env *messaging.Envelope,
	routeID, bindingID, address, sessionID, sourceID string,
	err error,
	attempts int,
) routing.DLQEntry {
	category, errorCode := classifyError(err)
	correlationID, _ := messaging.GetHeaderString(env.Headers(), messaging.HeaderCorrelationID)
	reason := safeErrorReason(err)

	return routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:            generateID(),
		Envelope:      *env,
		RouteID:       routeID,
		BindingID:     bindingID,
		Address:       address,
		SessionID:     sessionID,
		SourceID:      sourceID,
		CorrelationID: correlationID,
		Reason:        reason,
		Category:      category,
		ErrorCode:     errorCode,
		LastError:     reason,
		FailedAt:      r.clk.Now(),
		Attempts:      attempts,
	})
}

func (r *DLQRouter) writeDirect(ctx context.Context, entry routing.DLQEntry) error {
	writeCtx, cancel := context.WithTimeout(ctx, r.writeTimeout)
	defer cancel()
	return r.store.Write(writeCtx, entry)
}

func (r *DLQRouter) runWorker(_ context.Context) {
	defer r.wg.Done()
	for entry := range r.buffer {
		if r.tokenFn != nil {
			if _, hasLease := r.tokenFn(); !hasLease {
				if r.logger != nil {
					r.logger.Warn("DLQ write skipped, lease not held",
						"entry_id", entry.ID,
						"route_id", entry.RouteID,
					)
				}
				r.metrics.Counter(shared.MetricDLQWriteFailures, 1)
				continue
			}
		}

		maxAttempts := r.writeMaxAttempts
		delay := r.writeRetryBackoff.InitialInterval
		var writeErr error
		for attempt := 0; attempt < maxAttempts; attempt++ {
			if attempt > 0 {
				select {
				case <-r.done:
					return
				case <-r.clk.After(delay):
				}
				delay = time.Duration(float64(delay) * r.writeRetryBackoff.Multiplier)
				if delay > r.writeRetryBackoff.MaxInterval {
					delay = r.writeRetryBackoff.MaxInterval
				}
			}
			writeCtx, cancel := context.WithTimeout(context.Background(), r.writeTimeout)
			writeErr = r.store.Write(writeCtx, entry)
			cancel()
			if writeErr == nil {
				break
			}
		}
		if writeErr != nil {
			r.metrics.Counter(shared.MetricDLQWriteFailures, 1)
			if r.logger != nil {
				r.logger.Error("DLQ write failed after retries",
					"entry_id", entry.ID,
					"route_id", entry.RouteID,
					"error", writeErr,
					"attempts", maxAttempts,
				)
			}
		}
	}
}

// safeErrorReason returns a sanitized error reason suitable for persistence.
func safeErrorReason(err error) string {
	be, ok := shared.AsBridgeError(err)
	if ok {
		return be.Message
	}
	return "internal error"
}

func classifyError(err error) (category string, code string) {
	be, ok := shared.AsBridgeError(err)
	if !ok {
		return "unknown", ""
	}
	return string(be.Class), string(be.Code)
}
