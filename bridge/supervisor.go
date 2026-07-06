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
	mu                  sync.RWMutex
	rt                  *runtime.Runtime
	cfg                 *ports.BridgeConfig
	transports          map[string]ports.TransportFactory
	stores              map[string]ports.StoreFactory
	processors          map[string]ports.Processor
	credStore           ports.CredentialStore
	pushCredStore       ports.PushCredentialStore
	pollCredStore       ports.PullCredentialStore
	pollCredConfig      ports.PollBasedWrapperConfig
	metrics             ports.MetricsExporter
	tracer              ports.Tracer
	auditLogger         ports.AuditLogger
	logger              *slog.Logger
	clk                 clock.Clock
	swapMode            SwapMode
	strategy            ReconfigStrategy
	onSwap              func(SwapEvent)
	defaultDrainTimeout time.Duration
	validator           ports.BlueprintValidator

	// wedged is set when a swap AND its recovery both failed, leaving no
	// active runtime (s.rt == nil). It is a terminal state: the process is
	// alive but routes nothing, so the composition-root backstop must treat
	// it as terminal and exit non-zero (Finding 7).
	wedged bool
	// degraded records that live reconfiguration is no longer available
	// (the config change stream closed unexpectedly) while the current
	// runtime keeps serving. Not terminal (Finding 1).
	degraded       bool
	degradedReason string
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
// store via runtime/credentials.NewPollBasedWrapper.
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

// WithDefaultPerRecordDrainTimeout was removed: it had zero effect and no
// callers (the scaled drain formula is configured per-session via the
// blueprint, not on the supervisor). See Finding 9.

// WithDefaultMaxDrainTimeout was removed for the same reason (Finding 9).

// WithSupervisorMetrics injects the metrics exporter forwarded to every
// Builder (and thus every Runtime) the supervisor creates. Without it, a
// config-driven deployment runs the Noop exporter and emits no metrics
// (Finding 15). Nil is ignored.
func WithSupervisorMetrics(m ports.MetricsExporter) SupervisorOption {
	return func(s *Supervisor) {
		if m != nil {
			s.metrics = m
		}
	}
}

// WithSupervisorTracer injects the distributed tracer forwarded to every
// Builder/Runtime the supervisor creates (Finding 15). Nil is ignored.
func WithSupervisorTracer(t ports.Tracer) SupervisorOption {
	return func(s *Supervisor) {
		if t != nil {
			s.tracer = t
		}
	}
}

// WithSupervisorAuditLogger injects the audit logger forwarded to every
// Builder/Runtime the supervisor creates (Finding 15). Nil is ignored.
func WithSupervisorAuditLogger(a ports.AuditLogger) SupervisorOption {
	return func(s *Supervisor) {
		if a != nil {
			s.auditLogger = a
		}
	}
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
				// The config change stream closed WITHOUT a shutdown request
				// (ctx still live). This is a watcher failure or an upstream
				// close — NOT a reason to tear down a healthy runtime. Closing
				// here used to drain+stop the whole bridge and exit 0, turning
				// inotify exhaustion into a silent total outage (Finding 1).
				// Keep the current runtime serving, mark degraded so operators
				// can observe that live reconfiguration is gone, and block until
				// ctx is actually cancelled.
				select {
				case <-ctx.Done():
					return s.stopCurrent(ctx)
				default:
				}
				s.markDegraded("config change stream closed; live reconfiguration unavailable")
				if s.logger != nil {
					s.logger.Error("supervisor: config change stream closed unexpectedly; " +
						"keeping current runtime serving (live reconfiguration disabled until restart)")
				}
				<-ctx.Done()
				return s.stopCurrent(ctx)
			}
			s.apply(ctx, newCfg)
		}
	}
}

// markDegraded records that the supervisor can no longer apply config
// changes even though the current runtime keeps serving. Not terminal.
func (s *Supervisor) markDegraded(reason string) {
	s.mu.Lock()
	s.degraded = true
	s.degradedReason = reason
	s.mu.Unlock()
}

// Degraded reports whether live reconfiguration is unavailable while the
// current runtime keeps serving (e.g. the config change stream closed). It
// is NOT terminal; use Terminal for the exit backstop. Safe for concurrent use.
func (s *Supervisor) Degraded() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.degraded, s.degradedReason
}

