package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
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
	OldConfig *ports.BridgeConfig
	NewConfig *ports.BridgeConfig
	SwapMode  SwapMode
	Error     error
	Duration  time.Duration
}

// Supervisor manages the runtime lifecycle and applies new
// configurations by coordinating Stop-Rebuild-Start cycles. It
// supports pluggable ReconfigStrategy for debouncing and automatic
// SwapMode detection based on transport capabilities.
type Supervisor struct {
	mu                           sync.RWMutex
	rt                           *runtime.Runtime
	cfg                          *ports.BridgeConfig
	transports                   map[string]ports.TransportFactory
	stores                       map[string]ports.StoreFactory
	processors                   map[string]ports.Processor
	credStore                    ports.CredentialStore
	pushCredStore                ports.PushCredentialStore
	pollCredStore                ports.PullCredentialStore
	pollCredConfig               ports.PollBasedWrapperConfig
	logger                       *slog.Logger
	clk                          clock.Clock
	swapMode                     SwapMode
	strategy                     ReconfigStrategy
	onSwap                       func(SwapEvent)
	defaultDrainTimeout          time.Duration
	defaultPerRecordDrainTimeout time.Duration
	defaultMaxDrainTimeout       time.Duration
	validator                    ports.BlueprintValidator
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*Supervisor)

// WithSupervisorLogger sets the logger for the supervisor and
// the runtimes it creates.
func WithSupervisorLogger(l *slog.Logger) SupervisorOption {
	return func(s *Supervisor) { s.logger = l }
}

// WithSupervisorClock sets the clock used for swap timing.
func WithSupervisorClock(c clock.Clock) SupervisorOption {
	return func(s *Supervisor) {
		if c != nil {
			s.clk = c
		}
	}
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

// WithSupervisorCredentialStore sets the pull-style credential store
// passed to builders created by the supervisor.
func WithSupervisorCredentialStore(cs ports.CredentialStore) SupervisorOption {
	return func(s *Supervisor) { s.credStore = cs }
}

// WithSupervisorPushCredentialStore registers a push-style credential
// store that emits rotation events. The supervisor propagates it to
// the builders so CredentialRefresher can bind watchers.
func WithSupervisorPushCredentialStore(cs ports.PushCredentialStore) SupervisorOption {
	return func(s *Supervisor) { s.pushCredStore = cs }
}

// WithSupervisorPolledCredentialStore wires a pull store and adaption
// config together so the builder can lift a pull store into a push
// store via runtime.NewPollBasedWrapper.
func WithSupervisorPolledCredentialStore(cs ports.PullCredentialStore, cfg ports.PollBasedWrapperConfig) SupervisorOption {
	return func(s *Supervisor) {
		s.pollCredStore = cs
		s.pollCredConfig = cfg
	}
}

// WithOnSwap registers a callback invoked after each swap attempt.
func WithOnSwap(fn func(SwapEvent)) SupervisorOption {
	return func(s *Supervisor) { s.onSwap = fn }
}

// WithDefaultDrainTimeout sets the fallback drain timeout used when the
// BridgeConfig does not specify one. Defaults to 30s.
func WithDefaultDrainTimeout(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.defaultDrainTimeout = d }
}

// WithDefaultPerRecordDrainTimeout sets the fallback per-record drain
// timeout used by the scaled drain formula when the BridgeConfig does
// not specify one. Zero means use the legacy fixed DrainTimeout.
func WithDefaultPerRecordDrainTimeout(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.defaultPerRecordDrainTimeout = d }
}

// WithDefaultMaxDrainTimeout sets the fallback max drain timeout used
// by the scaled drain formula when the BridgeConfig does not specify
// one. Zero means use the legacy fixed DrainTimeout.
func WithDefaultMaxDrainTimeout(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.defaultMaxDrainTimeout = d }
}

// WithSupervisorBlueprintValidator injects a config validator that
// the supervisor passes to every Builder it creates. Composition
// roots that load config from a YAML/JSON file should supply
// config.Validate so invalid configs are rejected before any
// transports are constructed.
func WithSupervisorBlueprintValidator(v ports.BlueprintValidator) SupervisorOption {
	return func(s *Supervisor) { s.validator = v }
}

