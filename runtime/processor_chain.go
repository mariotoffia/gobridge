package runtime

import (
	"context"
	"fmt"
	"log/slog"
	goruntimedebug "runtime/debug"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// chainOptions tunes RunChain behavior: panic recovery, per-processor
// timeout, logger and metrics sink. Zero value is safe (defaults applied).
type chainOptions struct {
	logger  *slog.Logger
	metrics ports.MetricsExporter
	timeout time.Duration
	routeID string
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
// Non-positive values fall back to domain.DefaultProcessorTimeout.
func WithChainTimeout(d time.Duration) ChainOption {
	return func(o *chainOptions) { o.timeout = d }
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

	cfg := chainOptions{timeout: domain.DefaultProcessorTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.timeout <= 0 {
		cfg.timeout = domain.DefaultProcessorTimeout
	}
	if cfg.metrics == nil {
		cfg.metrics = &ports.NoopExporter{}
	}

	var terminal ports.ProcessorFunc = func(_ context.Context, _ *messaging.Envelope) error {
		return nil
	}

	chain := buildChain(processors, 0, terminal, &cfg)
	return chain(ctx, env)
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
// per-call deadline. It returns sentinel domain errors on panic / timeout
// and otherwise passes through the processor's own return value.
func invokeProcessor(
	ctx context.Context,
	p ports.Processor,
	index int,
	env *messaging.Envelope,
	next ports.ProcessorFunc,
	cfg *chainOptions,
) (err error) {
	name := processorName(p, index)

	callCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	// The processor runs on the calling goroutine so recover() here catches
	// panics raised by Process itself. Blocking processors are bounded by
	// callCtx -- they are expected to honour ctx.Done(). If they do not,
	// we still return ErrProcessorTimeout to the caller once callCtx
	// expires; the runaway goroutine is leaked (best-effort) but the
	// in-flight slot is released.
	done := make(chan struct{})
	var procErr error
	go func() {
		defer close(done)
		defer func() {
			if rv := recover(); rv != nil {
				procErr = buildPanicError(name, rv, cfg, env)
			}
		}()
		procErr = p.Process(callCtx, env, next)
	}()

	select {
	case <-done:
		return procErr
	case <-callCtx.Done():
		// Distinguish between the chain's own timeout (DeadlineExceeded with
		// no parent cancellation) and upstream cancellation. Only the former
		// is a processor-timeout; a cancelled parent context means the route
		// is shutting down and we should let that propagate.
		if ctx.Err() != nil {
			// Parent cancelled. Wait briefly for the processor to observe
			// cancellation so we do not race with it, then return its error.
			<-done
			return procErr
		}
		// Chain-induced timeout.
		logTimeout(ctx, cfg, name, env)
		emitTimeoutMetric(cfg)
		// Do not wait for the runaway processor; the goroutine will exit when
		// callCtx propagates or be leaked if it ignores cancellation.
		return shared.ErrProcessorTimeout.
			With("processor", name).
			With("timeout", cfg.timeout.String()).
			With("envelope_id", env.ID)
	}
}

func buildPanicError(name string, rv any, cfg *chainOptions, env *messaging.Envelope) error {
	stack := goruntimedebug.Stack()
	if cfg.logger != nil {
		cfg.logger.Error("processor panicked",
			"processor", name,
			"route_id", cfg.routeID,
			"envelope_id", env.ID,
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
		With("envelope_id", env.ID).
		Wrap(fmt.Errorf("panic: %v", rv))
}

func logTimeout(ctx context.Context, cfg *chainOptions, name string, env *messaging.Envelope) {
	if cfg.logger == nil {
		return
	}
	cfg.logger.ErrorContext(ctx, "processor timed out",
		"processor", name,
		"route_id", cfg.routeID,
		"envelope_id", env.ID,
		"timeout", cfg.timeout.String(),
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
