package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// SwapMode controls how the Supervisor transitions between runtimes
// during a reconfiguration.
type SwapMode int

const (
	// SwapOverlap builds the new runtime fully while the old is still
	// running, then stops old and starts new. Minimizes downtime.
	SwapOverlap SwapMode = iota

	// SwapPrepareCommit validates config and builds stores while old
	// runs, but defers session/receiver/sender creation until after
	// the old runtime stops. Safe for exclusive MQTT client-ids.
	SwapPrepareCommit

	// SwapAuto inspects the new config's sessions and their transport
	// factory capabilities. If any transport declares
	// CapExclusiveIdentity, PrepareCommit is used; otherwise Overlap.
	SwapAuto
)

// SwapEvent is emitted on each reconfiguration attempt via the
// OnSwap callback.
type SwapEvent struct {
	OldConfig *config.BridgeConfig
	NewConfig *config.BridgeConfig
	SwapMode  SwapMode
	Error     error
	Duration  time.Duration
}

// Supervisor manages the runtime lifecycle and applies new
// configurations by coordinating Stop-Rebuild-Start cycles. It
// supports pluggable ReconfigStrategy for debouncing and automatic
// SwapMode detection based on transport capabilities.
type Supervisor struct {
	mu         sync.RWMutex
	rt         *runtime.Runtime
	cfg        *config.BridgeConfig
	transports map[string]TransportFactory
	stores     map[string]StoreFactory
	processors map[string]ports.Processor
	credStore  ports.CredentialStore
	logger     *slog.Logger
	swapMode   SwapMode
	strategy   ReconfigStrategy
	onSwap     func(SwapEvent)
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*Supervisor)

// WithSupervisorLogger sets the logger for the supervisor and
// the runtimes it creates.
func WithSupervisorLogger(l *slog.Logger) SupervisorOption {
	return func(s *Supervisor) { s.logger = l }
}

// WithSwapMode overrides the automatic swap mode detection.
func WithSwapMode(m SwapMode) SupervisorOption {
	return func(s *Supervisor) { s.swapMode = m }
}

// WithReconfigStrategy sets the strategy that controls when config
// changes trigger a rebuild. Defaults to DirectStrategy.
func WithReconfigStrategy(rs ReconfigStrategy) SupervisorOption {
	return func(s *Supervisor) { s.strategy = rs }
}

// WithSupervisorCredentialStore sets the credential store passed to
// builders created by the supervisor.
func WithSupervisorCredentialStore(cs ports.CredentialStore) SupervisorOption {
	return func(s *Supervisor) { s.credStore = cs }
}

// WithOnSwap registers a callback invoked after each swap attempt.
func WithOnSwap(fn func(SwapEvent)) SupervisorOption {
	return func(s *Supervisor) { s.onSwap = fn }
}

// NewSupervisor creates a Supervisor with SwapAuto mode and
// DirectStrategy by default.
func NewSupervisor(opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		transports: make(map[string]TransportFactory),
		stores:     make(map[string]StoreFactory),
		processors: make(map[string]ports.Processor),
		swapMode:   SwapAuto,
		strategy:   NewDirectStrategy(),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RegisterTransport registers a transport factory for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
func (s *Supervisor) RegisterTransport(name string, factory TransportFactory) *Supervisor {
	s.mu.Lock()
	s.transports[name] = factory
	s.mu.Unlock()
	return s
}

// RegisterStoreFactory registers a store factory for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
func (s *Supervisor) RegisterStoreFactory(name string, factory StoreFactory) *Supervisor {
	s.mu.Lock()
	s.stores[name] = factory
	s.mu.Unlock()
	return s
}

// RegisterProcessor registers a named processor for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
func (s *Supervisor) RegisterProcessor(name string, proc ports.Processor) *Supervisor {
	s.mu.Lock()
	s.processors[name] = proc
	s.mu.Unlock()
	return s
}

// Runtime returns the currently active runtime, or nil if none is
// running. Safe for concurrent use.
func (s *Supervisor) Runtime() *runtime.Runtime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rt
}

// Config returns the currently active config, or nil if none has
// been applied. Safe for concurrent use.
func (s *Supervisor) Config() *config.BridgeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Run builds and starts the initial runtime, then watches the
// changes channel for new configs. It blocks until ctx is
// cancelled, the changes channel is closed, or an unrecoverable
// error occurs during initial startup. Config change failures are
// logged but do not stop Run.
func (s *Supervisor) Run(ctx context.Context, initial *config.BridgeConfig, changes <-chan *config.BridgeConfig) error {
	rt, err := s.buildRuntime(ctx, initial)
	if err != nil {
		return fmt.Errorf("supervisor: initial build: %w", err)
	}

	if err := rt.Start(ctx); err != nil {
		return fmt.Errorf("supervisor: initial start: %w", err)
	}

	s.mu.Lock()
	s.rt = rt
	s.cfg = initial
	s.mu.Unlock()

	if changes == nil {
		<-ctx.Done()
		return s.stopCurrent(ctx)
	}

	filtered := s.strategy.Filter(ctx, changes)

	for {
		select {
		case <-ctx.Done():
			return s.stopCurrent(ctx)
		case newCfg, ok := <-filtered:
			if !ok {
				return s.stopCurrent(ctx)
			}
			s.apply(ctx, newCfg)
		}
	}
}