// NewSupervisor creates a Supervisor with SwapAuto mode and
// DirectStrategy by default.
func NewSupervisor(opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		transports: make(map[string]ports.TransportFactory),
		stores:     make(map[string]ports.StoreFactory),
		processors: make(map[string]ports.Processor),
		swapMode:   SwapAuto,
		strategy:   NewDirectStrategy(),
		clk:        clock.System,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RegisterTransport registers a transport factory for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
func (s *Supervisor) RegisterTransport(name string, factory ports.TransportFactory) *Supervisor {
	s.mu.Lock()
	s.transports[name] = factory
	s.mu.Unlock()
	return s
}

// RegisterStoreFactory registers a store factory for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
func (s *Supervisor) RegisterStoreFactory(name string, factory ports.StoreFactory) *Supervisor {
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
func (s *Supervisor) Config() *ports.BridgeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Run builds and starts the initial runtime, then watches the
// changes channel for new configs. It blocks until ctx is
// cancelled, the changes channel is closed, or an unrecoverable
// error occurs during initial startup. Config change failures are
// logged but do not stop Run.
func (s *Supervisor) Run(ctx context.Context, initial *ports.BridgeConfig, changes <-chan *ports.BridgeConfig) error {
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

func (s *Supervisor) apply(ctx context.Context, newCfg *ports.BridgeConfig) {
	start := s.clk.Now()
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
		Duration:  s.clk.Since(start),
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
	oldCfg *ports.BridgeConfig,
	newCfg *ports.BridgeConfig,
) (*runtime.Runtime, error) {
	newRt, err := s.buildRuntime(ctx, newCfg)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}

	if oldRt != nil {
		drainTimeout := s.drainTimeoutFrom(oldCfg)
		// Detach caller cancellation so drain completes, preserving values.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		if stopErr := oldRt.Stop(stopCtx); stopErr != nil && s.logger != nil {
			s.logger.Warn("supervisor: old runtime stop error", "error", stopErr)
		}
		cancel()
	}

	if err := newRt.Start(ctx); err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: new runtime start failed, attempting recovery with old config", "error", err)
		}
		recoveredRt, recoverErr := s.buildRuntime(ctx, oldCfg)
		if recoverErr == nil {
			recoverErr = recoveredRt.Start(ctx)
		}
		s.mu.Lock()
		if recoverErr == nil {
			s.rt = recoveredRt
		} else {
			s.rt = nil
			if s.logger != nil {
				s.logger.Error("supervisor: recovery with old config also failed", "error", recoverErr)
			}
		}
		s.cfg = oldCfg
		s.mu.Unlock()
		return nil, fmt.Errorf("start: %w", err)
	}

	return newRt, nil
}

func (s *Supervisor) applyPrepareCommit(
	ctx context.Context,
	oldRt *runtime.Runtime,
	oldCfg *ports.BridgeConfig,
	newCfg *ports.BridgeConfig,
) (*runtime.Runtime, error) {
	builder := s.newBuilder(newCfg)

	prep, err := builder.Prepare(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}

	if oldRt != nil {
		drainTimeout := s.drainTimeoutFrom(oldCfg)
		// Detach caller cancellation so drain completes, preserving values.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		if stopErr := oldRt.Stop(stopCtx); stopErr != nil && s.logger != nil {
			s.logger.Warn("supervisor: old runtime stop error", "error", stopErr)
		}
		cancel()
	}

	newRt, err := builder.Complete(ctx, prep)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: Complete failed, attempting recovery with old config", "error", err)
		}
		recoveredRt, recoverErr := s.buildRuntime(ctx, oldCfg)
		if recoverErr == nil {
			recoverErr = recoveredRt.Start(ctx)
		}
		s.mu.Lock()
		if recoverErr == nil {
			s.rt = recoveredRt
		} else {
			s.rt = nil
			if s.logger != nil {
				s.logger.Error("supervisor: recovery with old config also failed", "error", recoverErr)
			}
		}
		s.cfg = oldCfg
		s.mu.Unlock()
		return nil, fmt.Errorf("complete: %w", err)
	}

	if err := newRt.Start(ctx); err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: new runtime start failed (prepare-commit), attempting recovery", "error", err)
		}
		recoveredRt, recoverErr := s.buildRuntime(ctx, oldCfg)
		if recoverErr == nil {
			recoverErr = recoveredRt.Start(ctx)
		}
		s.mu.Lock()
		if recoverErr == nil {
			s.rt = recoveredRt
		} else {
			s.rt = nil
			if s.logger != nil {
				s.logger.Error("supervisor: recovery with old config also failed", "error", recoverErr)
			}
		}
		s.cfg = oldCfg
		s.mu.Unlock()
		return nil, fmt.Errorf("start: %w", err)
	}

	return newRt, nil
}

