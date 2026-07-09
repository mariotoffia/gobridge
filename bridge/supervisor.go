package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
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

	// Deferred is true when the Supervisor deliberately did NOT apply NewConfig
	// and instead recorded it as the desired state to resume later — currently
	// only when the bridge is paused by an admin StopBridge. A deferred event
	// carries Error == nil (it is not a failure): the config is committed and
	// will take effect on the next StartBridge, so an in-band applier MUST treat
	// it as committed-not-applied (no rollback), not as a successful swap.
	Deferred bool
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
	swapDeadline        time.Duration
	validator           ports.BlueprintValidator

	// regErrs accumulates deferred registration errors (e.g. a duplicate
	// transport/store/processor name) under s.mu. The CLI registers plugins on
	// the Supervisor, whose deduped maps would otherwise silently drop a
	// collision before the Builder's own guard could see it; newBuilder forwards
	// these into every Builder it makes so prepare() fails the build naming the
	// offender rather than routing through a silently-replaced plugin.
	regErrs []error

	// wedged is set when a swap AND its recovery both failed, leaving no
	// active runtime (s.rt == nil). It is a terminal state: the process is
	// alive but routes nothing, so the composition-root backstop must treat
	// it as terminal and exit non-zero (Finding 7).
	wedged bool
	// swapping is true for the whole duration of apply(): a reconfiguration is
	// in progress. During a swap the old runtime is being stopped and a new one
	// built, so a momentary read of a stopping/absent runtime is NOT death.
	// Terminal() returns false while swapping so the liveness backstop never
	// kills the process mid-swap (CRITICAL 3).
	swapping bool
	// degraded records that live reconfiguration is no longer available
	// (the config change stream closed unexpectedly) while the current
	// runtime keeps serving. Not terminal (Finding 1).
	degraded       bool
	degradedReason string

	// lifecycleMu serializes control-plane mutations: apply() (config-driven
	// swaps, run from the Run goroutine) and StartBridge/StopBridge (admin,
	// called from HTTP handler goroutines). Without it two control ops can each
	// build+start a runtime and both publish s.rt — the loser is a fully-started
	// runtime (live consumers, held lease, open store handles) that nothing
	// references and nothing will ever Stop, causing duplicate delivery, lease
	// contention, and leaks. It is DISTINCT from mu, which stays free for fast
	// Terminal()/health reads, so liveness probes never block on a swap/drain.
	lifecycleMu sync.Mutex
	// baseCtx is the long-lived Run context. StartBridge starts a resumed runtime
	// under it, NOT the ephemeral admin-request ctx: Runtime.Start binds the
	// runtime's lifetime to the ctx it is given, so starting under a request ctx
	// would self-Stop the runtime the instant the HTTP handler returns and its
	// defer cancels that ctx (CRITICAL 1).
	baseCtx context.Context
	// paused records a deliberate admin StopBridge. While paused a config reload
	// records the new config but does NOT start a runtime, so an operator's
	// maintenance pause is not silently undone by a config-file edit. Cleared by
	// StartBridge.
	paused bool
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