func (s *Supervisor) apply(ctx context.Context, newCfg *config.BridgeConfig) {
	start := time.Now()
	mode := s.detectSwapMode(newCfg)

	s.mu.RLock()
	oldRt := s.rt
	oldCfg := s.cfg
	s.mu.RUnlock()

	var newRt *runtime.Runtime
	var err error

	switch mode {
	case SwapPrepareCommit:
		newRt, err = s.applyPrepareCommit(ctx, oldRt, oldCfg, newCfg)
	default:
		newRt, err = s.applyOverlap(ctx, oldRt, oldCfg, newCfg)
	}

	ev := SwapEvent{
		OldConfig: oldCfg,
		NewConfig: newCfg,
		SwapMode:  mode,
		Error:     err,
		Duration:  time.Since(start),
	}

	if err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: reconfiguration failed",
				"swap_mode", mode, "error", err, "duration", ev.Duration)
		}
	} else {
		s.mu.Lock()
		s.rt = newRt
		s.cfg = newCfg
		s.mu.Unlock()

		if s.logger != nil {
			s.logger.Info("supervisor: reconfiguration complete",
				"swap_mode", mode, "duration", ev.Duration)
		}
	}

	if s.onSwap != nil {
		s.safeOnSwap(ev)
	}
}

func (s *Supervisor) safeOnSwap(ev SwapEvent) {
	defer func() {
		if r := recover(); r != nil && s.logger != nil {
			s.logger.Error("supervisor: onSwap callback panicked", "panic", r)
		}
	}()
	s.onSwap(ev)
}

func (s *Supervisor) applyOverlap(
	ctx context.Context,
	oldRt *runtime.Runtime,
	oldCfg *config.BridgeConfig,
	newCfg *config.BridgeConfig,
) (*runtime.Runtime, error) {
	newRt, err := s.buildRuntime(ctx, newCfg)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}

	if oldRt != nil {
		drainTimeout := drainTimeoutFrom(oldCfg)
		stopCtx, cancel := context.WithTimeout(ctx, drainTimeout)
		if stopErr := oldRt.Stop(stopCtx); stopErr != nil && s.logger != nil {
			s.logger.Warn("supervisor: old runtime stop error", "error", stopErr)
		}
		cancel()
	}

	if err := newRt.Start(ctx); err != nil {
		s.mu.Lock()
		s.rt = nil
		s.cfg = oldCfg
		s.mu.Unlock()
		return nil, fmt.Errorf("start: %w", err)
	}

	return newRt, nil
}

func (s *Supervisor) applyPrepareCommit(
	ctx context.Context,
	oldRt *runtime.Runtime,
	oldCfg *config.BridgeConfig,
	newCfg *config.BridgeConfig,
) (*runtime.Runtime, error) {
	builder := s.newBuilder(newCfg)

	prep, err := builder.Prepare(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}

	if oldRt != nil {
		drainTimeout := drainTimeoutFrom(oldCfg)
		stopCtx, cancel := context.WithTimeout(ctx, drainTimeout)
		if stopErr := oldRt.Stop(stopCtx); stopErr != nil && s.logger != nil {
			s.logger.Warn("supervisor: old runtime stop error", "error", stopErr)
		}
		cancel()
	}

	newRt, err := builder.Complete(ctx, prep)
	if err != nil {
		s.mu.Lock()
		s.rt = nil
		s.cfg = oldCfg
		s.mu.Unlock()
		return nil, fmt.Errorf("complete: %w", err)
	}

	if err := newRt.Start(ctx); err != nil {
		s.mu.Lock()
		s.rt = nil
		s.cfg = oldCfg
		s.mu.Unlock()
		return nil, fmt.Errorf("start: %w", err)
	}

	return newRt, nil
}

func (s *Supervisor) buildRuntime(ctx context.Context, cfg *config.BridgeConfig) (*runtime.Runtime, error) {
	builder := s.newBuilder(cfg)
	return builder.Build(ctx)
}

func (s *Supervisor) newBuilder(cfg *config.BridgeConfig) *Builder {
	var opts []BuilderOption
	if s.logger != nil {
		opts = append(opts, WithLogger(s.logger))
	}
	if s.credStore != nil {
		opts = append(opts, WithCredentialStore(s.credStore))
	}
	b := NewBuilder(cfg, opts...)
	for name, tf := range s.transports {
		b.RegisterTransport(name, tf)
	}
	for name, sf := range s.stores {
		b.RegisterStoreFactory(name, sf)
	}
	for name, p := range s.processors {
		b.RegisterProcessor(name, p)
	}
	return b
}

func (s *Supervisor) detectSwapMode(cfg *config.BridgeConfig) SwapMode {
	if s.swapMode != SwapAuto {
		return s.swapMode
	}
	for _, sess := range cfg.Sessions {
		tf, ok := s.transports[sess.Transport]
		if !ok {
			continue
		}
		for _, cap := range tf.Capabilities() {
			if cap == ports.CapExclusiveIdentity {
				return SwapPrepareCommit
			}
		}
	}
	return SwapOverlap
}

func (s *Supervisor) stopCurrent(_ context.Context) error {
	s.mu.RLock()
	rt := s.rt
	cfg := s.cfg
	s.mu.RUnlock()

	if rt == nil {
		return nil
	}

	drainTimeout := drainTimeoutFrom(cfg)
	stopCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return rt.Stop(stopCtx)
}

func drainTimeoutFrom(cfg *config.BridgeConfig) time.Duration {
	if cfg == nil {
		return 30 * time.Second
	}
	return cfg.Bridge.DrainTimeoutDuration()
}
