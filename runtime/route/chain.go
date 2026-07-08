package route

import (
	"context"
	"fmt"
	"log/slog"
	goruntimedebug "runtime/debug"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// chainOptions tunes RunChain behavior: panic recovery, per-processor
// timeout, logger and metrics sink. Zero value is safe (defaults applied).
type chainOptions struct {
	logger        *slog.Logger
	metrics       ports.MetricsExporter
	timeout       time.Duration
	shutdownGrace time.Duration
	clk           clock.Clock
	routeID       string
	// root is the ORIGINAL caller context. Each processor frame runs under a
	// per-frame child (the caller's cancellation and any caller deadline still
	// propagate), but the per-processor timeout is enforced by an independent
	// budget timer — NOT a per-frame context deadline — so a slow processor 0
	// no longer caps the whole chain. root lets a frame tell a genuine shutdown
	// (root cancelled) apart from its own budget overrun, so a real
	// per-processor timeout is classified processor-timeout (and emits
	// MetricProcessorTimeouts) instead of being misread as shutdown-grace.
	root context.Context
	// outstanding counts processor goroutines spawned but not yet returned,
	// INCLUDING any abandoned on a timeout or shutdown-grace. RunChain refuses
	// the success-path merge while this is > 0: an OUTER processor may swallow an
	// inner ErrProcessorTimeout and return nil, but an abandoned inner goroutine
	// is still running (mutating only its own per-frame clone). Declaring that a
	// success would dispatch a half-processed message, so the chain returns a
	// transient timeout instead and the delivery is retried.
	outstanding *atomic.Int64
}

// ChainOption configures RunChain.
type ChainOption func(*chainOptions)

// WithChainLogger attaches a slog logger for panic / timeout reports.
// A nil logger is tolerated (no-op logging).
func WithChainLogger(logger *slog.Logger) ChainOption {
	return func(o *chainOptions) { o.logger = logger }
}

// WithChainMetrics attaches a metrics exporter so the chain can emit
// ProcessorPanics / ProcessorTimeouts counters.
func WithChainMetrics(m ports.MetricsExporter) ChainOption {
	return func(o *chainOptions) { o.metrics = m }
}

// WithChainTimeout overrides the per-processor execution timeout.
// Non-positive values fall back to routing.DefaultProcessorTimeout.
func WithChainTimeout(d time.Duration) ChainOption {
	return func(o *chainOptions) { o.timeout = d }
}

// WithChainShutdownGrace bounds how long invokeProcessor waits, after
// the parent context is cancelled, for a processor to observe the
// cancellation and unwind before the in-flight slot is reclaimed.
// Non-positive values fall back to a timeout-derived default.
func WithChainShutdownGrace(d time.Duration) ChainOption {
	return func(o *chainOptions) { o.shutdownGrace = d }
}

// WithChainClock injects the clock used to bound the shutdown-grace
// wait. A nil clock falls back to clock.System.
func WithChainClock(clk clock.Clock) ChainOption {
	return func(o *chainOptions) { o.clk = clk }
}

// WithChainRouteID tags emitted metrics with the owning route id.
func WithChainRouteID(id string) ChainOption {
	return func(o *chainOptions) { o.routeID = id }
}

// RunChain executes processors in order, each wrapping the next.
//
// Each processor runs under a deferred recover() and a per-processor
// context deadline. If the underlying processor panics the chain returns
// shared.ErrProcessorPanic (Permanent) without re-panicking. If the
// processor exceeds the configured timeout the chain returns
// shared.ErrProcessorTimeout (Transient).
//
// If processors is empty, returns nil immediately.
func RunChain(ctx context.Context, processors []ports.Processor, env *messaging.Envelope, opts ...ChainOption) error {
	if len(processors) == 0 {
		return nil
	}

	cfg := chainOptions{timeout: routing.DefaultProcessorTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.timeout <= 0 {
		cfg.timeout = routing.DefaultProcessorTimeout
	}
	if cfg.shutdownGrace <= 0 {
		cfg.shutdownGrace = defaultShutdownGrace(cfg.timeout)
	}
	if cfg.clk == nil {
		cfg.clk = clock.System
	}
	if cfg.metrics == nil {
		cfg.metrics = &ports.NoopExporter{}
	}
	cfg.root = ctx
	cfg.outstanding = new(atomic.Int64)

	var terminal ports.ProcessorFunc = func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	}

	chain := buildChain(processors, 0, terminal, &cfg)
	if err := chain(ctx, env); err != nil {
		return err
	}
	// Refuse the success path when a processor goroutine was abandoned on a
	// timeout / shutdown-grace and is still running: an OUTER "best-effort"
	// processor may have swallowed the inner ErrProcessorTimeout and returned
	// nil. The abandoned goroutine only ever mutates its own per-frame clone
	// (never chainEnv), so there is no data race — but merging chainEnv would
	// still dispatch a message the chain did not finish processing. Treat it as a
	// transient processing failure so the delivery is retried, never merged.
	if cfg.outstanding.Load() > 0 {
		return shared.ErrProcessorTimeout.
			With("reason", "processor-abandoned").
			With("envelope_id", env.ID())
	}
	return nil
}

func buildChain(processors []ports.Processor, index int, terminal ports.ProcessorFunc, cfg *chainOptions) ports.ProcessorFunc {
	if index >= len(processors) {
		return terminal
	}
	next := buildChain(processors, index+1, terminal, cfg)
	p := processors[index]
	return func(ctx context.Context, env *messaging.Envelope) error {
		return invokeProcessor(ctx, p, index, env, next, cfg)
	}
}

// invokeProcessor runs a single processor with panic recovery and a
// per-processor time budget. It returns sentinel domain errors on panic /
// timeout and otherwise passes through the processor's own return value.
//
// The per-processor budget measures the processor's OWN time: it is disarmed
// while the processor delegates to next() (so the outermost processor is not
// charged for the whole downstream chain) and rearmed when next() returns. A
// chain of N processors, each within the per-processor timeout, therefore
// succeeds even when their combined wall-clock time exceeds it. A processor that
// blows its own budget is classified processor-timeout (emitting
// MetricProcessorTimeouts); a genuine route shutdown is classified
// shutdown-grace and never counted as a processor timeout.
func invokeProcessor(
	ctx context.Context,
	p ports.Processor,
	index int,
	env *messaging.Envelope,
	next ports.ProcessorFunc,
	cfg *chainOptions,
) error {
	name := processorName(p, index)

	// Per-frame child context. It propagates the caller's cancellation (route
	// shutdown) and any caller deadline, but adds NO per-frame deadline of its
	// own — the per-processor timeout is the budget timer below, so processor 0
	// is not capped by time spent inside next().
	procCtx, cancelProc := context.WithCancel(ctx)
	defer cancelProc()

	done := make(chan struct{})
	var procErr error

	// frameEnv is THIS frame's private working clone. The processor mutates
	// frameEnv, never the caller's env; its processor-owned mutations (subject,
	// payload, headers) are merged back onto env ONLY once the goroutine has
	// cleanly returned (done observed) — never when it is abandoned on a timeout
	// or shutdown grace. An abandoned processor that ignores cancellation and
	// keeps writing headers is thereby confined to a clone no other frame
	// touches, so a sibling frame writing concurrently can never trigger a fatal
	// concurrent map write (finding 2, closed structurally rather than by
	// contract). Cost is one clone per frame per message; routes carry few
	// processors and the dispatch path already clones per message.
	frameEnv := env.Clone()

	// Fast path: the route is already shutting down. Do NOT arm a per-processor
	// budget (a processor overrun is meaningless once cancelled, and arming a
	// timer here would perturb the shutdown-grace timer accounting the shutdown
	// tests rely on). Cancel the child at once so a cancellation-honouring
	// processor unwinds immediately, then bound the wait by the shutdown grace.
	if cfg.root.Err() != nil {
		cancelProc()
		cfg.outstanding.Add(1)
		go runProcessor(procCtx, p, frameEnv, next, cfg, name, &procErr, done)
		return awaitShutdownGrace(ctx, done, &procErr, cfg, name, env, frameEnv)
	}

	// Steady state: the budget timer bounds the processor's OWN execution time.
	// wrappedNext disarms it for the duration of the delegation to next() and
	// rearms it afterwards, so only own-time is charged against the budget.
	budget := cfg.clk.NewTimer(cfg.timeout)
	defer budget.Stop()
	wrappedNext := func(nctx context.Context, nenv *messaging.Envelope) error {
		// Disarm the budget for the delegation to next() (only own-time is
		// charged), then rearm on return. If Stop reports the timer already
		// fired, drain the buffered tick before Reset so a stale tick cannot
		// survive the rearm and let the parent select below pick <-budget.C()
		// after the processor has in fact completed (a done message
		// misclassified as processor-timeout).
		//
		// ponytail: defensive, not load-bearing on this toolchain. Go 1.23+
		// timer channels are unbuffered (a fired-but-unreceived tick is not
		// retained across Reset), and the fake clock's Reset drains internally,
		// so neither production clock leaves a stale tick today. The drain is
		// kept for correctness under GODEBUG=asynctimerchan=1, pre-1.23
		// runtimes, and any non-time.Timer clock whose Reset does not drain; the
		// default: keeps it a no-op when C() is empty.
		if !budget.Stop() {
			select {
			case <-budget.C():
			default:
			}
		}
		defer budget.Reset(cfg.timeout)
		return next(nctx, nenv)
	}

	cfg.outstanding.Add(1)
	go runProcessor(procCtx, p, frameEnv, wrappedNext, cfg, name, &procErr, done)

	select {
	case <-done:
		// The processor goroutine has fully returned (its LIFO defers ran:
		// recover, outstanding--, close(done)), so frameEnv has no live writer.
		// Merge its processor-owned mutations back onto env single-threaded.
		mergeProcessedEnvelope(env, frameEnv)
		return procErr
	case <-budget.C():
		// The processor exceeded its OWN per-processor time budget. Cancel it so
		// a cancellation-honouring processor unwinds; otherwise the goroutine is
		// abandoned. It keeps writing ONLY frameEnv (this frame's private clone),
		// which is not merged on this path and which no other frame touches, so
		// an abandoned writer can never race a sibling frame's header write
		// (finding 2, closed structurally by the per-frame clone above).
		// outstanding stays > 0 so the caller also refuses the top-level merge.
		// Contract for third-party processors is unchanged but no longer
		// load-bearing for memory safety: honour cancellation, and do not mutate
		// the envelope after calling next().
		cancelProc()
		if cfg.root.Err() != nil {
			// Shutting down as well: do not emit a processor-timeout metric for a
			// message we are abandoning to shutdown.
			logShutdownGrace(ctx, cfg, name, env)
			return shared.ErrProcessorTimeout.
				With("processor", name).
				With("reason", "shutdown-grace-exceeded").
				With("grace", cfg.shutdownGrace.String()).
				With("envelope_id", env.ID())
		}
		logTimeout(ctx, cfg, name, env)
		emitTimeoutMetric(cfg)
		return shared.ErrProcessorTimeout.
			With("processor", name).
			With("reason", "processor-timeout").
			With("timeout", cfg.timeout.String()).
			With("envelope_id", env.ID())
	case <-cfg.root.Done():
		// Route shutting down mid-processing. Disarm the budget and give the
		// processor a bounded grace to observe cancellation and unwind before
		// abandoning it (distinct from a processor FAILURE).
		cancelProc()
		budget.Stop()
		return awaitShutdownGrace(ctx, done, &procErr, cfg, name, env, frameEnv)
	}
}

// runProcessor executes p.Process on its own goroutine, recovering panics into
// procErr, decrementing the outstanding counter, and finally closing done. The
// defer order is deliberate (LIFO): recover runs first (so a panic populates
// procErr), THEN outstanding is decremented, THEN done is closed — guaranteeing
// that once a waiter observes done closed, procErr is final and this goroutine
// no longer counts as outstanding.
func runProcessor(
	ctx context.Context,
	p ports.Processor,
	env *messaging.Envelope,
	next ports.ProcessorFunc,
	cfg *chainOptions,
	name string,
	procErr *error,
	done chan struct{},
) {
	defer close(done)
	defer cfg.outstanding.Add(-1)
	defer func() {
		if rv := recover(); rv != nil {
			*procErr = buildPanicError(name, rv, cfg, env)
		}
	}()
	*procErr = p.Process(ctx, env, next)
}

// awaitShutdownGrace waits for an already-cancelled processor to unwind, bounded
// by the shutdown grace. If it returns within the grace its own error is
// propagated (and its frame clone merged, single-threaded); if the grace elapses
// the processor is ignoring cancellation, so it is abandoned (its goroutine keeps
// outstanding > 0, and frameEnv is NOT merged — the abandoned writer stays
// confined to it) and a shutdown-grace-exceeded timeout is returned — distinct,
// via the reason tag, from a steady-state processor timeout so it is never
// counted as one.
func awaitShutdownGrace(
	ctx context.Context,
	done <-chan struct{},
	procErr *error,
	cfg *chainOptions,
	name string,
	env *messaging.Envelope,
	frameEnv *messaging.Envelope,
) error {
	graceTimer := cfg.clk.NewTimer(cfg.shutdownGrace)
	defer graceTimer.Stop()
	select {
	case <-done:
		mergeProcessedEnvelope(env, frameEnv)
		return *procErr
	case <-graceTimer.C():
		logShutdownGrace(ctx, cfg, name, env)
		return shared.ErrProcessorTimeout.
			With("processor", name).
			With("reason", "shutdown-grace-exceeded").
			With("grace", cfg.shutdownGrace.String()).
			With("envelope_id", env.ID())
	}
}

func buildPanicError(name string, rv any, cfg *chainOptions, env *messaging.Envelope) error {
	stack := goruntimedebug.Stack()
	if cfg.logger != nil {
		cfg.logger.Error("processor panicked",
			"processor", name,
			"route_id", cfg.routeID,
			"envelope_id", env.ID(),
			"recovered", rv,
			"stack", string(stack),
		)
	}
	if cfg.metrics != nil {
		tags := processorTags(cfg, name)
		cfg.metrics.Counter(shared.MetricProcessorPanics, 1, tags...)
	}
	// Wrap the recovered value so errors.Is(..., ErrProcessorPanic) works
	// and the cause is preserved for observability.
	return shared.ErrProcessorPanic.
		With("processor", name).
		With("envelope_id", env.ID()).
		Wrap(fmt.Errorf("panic: %v", rv))
}

func logTimeout(ctx context.Context, cfg *chainOptions, name string, env *messaging.Envelope) {
	if cfg.logger == nil {
		return
	}
	cfg.logger.ErrorContext(ctx, "processor timed out",
		"processor", name,
		"route_id", cfg.routeID,
		"envelope_id", env.ID(),
		"timeout", cfg.timeout.String(),
	)
}

// defaultShutdownGrace derives the bounded grace a processor gets to
// observe parent cancellation during shutdown. It is capped well below
// a large per-processor timeout so shutdown stays bounded, while never
// exceeding the timeout when that is already small.
func defaultShutdownGrace(timeout time.Duration) time.Duration {
	const maxGrace = 5 * time.Second
	if timeout > 0 && timeout < maxGrace {
		return timeout
	}
	return maxGrace
}

// logShutdownGrace reports a processor that ignored parent cancellation
// past the shutdown-grace window. The goroutine is abandoned (best
// effort) and the in-flight slot reclaimed.
func logShutdownGrace(ctx context.Context, cfg *chainOptions, name string, env *messaging.Envelope) {
	if cfg.logger == nil {
		return
	}
	cfg.logger.WarnContext(ctx, "processor ignored cancellation; abandoning after shutdown grace",
		"processor", name,
		"route_id", cfg.routeID,
		"envelope_id", env.ID(),
		"grace", cfg.shutdownGrace.String(),
	)
}

func emitTimeoutMetric(cfg *chainOptions) {
	if cfg.metrics == nil {
		return
	}
	cfg.metrics.Counter(shared.MetricProcessorTimeouts, 1, processorTags(cfg, "")...)
}

func processorTags(cfg *chainOptions, name string) []shared.Tag {
	tags := make([]shared.Tag, 0, 2)
	if cfg.routeID != "" {
		tags = append(tags, shared.Tag{Key: shared.TagKeyRouteID, Value: cfg.routeID})
	}
	if name != "" {
		tags = append(tags, shared.Tag{Key: "processor", Value: name})
	}
	return tags
}

func processorName(p ports.Processor, index int) string {
	if p == nil {
		return fmt.Sprintf("#%d", index)
	}
	if n := p.Name(); n != "" {
		return n
	}
	return fmt.Sprintf("#%d", index)
}
