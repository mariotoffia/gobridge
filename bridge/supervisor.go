package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
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
	durableIdentities   durableSessionIdentitySnapshot
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

	// allowDestructiveReload lets an operator FORCE a live reload that would
	// otherwise be refused because it strands already-durable backlog (an outbox
	// partition losing its drainer, or an outbox/DLQ store removed entirely,
	// while records still sit there). Default false = fail closed (never lose
	// messages). Set via WithAllowDestructiveReload only when the operator truly
	// intends to DISCARD that backlog (HIGH-2/HIGH-3).
	allowDestructiveReload bool

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

// WithAllowDestructiveReload permits a live reload that would otherwise be
// REFUSED because it strands already-durable backlog: an outbox partition that
// loses its drainer in the new topology, or an outbox/DLQ store removed
// entirely, while records still sit there. It is the explicit operator override
// for the durable-reload preflight (HIGH-2/HIGH-3).
//
// Default (false) fails CLOSED — GoBridge never silently discards durable
// messages. Set true ONLY when the operator truly intends to DISCARD that
// backlog; the supervisor then WARN-logs the discarded partitions/stores and
// proceeds with the swap. This option never bypasses durable broker-session
// identity protection; those migrations require an external drain/unsubscribe/
// backlog/cutover procedure.
func WithAllowDestructiveReload(allow bool) SupervisorOption {
	return func(s *Supervisor) { s.allowDestructiveReload = allow }
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
	return cloneConfigForBuild(s.cfg)
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

	appliedInitial := cloneConfigForBuild(initial)
	initialIdentities, err := snapshotDurableSessionIdentities(appliedInitial)
	if err != nil {
		return fmt.Errorf("supervisor: initial durable session identity preflight: %w", err)
	}
	rt, err := s.buildRuntime(ctx, appliedInitial)
	if err != nil {
		return fmt.Errorf("supervisor: initial build: %w", err)
	}

	if err := rt.Start(ctx); err != nil {
		// The initial runtime built its sessions/receivers/stores but never
		// started; returning here without releasing them leaks every connection
		// set (HIGH: initial Start failure leaks the built runtime). Mirror the
		// swap paths' careful stopAbandoned before returning.
		s.stopAbandoned(ctx, rt, appliedInitial)
		return fmt.Errorf("supervisor: initial start: %w", err)
	}

	s.mu.Lock()
	s.rt = rt
	s.cfg = appliedInitial
	s.durableIdentities = initialIdentities
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
	appliedIdentities := cloneDurableSessionIdentitySnapshot(s.durableIdentities)
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
	proposedIdentities, err := snapshotDurableSessionIdentities(cfg)
	if err == nil {
		err = compareDurableSessionIdentitySnapshots(appliedIdentities, proposedIdentities)
	}
	if err != nil {
		return fmt.Errorf("supervisor: start bridge durable session identity preflight: %w", err)
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

	// Freeze the proposal exactly once at the Supervisor boundary. Every
	// preflight, build, stored snapshot, and recovery decision below uses this
	// same immutable content; SwapEvent keeps the original pointer solely for
	// config-manager acknowledgement correlation.
	frozenCfg := cloneConfigForBuild(newCfg)

	// MQTT and similar durable broker sessions are external stores. Changing or
	// removing their identity can strand subscriptions and queued QoS messages,
	// so compare the typed plugin fingerprints before stopping the old runtime,
	// building a replacement, or honoring any destructive-reload override.
	s.mu.RLock()
	paused := s.paused
	oldCfgAtPreflight := s.cfg
	appliedIdentities := cloneDurableSessionIdentitySnapshot(s.durableIdentities)
	s.mu.RUnlock()
	proposedIdentities, identityErr := snapshotDurableSessionIdentities(frozenCfg)
	if identityErr == nil {
		identityErr = compareDurableSessionIdentitySnapshots(appliedIdentities, proposedIdentities)
	}
	if identityErr != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: refusing reload that changes or duplicates durable broker session identity; "+
				"externally drain, unsubscribe, and cut over the old session first",
				"error", identityErr, "attempted_config_version", frozenCfg.Version)
		}
		s.emitConfigReload(false)
		if s.onSwap != nil {
			s.safeOnSwap(SwapEvent{OldConfig: cloneConfigForBuild(oldCfgAtPreflight), NewConfig: newCfg, Error: identityErr})
		}
		return
	}

	// While an operator has deliberately paused the bridge (StopBridge), a config
	// reload must NOT silently resume it. Record the new config so a later
	// StartBridge resumes on the latest desired state, but do not build or start
	// a runtime now.
	if paused {
		oldCfg := oldCfgAtPreflight
		// A paused reload cannot prove-drained: StopBridge already Stopped the
		// old runtime and CLOSED its outbox/DLQ stores, so there is no live store
		// to query. Refuse a DESTRUCTIVE topology change (removed/retargeted
		// outbox|dlq store, changed store identity, or orphaned shared_outbox
		// partition) detected by pure config comparison rather than record a
		// config that would strand durable backlog on the next StartBridge
		// (HIGH-3, paused path). The operator drains before pausing, or forces it
		// with WithAllowDestructiveReload.
		//
		// ponytail: config-comparison superset only — the airtight alternative
		// (prove-drained at StartBridge against the reopened store) is heavier and
		// out of scope. Fire a FAILED swap event (Error set, Deferred false) so an
		// in-band applier waiting on this exact config resolves as a failure
		// instead of hanging to its deadline; keep oldCfg as the resume target.
		if destructiveReloadShape(oldCfg, frozenCfg) && !s.allowDestructiveReload {
			err := fmt.Errorf("bridge: refusing paused destructive reload: it would strand durable " +
				"backlog on resume (removed/retargeted outbox|dlq store, changed store identity, or " +
				"orphaned shared_outbox partition); drain before pausing or set WithAllowDestructiveReload")
			if s.logger != nil {
				s.logger.Error("supervisor: refusing paused config change that would strand durable "+
					"backlog on resume; drain before pausing or force with WithAllowDestructiveReload",
					"error", err, "attempted_config_version", frozenCfg.Version)
			}
			s.emitConfigReload(false)
			if s.onSwap != nil {
				s.safeOnSwap(SwapEvent{
					OldConfig: cloneConfigForBuild(oldCfg),
					NewConfig: newCfg,
					Error:     err,
				})
			}
			return
		}
		s.mu.Lock()
		s.cfg = frozenCfg
		s.durableIdentities = proposedIdentities
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Info("supervisor: config change recorded but not applied; bridge is paused by admin",
				"config_version", frozenCfg.Version)
		}
		// onSwap MUST be total: an in-band applier blocks on the swap result for
		// this exact config, so a paused apply that returned without firing
		// onSwap would hang that waiter until its deadline. Emit a DEFERRED event
		// (Error == nil, Deferred == true) so the applier resolves immediately
		// with a committed-not-applied (no-rollback) outcome.
		if s.onSwap != nil {
			s.safeOnSwap(SwapEvent{
				OldConfig: cloneConfigForBuild(oldCfg),
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
	mode := s.detectSwapMode(frozenCfg)

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
	identErr := storeIdentityChanged(oldCfg, frozenCfg)

	// A live reload that changes a lease-bearing exclusive route's session_id
	// changes the lease IDENTITY, splitting cluster ownership domains: with no
	// cross-node config barrier (by design, HIGH-2) a rolling reload lets one
	// instance run session_id=X while another still runs session_id=Y, and BOTH
	// acquire a lease for the same logical source under DIFFERENT keys and drain
	// independently. Refuse it locally like a store-identity change, unless the
	// operator forces it with the same WithAllowDestructiveReload escape hatch.
	var leaseIdentErr error
	if oldRt != nil && !s.allowDestructiveReload {
		leaseIdentErr = leaseSessionIDChanged(oldCfg, frozenCfg)
	}

	// Dropping a durable store entirely (non-nil -> nil) is a deliberate
	// operator action, but the LEASE store holds only fencing tokens, not
	// message backlog, so its removal strands no messages — warn so it is
	// observable and move on. Outbox/DLQ removal (and outbox-partition
	// orphaning) DOES strand durable backlog; that is gated fail-closed by the
	// durable-reload preflight below, not merely warned (HIGH-2/HIGH-3).
	if s.logger != nil {
		if removed := removedStoreRoles(oldCfg, frozenCfg); slices.Contains(removed, "lease") {
			s.logger.Warn("supervisor: reload removes the lease store; its fencing state is discarded "+
				"(holds no message backlog)", "attempted_config_version", frozenCfg.Version)
		}
	}

	// A live reload must NEVER strand already-durable backlog: an outbox
	// partition that loses its drainer in the new topology, or an outbox/DLQ
	// store removed while records still sit there. Prove the affected backlog is
	// drained (F2 depth capability) or refuse the swap — unless the operator set
	// WithAllowDestructiveReload to discard it (HIGH-2/HIGH-3). Only meaningful
	// when a store identity change has not already refused the swap.
	var preflightErr error
	if oldRt != nil && identErr == nil {
		preflightErr = s.durableReloadPreflight(ctx, oldRt, oldCfg, frozenCfg)
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
				"error", err, "attempted_config_version", frozenCfg.Version)
		}
	case oldRt != nil && preflightErr != nil:
		// Refuse the swap and keep the old runtime serving so its durable
		// backlog is still drained; fail closed rather than abandon messages.
		err = preflightErr
		if s.logger != nil {
			s.logger.Error("supervisor: refusing reload that would strand durable backlog; drain the "+
				"affected partitions/stores first or force with WithAllowDestructiveReload to discard them",
				"error", err, "attempted_config_version", frozenCfg.Version)
		}
	case oldRt != nil && leaseIdentErr != nil:
		// Refuse the swap and keep the old runtime serving under the CURRENT
		// session_id; changing a lease-bearing exclusive session_id is a
		// cluster-wide ownership invariant that a per-process reload cannot roll
		// safely (HIGH-2). The operator must stop/restart all nodes together.
		err = leaseIdentErr
		if s.logger != nil {
			s.logger.Error("supervisor: refusing reload that changes a lease-bearing exclusive "+
				"session_id (cluster ownership invariant); roll it with a full stop/restart of all "+
				"nodes or force with WithAllowDestructiveReload",
				"error", err, "attempted_config_version", frozenCfg.Version)
		}
	case mode == SwapPrepareCommit:
		newRt, err = s.applyPrepareCommit(ctx, oldRt, oldCfg, frozenCfg)
	default:
		newRt, err = s.applyOverlap(ctx, oldRt, oldCfg, frozenCfg)
	}

	// Observable residual (HIGH-2/3): the preflight proved the orphaned
	// partitions empty at CHECK time, but a record could still have landed in the
	// swap window (late ingress) or a claimed send could have failed. Both swap
	// paths fully STOP the old runtime before returning err==nil (applyOverlap
	// stops old before newRt.Start; applyPrepareCommit stops old before
	// complete/Start), so by here ingress has ceased and the NEW runtime opens
	// the SAME durable store (identity changes are refused). Re-query each
	// orphaned partition and surface a non-empty result as an OBSERVABLE signal
	// instead of a silent strand. One-shot / best-effort: a record still in
	// CLAIMED state is not counted here (CountPending is pending-only) and
	// surfaces only when it reverts to pending; the point-in-time refusal above
	// remains the primary guard.
	if err == nil && newRt != nil && oldRt != nil {
		s.reportOrphanedStrand(ctx, newRt, oldCfg, frozenCfg)
	}

	ev := SwapEvent{
		OldConfig: cloneConfigForBuild(oldCfg),
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
				"config_version", oldVersion, "attempted_config_version", frozenCfg.Version)
		}
		s.emitConfigReload(false)
	} else {
		s.mu.Lock()
		s.rt = newRt
		s.cfg = frozenCfg
		s.durableIdentities = proposedIdentities
		s.wedged = false
		s.degraded = false
		s.degradedReason = ""
		s.mu.Unlock()

		if s.logger != nil {
			s.logger.Info("supervisor: reconfiguration complete",
				"swap_mode", mode, "duration", ev.Duration,
				"config_version", frozenCfg.Version, "old_config_version", oldVersion)
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

	// A config that DECLARES exclusivity is serialized regardless of whether its
	// transport factory advertises CapExclusiveIdentity. amqp10 obeys the
	// single-use exclusive-session rule but does NOT advertise the capability
	// (UBIQUITOUS.md:111, PLUGIN.md:173-176), so the capability/hook probes below
	// miss it — and applyOverlap builds (and opens) the new exclusive session
	// before stopping the old one, colliding on the broker identity. hasExclusive
	// Sessions inspects exactly the two config-declared exclusive forms: a named
	// session with session_mode: exclusive, and a route inline session (always
	// exclusive per ports/blueprint.go:359). A single such declaration anywhere
	// forces the serialized prepare-commit swap (HIGH-1).
	if hasExclusiveSessions(cfg) {
		return SwapPrepareCommit
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

// durableSessionIdentitySnapshot is immutable after publication on Supervisor.
// Both keys and fingerprints are opaque comparison material and must not be
// logged. The plugin kind scopes fingerprints from unrelated transports.
type durableSessionIdentitySnapshot map[string]durableSessionIdentity

type durableSessionIdentity struct {
	kind        string
	fingerprint string
}

// durableSessionIdentityChanged remains the config-to-config seam used by unit
// tests. Supervisor reloads compare the proposed snapshot with the immutable
// snapshot captured when the current config was accepted.
func durableSessionIdentityChanged(oldCfg, newCfg *ports.BridgeConfig) error {
	oldIdentities, err := snapshotDurableSessionIdentities(oldCfg)
	if err != nil {
		return err
	}
	newIdentities, err := snapshotDurableSessionIdentities(newCfg)
	if err != nil {
		return err
	}
	return compareDurableSessionIdentitySnapshots(oldIdentities, newIdentities)
}

func snapshotDurableSessionIdentities(cfg *ports.BridgeConfig) (durableSessionIdentitySnapshot, error) {
	snapshot := make(durableSessionIdentitySnapshot)
	if cfg == nil {
		return snapshot, nil
	}
	referenced := referencedSessionIDs(cfg)
	owners := make(map[string]string)
	for _, session := range cfg.Sessions {
		mode := normalizedSessionMode(session.SessionMode)
		if _, ok := referenced[session.ID]; !ok ||
			(mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive) {
			continue
		}
		identityConfig, ok := session.Config.(ports.DurableSessionIdentityConfig)
		if !ok {
			continue
		}
		if isNilInterface(identityConfig) {
			return nil, fmt.Errorf("bridge: durable session %q identity config is nil", session.ID)
		}
		fingerprint, err := identityConfig.DurableSessionIdentity(mode)
		if err != nil || fingerprint == "" {
			return nil, fmt.Errorf("bridge: durable session %q identity cannot be verified", session.ID)
		}
		domains, err := identityConfig.DurableSessionIdentityDomains(mode)
		if err != nil || len(domains) == 0 {
			return nil, fmt.Errorf("bridge: durable session %q identity domains cannot be verified", session.ID)
		}
		identity := durableSessionIdentity{kind: session.Config.Kind(), fingerprint: fingerprint}
		for _, domain := range domains {
			if domain == "" {
				return nil, fmt.Errorf("bridge: durable session %q identity domains cannot be verified", session.ID)
			}
			collisionKey := identity.kind + "\x00" + domain
			if owner, duplicate := owners[collisionKey]; duplicate && owner != session.ID {
				return nil, fmt.Errorf("bridge: durable sessions %q and %q have duplicate effective broker identities", owner, session.ID)
			}
			owners[collisionKey] = session.ID
		}
		snapshot[session.ID] = identity
	}
	return snapshot, nil
}

func compareDurableSessionIdentitySnapshots(oldIdentities, newIdentities durableSessionIdentitySnapshot) error {
	for sessionID, oldIdentity := range oldIdentities {
		newIdentity, ok := newIdentities[sessionID]
		if !ok {
			return fmt.Errorf("bridge: refusing live reload: durable session %q was removed, renamed, or lost its identity capability; its broker state could be stranded", sessionID)
		}
		if oldIdentity != newIdentity {
			return fmt.Errorf("bridge: refusing live reload: durable session %q broker identity changed; externally drain queued messages, unsubscribe the old session, and perform a cutover", sessionID)
		}
	}
	return nil
}

func cloneDurableSessionIdentitySnapshot(in durableSessionIdentitySnapshot) durableSessionIdentitySnapshot {
	out := make(durableSessionIdentitySnapshot, len(in))
	for id, identity := range in {
		out[id] = identity
	}
	return out
}

func normalizedSessionMode(mode string) connectivity.SessionMode {
	if mode == "" {
		return connectivity.SessionEphemeral
	}
	return connectivity.SessionMode(mode)
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

// leaseSessionIDChanged reports an error when a live reload would change the
// session_id of a lease-bearing exclusive route session — the lease IDENTITY.
// An exclusive session's session_id keys the ownership lease the owner acquires.
// Changing it under a rolling reload splits ownership domains: with no
// cross-node config barrier (by design, HIGH-2) instance A can run session_id=X
// while instance B still runs session_id=Y, and BOTH acquire a lease for the
// same logical source under DIFFERENT keys and drain independently — elevated
// duplicate sends and stranded backlog.
//
// A route's exclusive lease identity has TWO sources, both covered here
// (routeExclusiveLeaseIdentities): (a) the inline session block (a
// RouteSessionDef is always exclusive, bridge/convert.go), and (b) the route's
// receiver referencing a shared session declared session_mode: exclusive
// (ReceiverDef.SessionID -> SessionDef.ID). The shared-session vector is the
// same lease keyed by SessionDef.ID that hasExclusiveSessions treats as
// single-owner; missing it left an identical split-ownership hole for
// non-HTTP exclusive routes (H4's shared_outbox rule only covers HTTP ingress,
// and R4's destructiveReloadShape only covers STORE identity).
//
// Routes are matched by route ID (the stable key); a route present in BOTH
// configs whose set of effective exclusive lease identities changed is refused.
// Adding or removing exclusivity entirely (a route exclusive in only one config)
// is a distinct topology action (covered by the durable-reload preflight /
// orphan detection), not flagged here. Returns nil when nothing changed.
func leaseSessionIDChanged(oldCfg, newCfg *ports.BridgeConfig) error {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	oldByRouteID := make(map[string]map[string]struct{}, len(oldCfg.Routes))
	for i := range oldCfg.Routes {
		if ids := routeExclusiveLeaseIdentities(oldCfg, &oldCfg.Routes[i]); len(ids) > 0 {
			oldByRouteID[oldCfg.Routes[i].ID] = ids
		}
	}
	for i := range newCfg.Routes {
		newIDs := routeExclusiveLeaseIdentities(newCfg, &newCfg.Routes[i])
		if len(newIDs) == 0 {
			continue // not exclusive in the new config — add/remove is out of scope
		}
		oldIDs, ok := oldByRouteID[newCfg.Routes[i].ID]
		if !ok {
			continue // not exclusive in the old config — add is out of scope
		}
		if !maps.Equal(oldIDs, newIDs) {
			return fmt.Errorf("bridge: refusing live reload: route %q lease-bearing exclusive "+
				"session_id changed (%v -> %v); session_id keys the ownership lease, and with no "+
				"cross-node config barrier a rolling reload lets different instances hold leases for "+
				"the same logical source under different keys (split ownership) — roll this change "+
				"with a full stop/restart of all nodes, or set WithAllowDestructiveReload to force it",
				newCfg.Routes[i].ID, sortedIdentities(oldIDs), sortedIdentities(newIDs))
		}
	}
	return nil
}

// routeExclusiveLeaseIdentities returns the set of session_ids that key an
// ownership lease for route r, from both sources leaseSessionIDChanged compares:
// the inline session block and the route's receiver referencing a shared
// session_mode: exclusive session. ponytail: the receiver source is gated on the
// config-declared session_mode (mirroring routeIsExclusive / hasExclusiveSessions)
// rather than re-deriving which sessions the builder actually leases at runtime;
// an exclusive-declared session that ends up unmanaged is a degenerate config,
// and refusing an identity change on it (escape hatch available) errs safe.
func routeExclusiveLeaseIdentities(cfg *ports.BridgeConfig, r *ports.RouteDef) map[string]struct{} {
	ids := make(map[string]struct{}, 2)
	if r.Session != nil && r.Session.SessionID != "" {
		ids[r.Session.SessionID] = struct{}{}
	}
	if rd := findReceiver(cfg, r.ReceiverID); rd != nil && rd.SessionID != "" {
		if sd := findSession(cfg, rd.SessionID); sd != nil &&
			sd.SessionMode == string(connectivity.SessionExclusive) {
			ids[rd.SessionID] = struct{}{}
		}
	}
	return ids
}

// sortedIdentities renders a session-id set as a deterministic slice for stable
// error messages.
func sortedIdentities(ids map[string]struct{}) []string {
	out := slices.Collect(maps.Keys(ids))
	slices.Sort(out)
	return out
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

// preflightDepthBudget bounds the depth-report probes the durable-reload
// preflight runs against the OLD runtime's stores. These are ordinary store
// reads (a COUNT / QueryPending), but they run inline on the reconfiguration
// path before the swap, so a wedged store must not stall the reload forever. It
// is detached from the caller ctx so a final-shutdown-cancelled ctx still lets
// the preflight decide.
const preflightDepthBudget = 5 * time.Second

// sharedOutboxPartitionSessions returns the set of session IDs a config wires
// shared_outbox outbox drainers for. Each such session owns the outbox
// partition SESSION#<sid> (persistence.OutboxPartitionKey(sid, "")). It mirrors
// the runtime drainer wiring (runtime/bridge_start.go): for every shared_outbox
// route it covers the resolved PRIMARY session (the inline route session if
// present, else the first binding's session) PLUS every binding session. A route
// that is not shared_outbox, or a binding without a session, contributes no
// drainer here — matching the runtime, which only drains SESSION# partitions.
func sharedOutboxPartitionSessions(cfg *ports.BridgeConfig) map[string]struct{} {
	out := make(map[string]struct{})
	if cfg == nil {
		return out
	}
	for i := range cfg.Routes {
		rd := &cfg.Routes[i]
		if routing.DeliveryMode(rd.DeliveryMode) != routing.DeliverySharedOutbox {
			continue
		}
		// Primary session: an inline route session wins; otherwise the first
		// binding's session (identical to wireRoutes' primary resolution).
		if rd.Session != nil {
			if rd.Session.SessionID != "" {
				out[rd.Session.SessionID] = struct{}{}
			}
		} else if len(rd.Bindings) > 0 {
			if bd := findBinding(cfg, rd.Bindings[0]); bd != nil && bd.SessionID != "" {
				out[bd.SessionID] = struct{}{}
			}
		}
		// Every binding with a session gets its own drainer/partition.
		for _, bid := range rd.Bindings {
			if bd := findBinding(cfg, bid); bd != nil && bd.SessionID != "" {
				out[bd.SessionID] = struct{}{}
			}
		}
	}
	return out
}

// destructiveReloadShape reports whether newCfg would strand durable backlog
// relative to oldCfg using CONFIG COMPARISON ONLY — no live depth query. The
// live preflight (durableReloadPreflight) can prove a partition/store is drained
// against the RUNNING old runtime, but the PAUSED reload path cannot: StopBridge
// has already Stopped the old runtime, which CLOSED its outbox/DLQ stores
// (runtime/bridge.go), so there is nothing to query. This is a conservative
// superset of the live preflight's stranding shape: any changed durable store
// identity, any removed outbox/DLQ store, or any shared_outbox partition that
// loses its drainer in the new topology. It never inspects backlog depth, so a
// paused reload matching this shape is refused unconditionally (unless the
// operator forces it with WithAllowDestructiveReload) rather than risk recording
// a config that would strand backlog on resume (HIGH-3, paused path).
func destructiveReloadShape(oldCfg, newCfg *ports.BridgeConfig) bool {
	if oldCfg == nil || newCfg == nil {
		return false
	}
	if storeIdentityChanged(oldCfg, newCfg) != nil {
		return true
	}
	removed := removedStoreRoles(oldCfg, newCfg)
	if slices.Contains(removed, "outbox") || slices.Contains(removed, "dlq") {
		return true
	}
	// Orphaned shared_outbox partitions: an old partition with no drainer in the
	// new topology. Removing the outbox store entirely leaves the new runtime
	// with no drainers at all, so every old partition is orphaned.
	oldSessions := sharedOutboxPartitionSessions(oldCfg)
	newSessions := sharedOutboxPartitionSessions(newCfg)
	if newCfg.Stores.Outbox == nil {
		newSessions = nil
	}
	for sid := range oldSessions {
		if _, keeps := newSessions[sid]; !keeps {
			return true
		}
	}
	return false
}

// durableReloadPreflight fails a live reload CLOSED when it would strand
// already-durable backlog: an outbox partition that loses its drainer in the new
// topology (route removed, retargeted, changed to direct_hold, or lost its
// session), or an outbox/DLQ store removed entirely — while records still sit
// there. It reuses the F2 depth-report capability (OutboxDepthReporter /
// DLQDepthReporter) to PROVE emptiness; a store that cannot prove its depth is
// treated as UNPROVEN and refused (fail closed — never assume drained). An
// operator who truly intends to discard the backlog forces it with
// WithAllowDestructiveReload. Returns nil when nothing durable would be
// stranded.
//
// ponytail: "refuse while backlog exists" is the audit's explicitly-accepted
// fix; the heavier "shadow drainer until empty" alternative is out of scope. The
// LEASE store is deliberately NOT gated here — it holds fencing tokens, not
// message backlog, exposes no depth capability to prove empty, and gating it
// would wedge legitimate exclusive->non-exclusive reloads that merely drop the
// lease store.
//
// ponytail: this proof is POINT-IN-TIME and PENDING-scoped — matching the
// audit's accepted "refuse while backlog exists" fix (the airtight alternative,
// shadow drainers / drain-orphaned-partitions-to-idle before swap, is
// deliberately out of scope). Two bounded residual windows remain, both
// requiring the operator to remove a route whose SOURCE is still delivering at
// the reload instant:
//  1. Late ingress: a record persisted into an orphaned partition AFTER this
//     count but BEFORE old-runtime Stop is left durable with no new drainer.
//  2. Claimed in-flight: CountPending counts PENDING only (metric semantics;
//     see memoryoutbox.CountPending), so a record a drainer is mid-sending is
//     not counted and strands if its send does not complete during Stop.
//
// Operational close: quiesce the source before removing its shared_outbox
// route. Surfaced observably post-swap via reportOrphanedStrand; primary guard
// is the point-in-time refusal above.
func (s *Supervisor) durableReloadPreflight(ctx context.Context, oldRt *runtime.Runtime, oldCfg, newCfg *ports.BridgeConfig) error {
	if oldRt == nil || oldCfg == nil || newCfg == nil {
		return nil
	}

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preflightDepthBudget)
	defer cancel()

	var stranded []string

	// Outbox: a partition is stranded when it had a drainer in the OLD topology
	// but has none in the NEW one. Removing the outbox store entirely leaves the
	// new runtime with NO drainers at all, so every old partition is orphaned.
	if oldCfg.Stores.Outbox != nil {
		oldSessions := sharedOutboxPartitionSessions(oldCfg)
		newSessions := sharedOutboxPartitionSessions(newCfg)
		if newCfg.Stores.Outbox == nil {
			newSessions = nil
		}
		for sid := range oldSessions {
			if _, keeps := newSessions[sid]; keeps {
				continue
			}
			partition := persistence.OutboxPartitionKey(sid, "")
			n, ok, err := oldRt.OutboxPending(pctx, partition)
			switch {
			case err != nil:
				stranded = append(stranded, fmt.Sprintf("outbox partition %s (depth query failed: %v)", partition, err))
			case !ok:
				stranded = append(stranded, fmt.Sprintf("outbox partition %s (store cannot prove it is drained)", partition))
			case n > 0:
				stranded = append(stranded, fmt.Sprintf("outbox partition %s (%d pending record(s))", partition, n))
			}
		}
	}

	// DLQ: only a full store removal strands backlog (a DLQ has no per-partition
	// drainer topology to orphan). Prove it empty via the DLQ depth capability.
	if oldCfg.Stores.DLQ != nil && newCfg.Stores.DLQ == nil {
		n, ok, err := runtime.ReportDLQDepth(pctx, oldRt.DLQReader(), nil)
		switch {
		case err != nil:
			stranded = append(stranded, fmt.Sprintf("dlq store (depth query failed: %v)", err))
		case !ok:
			stranded = append(stranded, "dlq store (store cannot prove it is drained)")
		case n > 0:
			stranded = append(stranded, fmt.Sprintf("dlq store (%d entr(y/ies))", n))
		}
	}

	if len(stranded) == 0 {
		return nil
	}
	if s.allowDestructiveReload {
		if s.logger != nil {
			s.logger.Warn("supervisor: destructive reload FORCED by WithAllowDestructiveReload; "+
				"discarding durable backlog", "discarded", stranded,
				"attempted_config_version", newCfg.Version)
		}
		return nil
	}
	return fmt.Errorf("bridge: refusing live reload: it would strand durable backlog with no drainer in "+
		"the new configuration [%s]; drain it first or set WithAllowDestructiveReload to discard it",
		strings.Join(stranded, "; "))
}

// reportOrphanedStrand re-queries, AFTER a successful swap, each shared_outbox
// partition that lost its drainer in the new topology and surfaces any residual
// pending records as an OBSERVABLE signal (MetricOutboxStranded + an Error log)
// rather than a silent strand. It is the post-swap complement to
// durableReloadPreflight: the preflight proves the partitions empty at CHECK
// time and refuses otherwise, but a record can still land in the swap window
// (late ingress) or a claimed send can fail. Because store-identity changes are
// already refused, the NEW runtime opens the SAME durable backing and can query
// the orphaned partition key even though it has no drainer for it. Best-effort /
// one-shot: a record still in CLAIMED state is not counted (CountPending is
// pending-only) and surfaces only when it reverts to pending.
func (s *Supervisor) reportOrphanedStrand(ctx context.Context, newRt *runtime.Runtime, oldCfg, newCfg *ports.BridgeConfig) {
	// Only meaningful when the outbox store is RETAINED but some partition lost
	// its drainer. Full outbox removal is already refused (backlog present) or
	// force-discarded (operator consent) — nothing to observe there.
	if newRt == nil || oldCfg == nil || newCfg == nil ||
		oldCfg.Stores.Outbox == nil || newCfg.Stores.Outbox == nil {
		return
	}
	oldS := sharedOutboxPartitionSessions(oldCfg)
	newS := sharedOutboxPartitionSessions(newCfg)

	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preflightDepthBudget)
	defer cancel()

	for sid := range oldS {
		if _, keeps := newS[sid]; keeps {
			continue
		}
		partition := persistence.OutboxPartitionKey(sid, "")
		n, ok, qerr := newRt.OutboxPending(qctx, partition)
		switch {
		case qerr != nil:
			if s.logger != nil {
				s.logger.Warn("supervisor: could not verify orphaned outbox partition drained after swap",
					"partition", partition, "error", qerr,
					"config_version", newCfg.Version)
			}
		case !ok:
			// Depth capability absent: cannot verify, not necessarily stranded.
			// The preflight already failed closed on this at CHECK time, so a
			// reload that reached here either proved empty or was force-discarded.
		case n > 0:
			if s.logger != nil {
				s.logger.Error("supervisor: reload STRANDED durable records: partition lost its drainer "+
					"and still holds pending records after swap; drain manually or restore a route/session for it",
					"partition", partition, "pending", n,
					"attempted_config_version", newCfg.Version)
			}
			if s.metrics != nil {
				s.metrics.Counter(shared.MetricOutboxStranded, int64(n),
					shared.Tag{Key: shared.TagKeyPartition, Value: partition})
			}
		}
	}
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
