package dlq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Router classifies errors and writes entries to a [ports.DLQStore].
// When started, [Router.Route] enqueues entries to an internal buffer and
// background workers drain them to the store asynchronously, avoiding
// blocking route-runner semaphore slots on slow DLQ writes.
type Router struct {
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

// Config configures the async DLQ router.
type Config struct {
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
	defaultBufferSize   = 1000
	defaultWriteTimeout = 30 * time.Second
	defaultEnqTimeout   = 5 * time.Second
	defaultWorkers      = 2
)

// New creates a DLQ router. If store is nil, [Router.Route] is a no-op.
// The router operates synchronously until [Router.Start] is called.
func New(store ports.DLQStore) *Router {
	return NewFromConfig(Config{Store: store})
}

// NewFromConfig creates a DLQ router with explicit configuration.
func NewFromConfig(cfg Config) *Router {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = defaultBufferSize
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.EnqTimeout <= 0 {
		cfg.EnqTimeout = defaultEnqTimeout
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkers
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
	return &Router{
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
func (r *Router) HasStore() bool {
	return r.store != nil
}

// SetTokenFn sets the function used to check lease validity before DLQ writes.
func (r *Router) SetTokenFn(fn func() (persistence.LeaseToken, bool)) {
	r.mu.Lock()
	r.tokenFn = fn
	r.mu.Unlock()
}

// Start launches background workers that drain the buffer to the store.
// Must be called before [Router.Route] for async behavior; without Start,
// Route writes synchronously.
func (r *Router) Start(ctx context.Context) {
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
func (r *Router) Close() {
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
// started, it writes synchronously.
//
// The address parameter is the transport destination address that was
// the target of the failed delivery (e.g. MQTT topic, SQS queue URL,
// AMQP routing key) on egress, or the source address on ingress. It
// is recorded on the DLQ entry so consumers can route or analyze
// failures by transport address without inspecting Envelope.Subject.
func (r *Router) Route(
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

func (r *Router) buildEntry(
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

func (r *Router) writeDirect(ctx context.Context, entry routing.DLQEntry) error {
	writeCtx, cancel := context.WithTimeout(ctx, r.writeTimeout)
	defer cancel()
	return r.store.Write(writeCtx, entry)
}

func (r *Router) runWorker(_ context.Context) {
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

// idFallbackCounter feeds the deterministic-but-unique fallback path in
// [generateID] when crypto/rand fails after composition (effectively
// impossible on supported OSes but kept as defence-in-depth so the DLQ
// router never panics on ID generation).
//
//nolint:gochecknoglobals // counter must outlive every call
var idFallbackCounter atomic.Uint64

// generateID returns a 32-hex-character DLQ entry identifier. It mirrors
// the runtime-wide convention used by other components but is duplicated
// here because the dlq package is a leaf and must not depend on its
// parent (which would create an import cycle once the parent imports
// this package).
func generateID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fallbackID()
	}
	return hex.EncodeToString(b)
}

func fallbackID() string {
	n := idFallbackCounter.Add(1)
	ts := uint64(clock.System.Now().UnixNano())
	return strconv.FormatUint(ts, 16) + "-" + strconv.FormatUint(n, 16)
}