// Terminal reports whether the supervisor is in an unrecoverable state that
// warrants a process restart. It covers two cases (Finding 7):
//
//   - a wedged supervisor: a swap AND its recovery both failed, so there is
//     no active runtime (s.rt == nil) and the process routes nothing;
//   - the active runtime itself reporting Terminal().
//
// The composition-root liveness backstop polls this instead of only the
// runtime, so a nil-runtime wedge no longer idles alive forever.
func (s *Supervisor) Terminal() bool {
	s.mu.RLock()
	wedged := s.wedged
	rt := s.rt
	s.mu.RUnlock()
	if wedged {
		return true
	}
	return rt != nil && rt.Terminal()
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

	// ponytail: Reconfiguration is per-process. GoBridge deliberately does
	// NOT coordinate config versions across the cluster (no version barrier,
	// no cluster rollback) — see docs/scenarios/10-dynamic-reconfiguration.md
	// "Cluster Semantics and Limitations". We *observe* the running version on
	// every swap (also readable via Supervisor.Config().Version) so operators
	// can detect cross-instance version divergence externally; we do not build
	// coordination here. config_version is always the version running AFTER this
	// attempt: a failed swap recovers the old config, so we log its version as
	// config_version and the rejected version as attempted_config_version —
	// logging the failed version as config_version would make a wedged instance
	// indistinguishable from a healthy one for divergence alerting. oldVersion is
	// -1 when there is no prior config (first apply); 0 is a valid "never committed
	// via the API" version, so -1 is the distinct sentinel.
	oldVersion := -1
	if oldCfg != nil {
		oldVersion = oldCfg.Version
	}

	if err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: reconfiguration failed",
				"swap_mode", mode, "error", err, "duration", ev.Duration,
				"config_version", oldVersion, "attempted_config_version", newCfg.Version)
		}
	} else {
		s.mu.Lock()
		s.rt = newRt
		s.cfg = newCfg
		s.wedged = false
		s.degraded = false
		s.degradedReason = ""
		s.mu.Unlock()

		if s.logger != nil {
			s.logger.Info("supervisor: reconfiguration complete",
				"swap_mode", mode, "duration", ev.Duration,
				"config_version", newCfg.Version, "old_config_version", oldVersion)
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
		// The new runtime built its sessions/receivers/stores but never
		// started; abandoning it here leaks every connection set forever.
		// Stop it (idempotent, bounded) before recovering (Finding 2 / C1).
		s.stopAbandoned(ctx, newRt, newCfg)
		s.recoverOldOrWedge(ctx, oldCfg)
		return nil, fmt.Errorf("start: %w", err)
	}

	return newRt, nil
}

// stopAbandoned stops a built-but-abandoned runtime with a bounded, detached
// context so its prep-opened sessions, receivers, and store handles are
// released instead of leaked (Finding 2 / contract C1). Stop is idempotent
// and safe on a never-started runtime.
func (s *Supervisor) stopAbandoned(ctx context.Context, rt *runtime.Runtime, cfg *ports.BridgeConfig) {
	if rt == nil {
		return
	}
	drainTimeout := s.drainTimeoutFrom(cfg)
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	if stopErr := rt.Stop(stopCtx); stopErr != nil && s.logger != nil {
		s.logger.Warn("supervisor: stopping abandoned runtime", "error", stopErr)
	}
}

// recoverOldOrWedge rebuilds and restarts the previous config after a failed
// swap. On success the old runtime resumes; on failure the supervisor enters
// the wedged terminal state (s.rt == nil) so the composition-root backstop can
// restart the process (Finding 7). Any runtime it builds but cannot start is
// stopped rather than leaked (Finding 2).
func (s *Supervisor) recoverOldOrWedge(ctx context.Context, oldCfg *ports.BridgeConfig) {
	recoveredRt, recoverErr := s.buildRuntime(ctx, oldCfg)
	if recoverErr == nil {
		if startErr := recoveredRt.Start(ctx); startErr != nil {
			s.stopAbandoned(ctx, recoveredRt, oldCfg)
			recoveredRt = nil
			recoverErr = startErr
		}
	}

	s.mu.Lock()
	if recoverErr == nil {
		s.rt = recoveredRt
		s.wedged = false
	} else {
		s.rt = nil
		s.wedged = true
		if s.logger != nil {
			s.logger.Error("supervisor: recovery with old config also failed; "+
				"supervisor wedged (no active runtime, routing nothing)", "error", recoverErr)
		}
	}
	s.cfg = oldCfg
	s.mu.Unlock()
}