// WithSwapDeadline bounds the synchronous build/complete phase of a
// reconfiguration swap (session/receiver/sender construction and credential
// resolve are external calls with no inherent timeout). A hung broker during a
// SwapPrepareCommit — where the old runtime is stopped BEFORE complete() runs —
// would otherwise leave the bridge routing nothing forever. Zero uses
// defaultSwapDeadline.
func WithSwapDeadline(d time.Duration) SupervisorOption {
	return func(s *Supervisor) {
		if d > 0 {
			s.swapDeadline = d
		}
	}
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
// A duplicate name is NOT silently overwritten (which would make the losing
// factory vanish from every route using it): the first registration is kept
// and the duplicate is recorded as a deferred error surfaced at the next build.
func (s *Supervisor) RegisterTransport(name string, factory ports.TransportFactory) *Supervisor {
	s.mu.Lock()
	if _, exists := s.transports[name]; exists {
		s.regErrs = append(s.regErrs, fmt.Errorf("bridge: transport factory %q already registered", name))
		s.mu.Unlock()
		return s
	}
	s.transports[name] = factory
	s.mu.Unlock()
	return s
}

// RegisterStoreFactory registers a store factory for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
// A duplicate name is recorded as a deferred error surfaced at the next build.
func (s *Supervisor) RegisterStoreFactory(name string, factory ports.StoreFactory) *Supervisor {
	s.mu.Lock()
	if _, exists := s.stores[name]; exists {
		s.regErrs = append(s.regErrs, fmt.Errorf("bridge: store factory %q already registered", name))
		s.mu.Unlock()
		return s
	}
	s.stores[name] = factory
	s.mu.Unlock()
	return s
}

// RegisterProcessor registers a named processor for reuse across
// rebuilds. Returns the supervisor for chaining. Safe for concurrent use.
// A duplicate name is recorded as a deferred error surfaced at the next build.
func (s *Supervisor) RegisterProcessor(name string, proc ports.Processor) *Supervisor {
	s.mu.Lock()
	if _, exists := s.processors[name]; exists {
		s.regErrs = append(s.regErrs, fmt.Errorf("bridge: processor %q already registered", name))
		s.mu.Unlock()
		return s
	}
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
	// Capture the process-lifetime context so admin StartBridge can start a
	// resumed runtime under it rather than an ephemeral request ctx (CRITICAL 1).
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()

	rt, err := s.buildRuntime(ctx, initial)
	if err != nil {
		return fmt.Errorf("supervisor: initial build: %w", err)
	}

	if err := rt.Start(ctx); err != nil {
		// The initial runtime built its sessions/receivers/stores but never
		// started; returning here without releasing them leaks every connection
		// set (HIGH: initial Start failure leaks the built runtime). Mirror the
		// swap paths' careful stopAbandoned before returning.
		s.stopAbandoned(ctx, rt, initial)
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

	// Export the degraded state so operators can alert on a bridge running
	// blind on its last good config (previously this was invisible outside the
	// logs). Emitted after releasing the lock so the exporter is never called
	// under s.mu.
	s.emitConfigDegradedGauge(true)
}

// emitConfigDegradedGauge exports the 0/1 config-degraded gauge. No-op when no
// metrics exporter is wired (WithSupervisorMetrics not supplied).
func (s *Supervisor) emitConfigDegradedGauge(degraded bool) {
	if s.metrics == nil {
		return
	}
	value := 0.0
	if degraded {
		value = 1
	}
	s.metrics.Gauge(shared.MetricConfigDegraded, value)
}

// emitConfigReload counts a live reconfiguration outcome, tagged success|failure.
// No-op when no metrics exporter is wired.
func (s *Supervisor) emitConfigReload(success bool) {
	if s.metrics == nil {
		return
	}
	state := "failure"
	if success {
		state = "success"
	}
	s.metrics.Counter(shared.MetricConfigReloads, 1, shared.Tag{Key: shared.TagKeyState, Value: state})
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
	swapping := s.swapping
	rt := s.rt
	s.mu.RUnlock()
	// A swap in progress is not death: the old runtime is being stopped and a
	// new one built, so a transient stopping/absent-runtime read during the swap
	// window must not trip the backstop (CRITICAL 3).
	if swapping {
		return false
	}
	if wedged {
		return true
	}
	return rt != nil && rt.Terminal()
}

// StopBridge performs a clean, DELIBERATE stop of the current runtime (an admin
// pause). Unlike a component-failure trip this leaves the runtime non-terminal,
// so /live stays 200 and the liveness backstop does NOT restart the process
// (CRITICAL 1). The runtime is single-use once stopped; StartBridge builds a
// fresh runtime to resume. Calling StopBridge when no runtime is active is a
// no-op. The stopped runtime reference is retained so StartBridge can rebuild
// from the same config.
func (s *Supervisor) StopBridge(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.RLock()
	rt := s.rt
	cfg := s.cfg
	s.mu.RUnlock()
	if rt == nil {
		return nil
	}
	drainTimeout := s.drainTimeoutFrom(cfg)
	// Detach caller cancellation so the drain completes, preserving values.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	stopErr := rt.Stop(stopCtx)
	// Latch the deliberate pause regardless of drain outcome: rt.Stop has already
	// marked the runtime stopped/single-use, so a subsequent config reload must
	// not silently resume the bridge while an operator intends it paused.
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
	return stopErr
}

// StartBridge resumes a deliberately-stopped bridge by building and starting a
// FRESH runtime from the current config. The runtime is single-use, so a
// stopped runtime cannot be restarted in place — "resume" means build+start a
// new instance (CRITICAL 1: /bridge/start after /bridge/stop must succeed, not
// return a permanent 409). If a runtime is already running this is an
// idempotent no-op. A build/start failure releases any half-built runtime
// rather than leaking it, and leaves the previous (stopped) runtime reference
// in place so a retry is possible.
func (s *Supervisor) StartBridge(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.RLock()
	rt := s.rt
	cfg := s.cfg
	baseCtx := s.baseCtx
	s.mu.RUnlock()
	if rt != nil && rt.IsRunning() {
		return nil
	}
	if cfg == nil {
		return fmt.Errorf("supervisor: no config available to start bridge")
	}
	if baseCtx == nil {
		// Run has not been entered, so there is no process-scoped context to own
		// the runtime's lifetime. Refuse rather than bind it to the request ctx.
		return fmt.Errorf("supervisor: not running; cannot start bridge")
	}
	// Bound the BUILD by the admin-request ctx (so the handler stays responsive to
	// its operation timeout), but START the runtime under the long-lived Run ctx:
	// binding the runtime's lifetime to the request ctx would self-Stop it the
	// instant the handler returns and cancels that ctx (CRITICAL 1).
	newRt, err := s.buildRuntimeBounded(ctx, cfg)
	if err != nil {
		return fmt.Errorf("supervisor: start bridge build: %w", err)
	}
	if err := newRt.Start(baseCtx); err != nil {
		s.stopAbandoned(ctx, newRt, cfg)
		return fmt.Errorf("supervisor: start bridge: %w", err)
	}
	s.mu.Lock()
	s.rt = newRt
	s.wedged = false
	s.paused = false
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) apply(ctx context.Context, newCfg *ports.BridgeConfig) {
	// Serialize against admin StartBridge/StopBridge so two control-plane
	// operations never both build and publish a runtime (HIGH: control-plane
	// TOCTOU leaks a fully-started runtime).
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	// While an operator has deliberately paused the bridge (StopBridge), a config
	// reload must NOT silently resume it. Record the new config so a later
	// StartBridge resumes on the latest desired state, but do not build or start
	// a runtime now.
	s.mu.RLock()
	paused := s.paused
	s.mu.RUnlock()
	if paused {
		s.mu.Lock()
		oldCfg := s.cfg
		s.cfg = newCfg
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Info("supervisor: config change recorded but not applied; bridge is paused by admin",
				"config_version", newCfg.Version)
		}
		// onSwap MUST be total: an in-band applier blocks on the swap result for
		// this exact config, so a paused apply that returned without firing
		// onSwap would hang that waiter until its deadline. Emit a DEFERRED event
		// (Error == nil, Deferred == true) so the applier resolves immediately
		// with a committed-not-applied (no-rollback) outcome.
		if s.onSwap != nil {
			s.safeOnSwap(SwapEvent{
				OldConfig: oldCfg,
				NewConfig: newCfg,
				Deferred:  true,
			})
		}
		return
	}

	// Mark a swap in progress for its whole duration so Terminal() reports false
	// while the old runtime is being stopped and the new one built — a swap is
	// not death (CRITICAL 3). Cleared on every exit path (success and error).
	s.mu.Lock()
	s.swapping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.swapping = false
		s.mu.Unlock()
	}()

	start := s.clk.Now()
	mode := s.detectSwapMode(newCfg)

	s.mu.RLock()
	oldRt := s.rt
	oldCfg := s.cfg
	s.mu.RUnlock()

	var newRt *runtime.Runtime
	var err error

	// A live reload that repoints a durable store to a DIFFERENT backing
	// identity (changed type, or changed path/table) would strand the OLD
	// store's pending/claimed backlog — the new runtime opens the new location
	// while unprocessed durable rows sit in the old one, silently missed until
	// manual recovery (supervisor.go:681, Chunk 10). Detect it before touching
	// either runtime.
	identErr := storeIdentityChanged(oldCfg, newCfg)

	// Dropping a durable store entirely (non-nil -> nil) is a deliberate
	// operator action the swap allows, but it silently abandons whatever backlog
	// sat in the removed store; warn so that abandonment is observable.
	if s.logger != nil {
		if removed := removedStoreRoles(oldCfg, newCfg); len(removed) > 0 {
			s.logger.Warn("supervisor: reload removes durable store(s); any backlog they hold is abandoned",
				"removed_stores", removed, "attempted_config_version", newCfg.Version)
		}
	}

	switch {
	case oldRt != nil && identErr != nil:
		// Refuse the swap and keep the old runtime (and its store) serving; an
		// operator must drain/migrate deliberately. Coherent with the
		// stop-failed abort (Finding #3): both keep the old runtime rather than
		// risk data hazards.
		err = identErr
		if s.logger != nil {
			s.logger.Error("supervisor: refusing reload that changes a durable store identity; "+
				"draining/migrating the old store is required to avoid stranding its backlog",
				"error", err, "attempted_config_version", newCfg.Version)
		}
	case mode == SwapPrepareCommit:
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
		s.emitConfigReload(false)
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
		// A successful reload clears any prior degraded state; export both the
		// gauge reset and the success counter so operators see recovery.
		s.emitConfigDegradedGauge(false)
		s.emitConfigReload(true)
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
	newRt, err := s.buildRuntimeBounded(ctx, newCfg)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}

	if oldRt != nil {
		drainTimeout := s.drainTimeoutFrom(oldCfg)
		// Detach caller cancellation so drain completes, preserving values.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		stopErr := oldRt.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			// The old runtime did NOT stop cleanly (e.g. a hung broker close).
			// Starting the freshly built runtime now would leave TWO runtimes
			// live against the same brokers — duplicate consumption,
			// exclusive-identity conflicts, stale lease writes
			// (supervisor.go:690, Chunk 3). Refuse the swap: release the
			// built-but-unstarted new runtime and fail the reload, leaving the
			// old (not-fully-stopped) runtime as the still-current one.
			if s.logger != nil {
				s.logger.Error("supervisor: old runtime stop failed; aborting swap to avoid running two runtimes", "error", stopErr)
			}
			s.stopAbandoned(ctx, newRt, newCfg)
			// The retained old runtime is in an ambiguous half-stopped state
			// (drain timed out / broker close hung), so it is NOT plainly
			// healthy. Surface a degraded signal so operators can observe it
			// rather than reading a clean health; a later successful reload
			// clears it.
			s.markDegraded("old runtime stop failed during reload; retained runtime may be half-stopped")
			return nil, fmt.Errorf("stop old runtime: %w", stopErr)
		}
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
	// Bound the recovery build by the swap deadline. If the swap failed because a
	// broker is partitioned, rebuilding the OLD config hits the same broker and
	// NewSession can hang; an UNBOUNDED build here would keep apply() from
	// returning, leaving swapping=true (and thus Terminal()==false) forever — a
	// permanent silent outage with the liveness backstop disabled (BLOCKER 2). A
	// bounded build fails within the deadline, wedges, and lets Terminal() report
	// the wedge so the composition-root backstop restarts the process.
	recoveredRt, recoverErr := s.buildRuntimeBounded(ctx, oldCfg)
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

	// Bound the synchronous build/complete phase so a hung external construction
	// call (NewSession against a partitioned broker, credential resolve) cannot
	// strand the bridge. This is critical for prepare-commit: complete() runs
	// AFTER the old runtime is stopped, so an unbounded hang there routes nothing
	// forever. On deadline the error routes into recoverOldOrWedge below (HIGH:
	// no deadline on swap build/complete phase).
	phaseCtx, phaseCancel := s.swapPhaseCtx(ctx)
	defer phaseCancel()

	prep, err := builder.prepare(phaseCtx)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}

	if oldRt != nil {
		drainTimeout := s.drainTimeoutFrom(oldCfg)
		// Detach caller cancellation so drain completes, preserving values.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		stopErr := oldRt.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			// The old runtime did NOT stop cleanly. complete() below opens the
			// NEW exclusive-identity sessions/receivers; doing so while the old
			// runtime may still hold them causes broker identity conflicts and
			// double consumption (supervisor.go:690, Chunk 3). Refuse the swap:
			// release the prep-opened store handles and fail the reload, leaving
			// the old (not-fully-stopped) runtime current instead of overlapping.
			if s.logger != nil {
				s.logger.Error("supervisor: old runtime stop failed; aborting prepare-commit swap to avoid running two runtimes", "error", stopErr)
			}
			builder.closeStoreHandles(prep.stores)
			// Retained old runtime is in an ambiguous half-stopped state; surface
			// a degraded signal (cleared by a later successful reload) rather than
			// reporting plain health.
			s.markDegraded("old runtime stop failed during reload; retained runtime may be half-stopped")
			return nil, fmt.Errorf("stop old runtime: %w", stopErr)
		}
	}

	newRt, err := builder.complete(phaseCtx, prep)
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