func (s *Supervisor) buildRuntime(ctx context.Context, cfg *ports.BridgeConfig) (*runtime.Runtime, error) {
	builder := s.newBuilder(cfg)
	return builder.Build(ctx)
}

func (s *Supervisor) newBuilder(cfg *ports.BridgeConfig) *Builder {
	s.mu.RLock()
	transports := maps.Clone(s.transports)
	stores := maps.Clone(s.stores)
	procs := maps.Clone(s.processors)
	s.mu.RUnlock()

	var opts []BuilderOption
	if s.logger != nil {
		opts = append(opts, WithLogger(s.logger))
	}
	if s.credStore != nil {
		opts = append(opts, WithCredentialStore(s.credStore))
	}
	if s.validator != nil {
		opts = append(opts, WithBlueprintValidator(s.validator))
	}
	b := NewBuilder(cfg, opts...)
	for name, tf := range transports {
		b.RegisterTransport(name, tf)
	}
	for name, sf := range stores {
		b.RegisterStoreFactory(name, sf)
	}
	for name, p := range procs {
		b.RegisterProcessor(name, p)
	}
	return b
}

func (s *Supervisor) detectSwapMode(cfg *ports.BridgeConfig) SwapMode {
	if s.swapMode != SwapAuto {
		return s.swapMode
	}
	s.mu.RLock()
	transports := maps.Clone(s.transports)
	s.mu.RUnlock()

	for _, sess := range cfg.Sessions {
		tf, ok := transports[sess.Transport]
		if !ok {
			continue
		}
		if slices.Contains(tf.Capabilities(), ports.CapExclusiveIdentity) {
			return SwapPrepareCommit
		}
	}
	return SwapOverlap
}

func (s *Supervisor) stopCurrent(ctx context.Context) error {
	s.mu.RLock()
	rt := s.rt
	cfg := s.cfg
	s.mu.RUnlock()

	if rt == nil {
		return nil
	}

	drainTimeout := s.drainTimeoutFrom(cfg)
	// Caller ctx is typically already cancelled when we reach here (final
	// shutdown path); detach cancellation but preserve values so the drain
	// can complete within drainTimeout.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	return rt.Stop(stopCtx)
}

func (s *Supervisor) drainTimeoutFrom(cfg *ports.BridgeConfig) time.Duration {
	if cfg != nil {
		return cfg.Bridge.DrainTimeoutDuration()
	}
	if s.defaultDrainTimeout > 0 {
		return s.defaultDrainTimeout
	}
	return 30 * time.Second
}

// PerRecordDrainTimeout returns the configured per-record drain
// timeout from the supplied config, falling back to the supervisor
// default when unset. Zero means the caller should use the legacy
// fixed DrainTimeout instead of the scaled formula.
func (s *Supervisor) PerRecordDrainTimeout(cfg *ports.BridgeConfig) time.Duration {
	if cfg != nil {
		if d := cfg.Bridge.PerRecordDrainTimeoutDuration(); d > 0 {
			return d
		}
	}
	return s.defaultPerRecordDrainTimeout
}

// MaxDrainTimeout returns the configured max drain timeout from the
// supplied config, falling back to the supervisor default when unset.
// Zero means the caller should use the legacy fixed DrainTimeout.
func (s *Supervisor) MaxDrainTimeout(cfg *ports.BridgeConfig) time.Duration {
	if cfg != nil {
		if d := cfg.Bridge.MaxDrainTimeoutDuration(); d > 0 {
			return d
		}
	}
	return s.defaultMaxDrainTimeout
}