func (s *Supervisor) applyPrepareCommit(
	ctx context.Context,
	oldRt *runtime.Runtime,
	oldCfg *ports.BridgeConfig,
	newCfg *ports.BridgeConfig,
) (*runtime.Runtime, error) {
	builder := s.newBuilder(newCfg)

	prep, err := builder.prepare(ctx)
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

	newRt, err := builder.complete(ctx, prep)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: Complete failed, attempting recovery with old config", "error", err)
		}
		// complete() failed after prepare() opened stores; its own defers
		// close the sessions it created, and complete now also releases the
		// prep-opened stores on failure (Finding 2). Recover the old config.
		s.recoverOldOrWedge(ctx, oldCfg)
		return nil, fmt.Errorf("complete: %w", err)
	}

	if err := newRt.Start(ctx); err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: new runtime start failed (prepare-commit), attempting recovery", "error", err)
		}
		s.stopAbandoned(ctx, newRt, newCfg)
		s.recoverOldOrWedge(ctx, oldCfg)
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
	// Forward the rotation-capable credential stores so CredentialRefresher
	// actually binds watchers under hot-reload. Previously these were stored
	// on the supervisor but never forwarded, so rotation silently did nothing
	// for supervisor-built runtimes (Finding 3).
	if s.pushCredStore != nil {
		opts = append(opts, WithPushCredentialStore(s.pushCredStore))
	}
	if s.pollCredStore != nil {
		opts = append(opts, WithPolledCredentialStore(s.pollCredStore, s.pollCredConfig))
	}
	// Forward observability so config-driven deployments are not stuck on Noop
	// everything (Finding 15).
	if s.metrics != nil {
		opts = append(opts, WithMetrics(s.metrics))
	}
	if s.tracer != nil {
		opts = append(opts, WithTracer(s.tracer))
	}
	if s.auditLogger != nil {
		opts = append(opts, WithAuditLogger(s.auditLogger))
	}
	if s.validator != nil {
		opts = append(opts, WithBlueprintValidator(s.validator))
	}
	b := NewBuilder(cfg, opts...)
	for name, tf := range transports {
		b.RegisterTransportFactory(name, tf)
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
	// Capabilities() only reports exclusivity once a factory has already
	// BUILT an exclusive receiver, so the loop above misses the FIRST reconfig
	// that INTRODUCES one (config A: no exclusive → config B: exclusive on the
	// same queue). That swap would still run Overlap, attaching the new
	// exclusive consumer while the old consumer holds the queue → broker 403 →
	// terminal teardown. Detect it up front from the incoming receiver configs
	// via the optional per-transport hook.
	for i := range cfg.Receivers {
		recv := &cfg.Receivers[i]
		transport := recv.Transport
		if transport == "" {
			if sd := findSession(cfg, recv.SessionID); sd != nil {
				transport = sd.Transport
			}
		}
		tf, ok := transports[transport]
		if !ok {
			continue
		}
		if d, ok := tf.(exclusiveIdentityConfigDetector); ok &&
			d.ConfigRequiresExclusiveIdentity(recv.Config) {
			return SwapPrepareCommit
		}
	}
	return SwapOverlap
}

// exclusiveIdentityConfigDetector is an optional transport-factory hook that
// reports, from an incoming receiver config alone, whether that receiver will
// be an exclusive-identity consumer — before any receiver (and thus the
// factory's post-build CapExclusiveIdentity latch) exists. It lets
// detectSwapMode pick the serialized swap mode on the first reconfig that
// introduces exclusivity. Factories that do not implement it are simply not
// consulted (the Capabilities() latch still covers steady-state swaps).
type exclusiveIdentityConfigDetector interface {
	ConfigRequiresExclusiveIdentity(cfg ports.PluginConfig) bool
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
	// The blueprint wins only when it explicitly sets drain_timeout; otherwise
	// the supervisor's configured default applies (Finding 9). Previously
	// DrainTimeoutDuration() always returned 30s for an unset field, so
	// WithDefaultDrainTimeout could never take effect.
	if cfg != nil && cfg.Bridge.DrainTimeout != "" {
		if d := cfg.Bridge.DrainTimeoutDuration(); d > 0 {
			return d
		}
	}
	if s.defaultDrainTimeout > 0 {
		return s.defaultDrainTimeout
	}
	return 30 * time.Second
}
