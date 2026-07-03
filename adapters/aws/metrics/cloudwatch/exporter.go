package cloudwatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// flushRetryBaseBackoff is the initial suppression window applied to
// buffer-full flush triggers after a failed flush. It doubles per
// consecutive failure up to FlushInterval, so a stalled CloudWatch
// endpoint is not hammered by threshold triggers (the periodic ticker
// still flushes on its own cadence).
const flushRetryBaseBackoff = time.Second

// Exporter implements [ports.MetricsExporter] for AWS CloudWatch.
//
// Concurrency model (MF-1): all flushing is performed by ONE
// long-lived flusher goroutine. The emission path (Counter, Gauge,
// Histogram, Timer) only appends to the in-memory batcher and — when
// the flush-trigger threshold is reached — performs a non-blocking
// send on a 1-slot channel. It never spawns goroutines and never
// blocks the caller, so a slow or stalled CloudWatch endpoint cannot
// turn the observability path into a data-path OOM. The batcher's
// hard cap (MaxBufferedDatums) bounds memory during a stall;
// overflow is dropped and counted.
type Exporter struct {
	config    Config
	client    cloudWatchAPI
	batcher   *batcher
	stopCh    chan struct{}
	flushCh   chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	closeOnce sync.Once
	loggerSet bool
}

var _ ports.MetricsExporter = (*Exporter)(nil)

// New creates a new CloudWatch metrics exporter. The namespace is used
// as the CloudWatch metric namespace (e.g. shared.MetricNamespace,
// "GoBridge/Runtime").
func New(ctx context.Context, namespace string, opts ...Option) (*Exporter, error) {
	e := &Exporter{
		config:  Config{Namespace: namespace},
		stopCh:  make(chan struct{}),
		flushCh: make(chan struct{}, 1),
	}

	for _, opt := range opts {
		opt(e)
	}
	applyDefaults(&e.config)
	if e.config.Logger == nil && !e.loggerSet {
		// Self-loss and export failures must not be silent by default
		// (MF-5). Opt out with WithLogger(nil).
		e.config.Logger = slog.Default()
	}

	if e.client == nil {
		client, err := newCloudWatchClient(ctx, e.config)
		if err != nil {
			return nil, fmt.Errorf("cloudwatch: %w", err)
		}
		e.client = client
	}

	e.batcher = newBatcher(e.config)

	e.wg.Add(1)
	go e.flushLoop()

	return e, nil
}

func (e *Exporter) Counter(name string, value int64, tags ...shared.Tag) {
	if e.batcher.addCounter(name, value, tags) {
		e.triggerFlush()
	}
}

func (e *Exporter) Gauge(name string, value float64, tags ...shared.Tag) {
	if e.batcher.addGauge(name, value, tags) {
		e.triggerFlush()
	}
}

func (e *Exporter) Histogram(name string, value float64, tags ...shared.Tag) {
	if e.batcher.addHistogram(name, value, tags) {
		e.triggerFlush()
	}
}

func (e *Exporter) Timer(name string, duration time.Duration, tags ...shared.Tag) {
	if e.batcher.addTimer(name, duration, tags) {
		e.triggerFlush()
	}
}

// triggerFlush wakes the flusher goroutine without blocking or
// spawning (MF-1). The 1-slot channel coalesces bursts: while a flush
// is pending or in progress, additional triggers are no-ops.
func (e *Exporter) triggerFlush() {
	select {
	case e.flushCh <- struct{}{}:
	default:
	}
}

// Flush sends all buffered metrics to CloudWatch immediately.
func (e *Exporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.batcher.flush(ctx, e.client, e.config.Namespace, e.config.MaxBatchSize, e.config.MaxBatchBytes)
}

// Close stops the background flush loop and performs a final flush.
// It is safe to call multiple times.
func (e *Exporter) Close(ctx context.Context) error {
	var err error
	e.closeOnce.Do(func() {
		close(e.stopCh)
		e.wg.Wait()
		err = e.Flush(ctx)
	})
	return err
}

// flushLoop is the single long-lived flusher goroutine (MF-1). It
// flushes on the periodic ticker and on buffer-full triggers. After a
// retryable failure, trigger-driven flushes are suppressed for an
// exponentially growing backoff (capped at FlushInterval) so a
// stalled endpoint is not hammered; the ticker cadence still applies.
// Non-retryable failures already dropped the offending batch inside
// the batcher (MF-3), so no backoff is applied for them.
func (e *Exporter) flushLoop() {
	defer e.wg.Done()
	ticker := e.config.Clock.NewTicker(e.config.FlushInterval)
	defer ticker.Stop()

	gov := newFlushGovernor(e.config.Clock, e.config.FlushInterval)

	flush := func() {
		ctx, cancel := context.WithTimeout(context.Background(), e.config.FlushRPCTimeout)
		defer cancel()
		err := e.Flush(ctx)
		retryable := gov.observe(err)
		if err == nil || e.config.Logger == nil {
			return
		}
		if retryable {
			e.config.Logger.Warn("cloudwatch: flush failed; samples requeued for retry",
				slog.Duration("trigger_backoff", gov.backoff),
				slog.String("error", err.Error()))
			return
		}
		// Offending batch already dropped and counted; nothing to
		// retry, so subsequent triggers are not suppressed.
		e.config.Logger.Warn("cloudwatch: flush dropped invalid metric batch",
			slog.String("error", err.Error()))
	}

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C():
			flush()
		case <-e.flushCh:
			if gov.allowTrigger() {
				flush()
			}
		}
	}
}

// flushGovernor arbitrates trigger-driven flushes after failures
// (MF-1). It is owned exclusively by the flusher goroutine — no
// locking. Kept separate from flushLoop so the suppression policy is
// synchronously testable with a fake clock.
type flushGovernor struct {
	clk        clock.Clock
	maxBackoff time.Duration
	backoff    time.Duration
	retryAfter time.Time
	suppressed bool
}

func newFlushGovernor(clk clock.Clock, maxBackoff time.Duration) *flushGovernor {
	return &flushGovernor{clk: clk, maxBackoff: maxBackoff, backoff: flushRetryBaseBackoff}
}

// allowTrigger reports whether a buffer-full trigger may flush now.
// Suppression applies only while the backoff window armed by the last
// retryable failure has not yet elapsed.
func (g *flushGovernor) allowTrigger() bool {
	return !g.suppressed || !g.clk.Now().Before(g.retryAfter)
}

// observe records a flush outcome and reports whether the failure was
// retryable (i.e. suppression was armed). Success resets the backoff;
// a permanent failure neither suppresses nor resets (the offending
// batch was already dropped by the batcher); a retryable failure arms
// the window and doubles the backoff up to maxBackoff.
func (g *flushGovernor) observe(err error) bool {
	switch {
	case err == nil:
		g.backoff = flushRetryBaseBackoff
		g.suppressed = false
		return false
	case isPermanentPutError(err):
		return false
	default:
		g.suppressed = true
		g.retryAfter = g.clk.Now().Add(g.backoff)
		if g.backoff < g.maxBackoff {
			g.backoff = min(g.backoff*2, g.maxBackoff)
		}
		return true
	}
}