// buildRuntimeBounded builds a runtime under the swap-phase deadline so a hung
// external construction call during an overlap swap cannot block forever. The
// overlap build runs BEFORE the old runtime is stopped, so a deadline here
// simply fails the swap and leaves the old runtime serving.
func (s *Supervisor) buildRuntimeBounded(ctx context.Context, cfg *ports.BridgeConfig) (*runtime.Runtime, error) {
	phaseCtx, cancel := s.swapPhaseCtx(ctx)
	defer cancel()
	return s.buildRuntime(phaseCtx, cfg)
}

func (s *Supervisor) newBuilder(cfg *ports.BridgeConfig) *Builder {
	s.mu.RLock()
	transports := maps.Clone(s.transports)
	stores := maps.Clone(s.stores)
	procs := maps.Clone(s.processors)
	regErrs := append([]error(nil), s.regErrs...)
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
	// Surface any Supervisor-level registration errors (duplicate names) that the
	// deduped maps above would otherwise hide, so prepare() fails the build
	// naming the offender. These are permanent until the registration code is
	// fixed, so re-forwarding them on every rebuild is intentional.
	b.regErrs = append(b.regErrs, regErrs...)
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

// storageIdentifiedConfig is an OPTIONAL store PluginConfig capability that
// reports a stable descriptor of the DURABLE backing location (e.g. a SQLite
// file path or a DynamoDB table name) — and nothing tunable. When a store
// config implements it, storeIdentity compares only that descriptor, so a
// tuning-only reload (e.g. changing stale_claim_duration) is NOT mistaken for a
// backing-store change. The descriptor MUST NOT contain secrets.
type storageIdentifiedConfig interface {
	StorageIdentity() string
}

// storeIdentityChanged reports an error when a hot reload would repoint an
// EXISTING durable store (lease/outbox/DLQ present in BOTH configs) to a
// different backing identity — a changed Type, or a changed path/table.
// Swapping such a store live strands the old store's durable backlog
// (supervisor.go:681, Chunk 10). It returns nil when nothing durable changes.
//
// Adding a store where none existed (nil -> non-nil) or removing one entirely
// (non-nil -> nil) is deliberately NOT flagged here: an add has no prior backlog
// to strand, and a full removal is a distinct operator action. This guard
// targets the specific "same role, different backing location" swap the finding
// describes.
func storeIdentityChanged(oldCfg, newCfg *ports.BridgeConfig) error {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	roles := []struct {
		name     string
		old, new *ports.StoreConfig
	}{
		{"lease", oldCfg.Stores.Lease, newCfg.Stores.Lease},
		{"outbox", oldCfg.Stores.Outbox, newCfg.Stores.Outbox},
		{"dlq", oldCfg.Stores.DLQ, newCfg.Stores.DLQ},
	}
	for _, r := range roles {
		if r.old == nil || r.new == nil {
			continue
		}
		if storeIdentity(r.old) != storeIdentity(r.new) {
			// The identity fingerprint (path/table, or a raw options blob that
			// could carry credentials) is NEVER put in the error. Surface only
			// the role and the non-secret type discriminators, and adapt the
			// wording so an unchanged type does not read as the misleading
			// "type sqlite -> sqlite".
			if r.old.Type != r.new.Type {
				return fmt.Errorf("bridge: refusing live reload: %s store type changed "+
					"(%q -> %q); the old store's durable backlog would be stranded — "+
					"drain or migrate it before changing its type",
					r.name, r.old.Type, r.new.Type)
			}
			return fmt.Errorf("bridge: refusing live reload: %s store keeps type %q but its "+
				"durable backing location (path/table) changed; the old store's backlog would be "+
				"stranded — drain or migrate it before repointing the store",
				r.name, r.old.Type)
		}
	}
	return nil
}

// removedStoreRoles reports which durable store roles were present in oldCfg but
// dropped entirely in newCfg (non-nil -> nil). Such a removal is a deliberate
// operator action, not a stranding hazard the swap refuses, but it silently
// abandons whatever backlog sat in the removed store — so the caller logs an
// operator-facing warning. Returns nil when nothing was removed.
func removedStoreRoles(oldCfg, newCfg *ports.BridgeConfig) []string {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	var removed []string
	roles := []struct {
		name     string
		old, new *ports.StoreConfig
	}{
		{"lease", oldCfg.Stores.Lease, newCfg.Stores.Lease},
		{"outbox", oldCfg.Stores.Outbox, newCfg.Stores.Outbox},
		{"dlq", oldCfg.Stores.DLQ, newCfg.Stores.DLQ},
	}
	for _, r := range roles {
		if r.old != nil && r.new == nil {
			removed = append(removed, r.name)
		}
	}
	return removed
}

// storeIdentity returns a comparable descriptor of a store's durable backing.
// It prefers an explicit storageIdentifiedConfig capability (path/table only);
// otherwise it falls back to Type plus a deterministic fingerprint of the raw
// stage-1 options (conservative: any option change trips the guard, which is the
// safe default for durable data). A hand-built config with neither capability
// nor raw options degrades to a deterministic fingerprint of its typed value, so
// two same-type configs pointing at DIFFERENT backings are still distinguished
// (fail-safe: any field difference trips the guard) rather than collapsing to
// the Type discriminator alone and silently stranding a backlog.
func storeIdentity(sc *ports.StoreConfig) string {
	if sc == nil {
		return ""
	}
	if si, ok := sc.Config.(storageIdentifiedConfig); ok {
		return sc.Type + "|id=" + si.StorageIdentity()
	}
	if raw := sc.Raw(); raw != nil {
		var m map[string]any
		if err := raw.Decode(&m); err == nil {
			if b, err := json.Marshal(m); err == nil {
				return sc.Type + "|raw=" + string(b)
			}
		}
	}
	// Last resort: fingerprint the typed config value itself. fmt prints struct
	// fields in declaration order and map keys sorted, so this is deterministic
	// for a given concrete type.
	return sc.Type + "|struct=" + fmt.Sprintf("%#v", sc.Config)
}

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

// defaultSwapDeadline bounds the synchronous build/complete phase of a
// reconfiguration swap when WithSwapDeadline is unset. Session/receiver/sender
// construction and credential resolve are external calls with no inherent
// timeout; without this ceiling a hung NewSession (e.g. a partitioned broker)
// during a SwapPrepareCommit — where the old runtime is stopped BEFORE
// complete() runs — leaves the bridge routing nothing forever.
const defaultSwapDeadline = 60 * time.Second

// swapPhaseCtx derives a deadline-bounded context for the build/complete phase
// of a swap, so a hung external construction call cannot strand the bridge. The
// returned context is used ONLY for construction; a started runtime's
// background goroutines keep using the long-lived Run ctx.
func (s *Supervisor) swapPhaseCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := s.swapDeadline
	if d <= 0 {
		d = defaultSwapDeadline
	}
	return context.WithTimeout(ctx, d)
}
