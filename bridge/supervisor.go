package bridge

import (
	"bytes"
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

// DeferReason names WHY a SwapEvent was deferred. A deferred swap is
// committed-not-applied, but the follow-up an operator must take differs per
// cause, so the cause travels with the event instead of being assumed by the
// consumer.
type DeferReason string

const (
	// DeferReasonPaused: the bridge is paused by an admin StopBridge. The config
	// is recorded as the resume target and takes effect on the next StartBridge.
	DeferReasonPaused DeferReason = "paused"

	// DeferReasonRolloutPending: the delta was handed to the coordinated cluster
	// rollout barrier. It is proposed cluster-wide and this node swaps only when
	// the barrier reaches Committed (or discards it on Abort) — no operator
	// action is required, unlike a pause.
	DeferReasonRolloutPending DeferReason = "rollout_pending"
)

// deferReasonWhenPaused returns the pause reason only when the event is
// actually deferred, so a non-deferred event never carries a stale cause.
func deferReasonWhenPaused(paused bool) DeferReason {
	if paused {
		return DeferReasonPaused
	}
	return ""
}

// SwapEvent is emitted on each reconfiguration attempt via the
// OnSwap callback.
type SwapEvent struct {
	OldConfig *ports.BridgeConfig
	NewConfig *ports.BridgeConfig
	SwapMode  SwapMode
	Error     error
	Duration  time.Duration

	// Deferred is true when the Supervisor deliberately did NOT apply NewConfig
	// and instead recorded it as the desired state to resume later — an admin
	// StopBridge pause, or a hand-off to the coordinated rollout barrier. A
	// deferred event carries Error == nil (it is not a failure): the config is
	// committed and will take effect on resume/commit, so an in-band applier MUST
	// treat it as committed-not-applied (no rollback), not as a successful swap.
	Deferred bool

	// DeferReason names the cause when Deferred is true (empty otherwise).
	// Consumers MUST branch on it rather than assuming a pause: rendering a
	// rollout-pending deferral as "paused by admin" sends an operator to the
	// wrong runbook.
	DeferReason DeferReason
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
	rollout             *rolloutBarrier
	rolloutDriver       *ClusterRolloutDriver
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
	// intends to DISCARD that backlog.
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
	// kills the process mid-swap.
	swapping bool
	// degraded records a non-terminal config-machinery problem while the
	// current runtime keeps serving: live reconfiguration is no longer
	// available (the config change stream closed unexpectedly — Finding 1),
	// or a committed reload never converged on the broker within its
	// activation budget (applied-but-not-converged, — set and
	// cleared by the post-swap convergence watch). degradedReason
	// distinguishes the two in deep health. degradedByConvergence marks the
	// convergence watch as the owner of the current degraded state, so a
	// LATER watcher (a new swap or an admin resume) or a deliberate
	// StopBridge can clear a stale applied-but-not-converged alarm it did
	// not set itself — while never touching a foreign degraded cause.
	degraded              bool
	degradedReason        string
	degradedByConvergence bool

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
	// defer cancels that ctx.
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
// for the durable-reload preflight.
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

// WithClusterRollout wires the coordinated cluster rollout barrier
// (design cluster-config-rollout-protocol.md §6). It is opt-in and additive: a
// clustered deployment that does not configure it keeps the ADR 0012 refusal for
// every live reload, and an incompletely configured barrier is ignored (the
// refusal stands) rather than half-applied.
//
// With the barrier wired, a live-safe delta in a deployment whose config sets
// cluster.rollout: coordinated is proposed to the shared rollout store and
// deferred locally instead of refused; this node swaps only when the barrier
// commits.
//
// The full barrier is wired and proven end to end: the applier loop
// (build-without-swap, Ack / Nack, swap on Committed), the lease-elected
// coordinator loop (commit / abort under a fencing token), the joiner startup
// rule, and — when Encode/Decode are supplied — the durable last-committed config
// artifact that lets a (re)joining member boot on the committed config after an
// abort and a member that missed a commit reconcile to it. Coverage is the
// bridge/ unit tests, the integration cluster-rollout suite over real DynamoDB,
// and the long-running multi-process UC-CR proofs (design §10 / Phase 5).
//
// It is opt-in. The Supervisor is one RolloutHost; the shipped file-based
// bootstrap.App is the other (design Phase 6), both driving the same barrier
// through a bridge.ClusterRolloutDriver. A deployment that does not wire it keeps
// the ADR 0012 whole-cohort replacement procedure.
//
// Wiring stores the barrier as s.rollout (kept reachable for in-package tests) and
// builds the driver eagerly with the Supervisor as its host, so boot resolution
// and proposals work before Run; the drive's clock and metrics are supplied later,
// at Run, when they are finalised.
func WithClusterRollout(cfg ClusterRolloutConfig) SupervisorOption {
	return func(s *Supervisor) {
		// Assign both fields together, unconditionally, so s.rolloutDriver == nil
		// stays EXACTLY equivalent to s.rollout == nil — the fail-closed invariant
		// every delegator relies on. A repeated WithClusterRollout whose later call
		// is half-wired must clear a previously-built driver, not leave it stale.
		s.rollout = newRolloutBarrier(cfg)
		if s.rollout == nil {
			s.rolloutDriver = nil
			return
		}
		s.rolloutDriver = newRolloutDriver(supervisorRolloutHost{s}, s.rollout)
	}
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
	return cloneConfigSnapshot(s.cfg)
}

// cloneConfigSnapshot is used only for already-accepted Supervisor state. Its
// freeze contract was validated before publication, so failure here is defensive.
func cloneConfigSnapshot(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	cloned, err := cloneConfigForBuild(cfg)
	if err != nil {
		return nil
	}
	return cloned
}

// Run builds and starts the initial runtime, then watches the
// changes channel for new configs. It blocks until ctx is
// cancelled, the changes channel is closed, or an unrecoverable
// error occurs during initial startup. Config change failures are
// logged but do not stop Run.
func (s *Supervisor) Run(ctx context.Context, initial *ports.BridgeConfig, changes <-chan *ports.BridgeConfig) error {
	// Capture the process-lifetime context so admin StartBridge can start a
	// resumed runtime under it rather than an ephemeral request ctx.
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()

	appliedInitial, err := cloneConfigForBuild(initial)
	if err != nil {
		return fmt.Errorf("supervisor: initial config freeze: %w", err)
	}
	// Coordinated-rollout startup gate (design §6): this node must be in its own
	// cohort roster, and — the joiner rule — must not boot onto a config the
	// barrier has not committed. The operator's change is durably in the config
	// source BEFORE the barrier decides, so an aborted or still-undecided candidate
	// would otherwise start this node on a config no other member runs. With the
	// committed-config artifact wired, this SUBSTITUTES the durable committed config
	// for such a boot config rather than refusing (design Phase-4 residual); without
	// it, the conservative refusal stands. Runs before anything is built.
	appliedInitial, err = s.resolveCoordinatedBoot(ctx, appliedInitial)
	if err != nil {
		return err
	}
	initialIdentities, err := snapshotDurableSessionIdentities(appliedInitial)
	if err != nil {
		return fmt.Errorf("supervisor: initial durable session identity preflight: %w", err)
	}
	// Bounded like every reload build: an unbounded initial construction lets a
	// hung external call (NewSession against a partitioned broker, a credential
	// resolve that never returns) block Run forever, with no runtime, no health
	// surface and no terminal signal. The shipped command wraps this in its own
	// outer wait; a custom composition root got nothing.
	rt, err := s.buildRuntimeBounded(ctx, appliedInitial)
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

	// Start the coordinated-rollout barrier drive (applier + coordinator) once
	// the runtime is serving. It is a no-op unless the deployment opted in AND a
	// barrier is wired, so a non-clustered or ADR-0012 deployment starts nothing.
	stopDrive := s.startRolloutDrive(ctx)
	if stopDrive == nil {
		stopDrive = func(context.Context) {}
	}
	// Backstop only: every shutdown path below goes through shutdown(), which
	// stops the drive FIRST. This defer covers a path that returns without it.
	defer func() { stopDrive(context.Background()) }()

	// shutdown drains the bridge in the one order that is safe: stop the barrier
	// drive, THEN stop the runtime. The drive can be mid-swap (a committed
	// rollout applies on ITS goroutine, not this one), so draining first would
	// stop the old runtime while a replacement is still being built and started —
	// publishing a fully-started runtime that nothing references and nothing will
	// ever Stop. stopDrive waits for that goroutine to unwind, which is bounded by
	// the swap deadline, and it is idempotent so the defer above is harmless.
	shutdown := func() error {
		// One shutdown budget covers the drive AND the runtime drain: the drive
		// is waited on first (a committed rollout applies on ITS goroutine, so
		// draining first could start a replacement nothing would ever stop), but
		// a barrier store call that never returns must not hold SIGTERM ahead of
		// the drain. drain_timeout bounds the wait; the drive is already
		// cancelled and unwinds on its own afterwards.
		driveCtx, driveCancel := context.WithTimeout(
			context.WithoutCancel(ctx), s.drainTimeoutFrom(s.Config()))
		stopDrive(driveCtx)
		driveCancel()
		return s.stopCurrent(ctx)
	}

	if changes == nil {
		<-ctx.Done()
		return shutdown()
	}

	filtered := s.strategy.Filter(ctx, changes)

	for {
		select {
		case <-ctx.Done():
			return shutdown()
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
					return shutdown()
				default:
				}
				s.markDegraded("config change stream closed; live reconfiguration unavailable")
				if s.logger != nil {
					s.logger.Error("supervisor: config change stream closed unexpectedly; " +
						"keeping current runtime serving (live reconfiguration disabled until restart)")
				}
				<-ctx.Done()
				return shutdown()
			}
			s.apply(ctx, newCfg)
		}
	}
}

// markDegraded records that the supervisor can no longer apply config
// changes even though the current runtime keeps serving. Not terminal.
// It takes ownership of the degraded state away from any convergence
// watcher (degradedByConvergence=false) so a watcher can never clear a
// foreign cause recorded here.
func (s *Supervisor) markDegraded(reason string) {
	s.mu.Lock()
	s.degraded = true
	s.degradedReason = reason
	s.degradedByConvergence = false
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
	// window must not trip the backstop.
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
// The runtime is single-use once stopped; StartBridge builds a
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
	// A deliberate pause also invalidates any applied-but-not-converged
	// observation: the convergence watcher abandons on pause (it is
	// pause-aware), and a mark it already set would otherwise scream "revert
	// the config" about a bridge that is merely paused — clear the
	// convergence-owned state (never a foreign degraded cause).
	s.mu.Lock()
	s.paused = true
	clearedConvergence := s.degradedByConvergence
	if clearedConvergence {
		s.degraded = false
		s.degradedReason = ""
		s.degradedByConvergence = false
	}
	s.mu.Unlock()
	if clearedConvergence {
		s.emitConfigDegradedGauge(false)
	}
	return stopErr
}

// StartBridge resumes a deliberately-stopped bridge by building and starting a
// FRESH runtime from the current config. The runtime is single-use, so a
// stopped runtime cannot be restarted in place — "resume" means build+start a
// new instance (/bridge/start after /bridge/stop must succeed, not
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
	if rt != nil {
		// The runtime is not running, but that covers two very different states:
		// a deliberate StopBridge (already torn down; Stop is an idempotent
		// no-op) and a component-failure trip, which flips healthy and cancels
		// the work context but leaves running=true and never closes anything.
		// Publishing a replacement over the second state orphaned its broker
		// sessions, store handles and leases for the process lifetime AND
		// disarmed the terminal backstop, because Terminal() then reads the new
		// runtime. Release it first.
		s.stopAbandoned(ctx, rt, cfg)
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
	// instant the handler returns and cancels that ctx.
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
	// An admin resume is a fresh Start with the same false-success shape as a
	// reload commit: Start returns before the transport ever reaches the
	// broker, and broker-side state may have changed during the very
	// maintenance window StopBridge exists for (rotated credentials, ACL
	// edits). Watch the resumed runtime exactly like a committed swap
	// under the long-lived Run context — the admin-request ctx
	// dies with the handler.
	s.watchPostSwapConvergence(baseCtx, newRt, cfg)
	return nil
}

// apply performs a config-driven reconfiguration, subject to every preflight
// including the clustered-reload guard.
func (s *Supervisor) apply(ctx context.Context, newCfg *ports.BridgeConfig) {
	s.applyConfig(ctx, newCfg, false)
}

// applyBarrierCommitted applies a config the coordinated cluster rollout barrier
// has ALREADY committed cluster-wide, so it bypasses the clustered-reload guard
// (design §6 applier). The bypass is safe for exactly this caller: the barrier
// only reaches Committed after every member of the frozen epoch acked a
// validated, built candidate, which is a strictly stronger check than the guard
// — the guard's job is to stop an UNCOORDINATED per-process reload, and this one
// is the coordinated commit itself. Every other preflight (durable session
// identity, store identity, orphaned backlog, paused) still runs.
func (s *Supervisor) applyBarrierCommitted(ctx context.Context, newCfg *ports.BridgeConfig) {
	s.applyConfig(ctx, newCfg, true)
}

func (s *Supervisor) applyConfig(ctx context.Context, newCfg *ports.BridgeConfig, barrierCommitted bool) {
	// Serialize against admin StartBridge/StopBridge so two control-plane
	// operations never both build and publish a runtime (HIGH: control-plane
	// TOCTOU leaks a fully-started runtime).
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	// Freeze the proposal exactly once at the Supervisor boundary. Every
	// preflight, build, stored snapshot, and recovery decision below uses this
	// same immutable content; SwapEvent keeps the original pointer solely for
	// config-manager acknowledgement correlation.
	frozenCfg, freezeErr := cloneConfigForBuild(newCfg)
	if freezeErr != nil {
		s.mu.RLock()
		oldCfg := s.cfg
		s.mu.RUnlock()
		if s.logger != nil {
			s.logger.Error("supervisor: refusing config whose plugin snapshot cannot be frozen", "error", freezeErr)
		}
		s.emitConfigReload(false)
		if s.onSwap != nil {
			s.safeOnSwap(SwapEvent{OldConfig: cloneConfigSnapshot(oldCfg), NewConfig: newCfg, Error: freezeErr})
		}
		return
	}

	// No-op detection and the clustered-reload guard run FIRST — before the
	// durable-identity preflight, the paused handling, and any Plan/build/store
	// query or Stop — so no clustered reload can slip through a paused or
	// destructive path and a genuine no-op never needlessly rebuilds
	// a runtime.
	s.mu.RLock()
	oldCfgForGuard := s.cfg
	pausedForGuard := s.paused
	s.mu.RUnlock()

	// (1) No-op: a re-emit whose CANONICAL content matches the applied config is
	// not a reconfiguration. This is WHOLE-CONFIG detection and it is the only
	// delta that avoids a restart: every accepted change — however narrow, down to
	// a single route's address — rebuilds the entire runtime, so unrelated routes'
	// broker sessions are torn down and re-established with them (full-session
	// reload, the accepted tradeoff; diff-based reload is out of
	// scope). Operators must batch config changes and budget the loss window this
	// opens for QoS 0 and ephemeral sessions on every reload;
	// bridge/supervisor_reload_test.go pins the semantics.
	//
	// Acknowledge a no-op WITHOUT a swap so the running runtime,
	// applied config, version, and reference are all preserved. onSwap MUST still
	// fire (an in-band applier blocks on the result for this exact config pointer);
	// mirror the paused/live distinction with Deferred so a no-op re-emit received
	// while paused resolves as recorded-not-applied, not applied.
	if oldCfgForGuard != nil && configContentEqual(oldCfgForGuard, frozenCfg) {
		if s.logger != nil {
			s.logger.Debug("supervisor: reload matches the running config; no swap performed",
				"config_version", frozenCfg.Version)
		}
		if s.onSwap != nil {
			s.safeOnSwap(SwapEvent{
				OldConfig:   cloneConfigSnapshot(oldCfgForGuard),
				NewConfig:   newCfg,
				Deferred:    pausedForGuard,
				DeferReason: deferReasonWhenPaused(pausedForGuard),
			})
		}
		return
	}

	// (2) Cluster guard (lifted for coordinated rollout — design
	// cluster-config-rollout-protocol.md §6; the three outcomes live in
	// rollout_guard.go). Skipped only for a config the barrier already committed
	// cluster-wide, which is a strictly stronger check than the guard performs.
	// Not a no-op (checked above).
	if !barrierCommitted && s.clusterReloadGuard(ctx, oldCfgForGuard, frozenCfg, newCfg) {
		return
	}
	// clusterReloadProceed: continue to normal apply.

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
			s.safeOnSwap(SwapEvent{OldConfig: cloneConfigSnapshot(oldCfgAtPreflight), NewConfig: newCfg, Error: identityErr})
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
		// (paused path). The operator drains before pausing, or forces it
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
					OldConfig: cloneConfigSnapshot(oldCfg),
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
				OldConfig:   cloneConfigSnapshot(oldCfg),
				NewConfig:   newCfg,
				Deferred:    true,
				DeferReason: DeferReasonPaused,
			})
		}
		return
	}

	// Mark a swap in progress for its whole duration so Terminal() reports false
	// while the old runtime is being stopped and the new one built — a swap is
	// not death. Cleared on every exit path (success and error).
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
	// cross-node config barrier (by design) a rolling reload lets one
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
	// durable-reload preflight below, not merely warned.
	if s.logger != nil {
		if removed := removedStoreRoles(oldCfg, frozenCfg); slices.Contains(removed, "lease") {
			s.logger.Warn("supervisor: reload removes the lease store; its fencing state is discarded "+
				"(holds no message backlog)", "attempted_config_version", frozenCfg.Version)
		}
	}

	// A live reload must NEVER strand durable backlog. Pending depth cannot prove
	// safety here: it excludes claimed records, DynamoDB's supporting GSI is
	// eventually consistent, and ingress can persist after a point-in-time read.
	// Refuse every outbox/DLQ removal or orphaned partition unless the operator
	// explicitly authorizes destructive discard.
	var preflightErr error
	if oldRt != nil && identErr == nil && leaseIdentErr == nil {
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
	case oldRt != nil && leaseIdentErr != nil:
		// Refuse the swap and keep the old runtime serving under the CURRENT
		// session_id; changing a lease-bearing exclusive session_id is a
		// cluster-wide ownership invariant that a per-process reload cannot roll
		// safely. The operator must stop/restart all nodes together.
		err = leaseIdentErr
		if s.logger != nil {
			s.logger.Error("supervisor: refusing reload that changes a lease-bearing exclusive "+
				"session_id (cluster ownership invariant); roll it with a full stop/restart of all "+
				"nodes or force with WithAllowDestructiveReload",
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
	case mode == SwapPrepareCommit:
		newRt, err = s.applyPrepareCommit(ctx, oldRt, oldCfg, frozenCfg)
	default:
		newRt, err = s.applyOverlap(ctx, oldRt, oldCfg, frozenCfg)
	}

	// Observable residual: the preflight proved the orphaned
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
		OldConfig: cloneConfigSnapshot(oldCfg),
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
		s.degradedByConvergence = false
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

		// Success above means built + Start returned — NOT that the transport
		// ever reached the broker (MQTT dials/reconciles in background
		// goroutines). Watch the committed runtime until its sessions
		// genuinely converge and surface applied-but-not-converged as a
		// distinct degraded state otherwise.
		s.watchPostSwapConvergence(ctx, newRt, frozenCfg)
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
			// exclusive-identity conflicts, stale lease writes. Refuse the swap
			// and release the built-but-unstarted new runtime.
			//
			// The old runtime cannot be retained either: Stop has no early error
			// return, so by the time it reports a failure it has already
			// cancelled the work context and closed managers, sessions and
			// stores — and a stopped runtime is single-use. Keeping it as the
			// current one left the process bridging nothing behind a green
			// /live. Wedge instead (ADR-0004): Terminal() trips, /live fails
			// closed, and the orchestrator restarts the process with
			// freshly-built transports, which is also the only thing that clears
			// the hung residue.
			if s.logger != nil {
				s.logger.Error("supervisor: old runtime stop failed; wedging rather than serving a torn-down runtime", "error", stopErr)
			}
			s.stopAbandoned(ctx, newRt, newCfg)
			s.wedgeAfterFailedStop(stopErr)
			return nil, fmt.Errorf("stop old runtime: %w", stopErr)
		}
	}

	if err := newRt.Start(ctx); err != nil {
		if s.logger != nil {
			s.logger.Error("supervisor: new runtime start failed, attempting recovery with old config", "error", err)
		}
		// The new runtime built its sessions/receivers/stores but never
		// started; abandoning it here leaks every connection set forever.
		// Stop it (idempotent, bounded) before recovering (Finding 2 /).
		s.stopAbandoned(ctx, newRt, newCfg)
		s.recoverOldOrWedge(ctx, oldCfg)
		return nil, fmt.Errorf("start: %w", err)
	}

	return newRt, nil
}

// stopAbandoned stops a built-but-abandoned runtime with a bounded, detached
// context so its prep-opened sessions, receivers, and store handles are
// released instead of leaked (Finding 2 / contract). Stop is idempotent
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

// wedgeAfterFailedStop drops the torn-down runtime and enters the terminal
// wedged state. It is the answer to a Runtime.Stop that reported an error during
// a swap: that runtime has already released (or hung on) everything it owned and
// is single-use, so there is nothing left to serve with. Terminal() then trips,
// /live fails closed, and the composition-root backstop restarts the process —
// the only thing that can clear hung plugin residue (ADR-0004).
func (s *Supervisor) wedgeAfterFailedStop(stopErr error) {
	s.mu.Lock()
	s.rt = nil
	s.wedged = true
	s.mu.Unlock()
	s.markDegraded(fmt.Sprintf("old runtime stop failed during reload; no runtime is serving: %v", stopErr))
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
	// forever. On deadline the error routes into recoverOldOrWedge below.
	//
	// prepare and complete get SEPARATE deadlines because the old runtime's Stop
	// sits between them and is allowed to consume the whole drain budget. One
	// deadline spanning both charged construction for that drain, so a
	// slow-but-successful stop handed complete() a spent context and the reload
	// failed deterministically on every retry.
	prepCtx, prepCancel := s.swapPhaseCtx(ctx)
	defer prepCancel()
	prep, err := builder.prepare(prepCtx)
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
			// double consumption. Refuse the swap and release the prep-opened
			// store handles. The old runtime is already torn down and single-use
			// (see applyOverlap), so it cannot be retained either: wedge so the
			// orchestrator restarts the process.
			if s.logger != nil {
				s.logger.Error("supervisor: old runtime stop failed; wedging rather than serving a torn-down runtime", "error", stopErr)
			}
			builder.closeStoreHandles(prep.stores)
			s.wedgeAfterFailedStop(stopErr)
			return nil, fmt.Errorf("stop old runtime: %w", stopErr)
		}
	}

	completeCtx, completeCancel := s.swapPhaseCtx(ctx)
	defer completeCancel()
	newRt, err := builder.complete(completeCtx, prep)
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
	// forces the serialized prepare-commit swap.
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
	frozenOld, err := cloneConfigForBuild(oldCfg)
	if err != nil {
		return err
	}
	oldIdentities, err := snapshotDurableSessionIdentities(frozenOld)
	if err != nil {
		return err
	}
	frozenNew, err := cloneConfigForBuild(newCfg)
	if err != nil {
		return err
	}
	newIdentities, err := snapshotDurableSessionIdentities(frozenNew)
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
		if ports.IsNilPluginConfig(session.Config) {
			return nil, fmt.Errorf("bridge: durable session %q identity config is nil", session.ID)
		}
		if _, ok := session.Config.(ports.FreezableConfig); !ok {
			return nil, fmt.Errorf("bridge: durable session %q identity config lacks adapter-owned freeze capability", session.ID)
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
			return fmt.Errorf("bridge: refusing live reload: durable session %q was removed, renamed, or lost its identity capability; its broker state and managed filter history could be stranded; externally drain and exact-unsubscribe every managed filter before cutover", sessionID)
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

// roleStorageIdentifiedConfig resolves role-dependent defaults before durable
// identity comparison (for example one DynamoDB config shared by four roles).
type roleStorageIdentifiedConfig interface {
	StorageIdentityForRole(role string) string
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
		{"managed_subscriptions", oldCfg.Stores.ManagedSubscriptions, newCfg.Stores.ManagedSubscriptions},
	}
	for _, r := range roles {
		if r.name == "managed_subscriptions" && r.old != nil && r.new == nil {
			return fmt.Errorf("bridge: refusing live reload: managed subscription store was removed while persistent/exclusive MQTT subscriptions remain; exact broker-filter history would be unavailable")
		}
		if r.old == nil || r.new == nil {
			continue
		}
		if storeIdentity(r.name, r.old) != storeIdentity(r.name, r.new) {
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
// cross-node config barrier (by design) instance A can run session_id=X
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
// non-HTTP exclusive routes (the shared_outbox rule only covers HTTP ingress,
// and the destructiveReloadShape only covers STORE identity).
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
		{"managed_subscriptions", oldCfg.Stores.ManagedSubscriptions, newCfg.Stores.ManagedSubscriptions},
	}
	for _, r := range roles {
		if r.old != nil && r.new == nil {
			removed = append(removed, r.name)
		}
	}
	return removed
}

// strandReportBudget bounds the best-effort pending-depth query after an
// explicitly forced destructive reload. Correctness never depends on this
// pending-only observation.
const strandReportBudget = 5 * time.Second

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

// configContentEqual reports whether two configs are the SAME deployment for the
// purpose of the no-op / clustered-reload decision. It mirrors the project's
// authoritative content identity — config.configFingerprint — rather than a
// structural reflect.DeepEqual:
//
//   - It canonicalises each config to the JSON projection of its EXPORTED fields
//     with secrets revealed (via shared.RevealSecrets, so a secret-only edit is
//     still a change), then appends the JSON of every decoded PluginConfig in the
//     same fixed order the fingerprint uses (the blueprint tags the Config fields
//     json:"-", so a plugin-only change would otherwise be invisible).
//   - It therefore INCLUDES BridgeConfig.Version: the project treats a version
//     bump as a content change (the fingerprint json-encodes version), so a
//     version-only reload is NOT a no-op and, when clustered, is still refused.
//   - It IGNORES opaque unexported plugin internals (mutexes, clients, sync.Once):
//     json marshals only exported fields, so an equivalent RE-EMIT of the same
//     decoded config compares equal instead of being falsely rejected.
//
// On any marshal error it FAILS CLOSED (reports not-equal), so an
// un-canonicalisable config is treated as a change rather than a spurious no-op.
func configContentEqual(a, b *ports.BridgeConfig) bool {
	ab, aok := configCanonicalBytes(a)
	bb, bok := configCanonicalBytes(b)
	if !aok || !bok {
		return false
	}
	return bytes.Equal(ab, bb)
}

// configCanonicalBytes returns the canonical content projection of cfg used by
// configContentEqual (see there). ok is false on a marshal error so the caller
// can fail closed. Both callers pass already-frozen configs, so the projection is
// stable across repeated calls.
func configCanonicalBytes(cfg *ports.BridgeConfig) ([]byte, bool) {
	if cfg == nil {
		return nil, true
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(shared.RevealSecrets(cfg)); err != nil {
		return nil, false
	}
	ok := true
	visitPluginConfigs(cfg, func(pc ports.PluginConfig) {
		if !ok {
			return
		}
		// enc.Encode writes each value followed by a newline, so the ordered
		// stream of plugin payloads is self-delimiting; a nil PluginConfig encodes
		// as "null", preserving its structural position.
		if err := enc.Encode(shared.RevealSecrets(pc)); err != nil {
			ok = false
		}
	})
	if !ok {
		return nil, false
	}
	return buf.Bytes(), true
}

// visitPluginConfigs visits every decoded PluginConfig on the blueprint in the
// same fixed, deterministic order as config.forEachPluginConfig (stores, then
// sessions, receivers + their topics, senders, bindings). The order is part of
// the content contract: two configs differing only in the position of an
// otherwise-identical plugin option must canonicalise differently. Bridge cannot
// import the config package (arch-lint), so this mirrors that traversal locally.
func visitPluginConfigs(cfg *ports.BridgeConfig, fn func(ports.PluginConfig)) {
	for _, sc := range []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions} {
		if sc == nil {
			continue
		}
		fn(sc.Config)
	}
	for i := range cfg.Sessions {
		fn(cfg.Sessions[i].Config)
	}
	for i := range cfg.Receivers {
		fn(cfg.Receivers[i].Config)
		for j := range cfg.Receivers[i].Topics {
			fn(cfg.Receivers[i].Topics[j].Config)
		}
	}
	for i := range cfg.Senders {
		fn(cfg.Senders[i].Config)
	}
	for i := range cfg.Bindings {
		fn(cfg.Bindings[i].Config)
	}
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
// a config that would strand backlog on resume (paused path).
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

// durableReloadPreflight fails closed on every live-reload shape that can
// orphan durable records: removing an outbox/DLQ store or removing the last
// drainer for an outbox partition. A pending-depth query is not proof of zero
// nonterminal records because it excludes claimed records, may be eventually
// consistent, and races late ingress. Safe removal therefore requires an
// externally quiesced stop/migration; the only live override is explicit
// destructive discard via WithAllowDestructiveReload.
//
// The lease store is deliberately not gated here: it holds fencing tokens, not
// message backlog. Store identity changes are rejected separately before this
// preflight.
func (s *Supervisor) durableReloadPreflight(_ context.Context, _ *runtime.Runtime, oldCfg, newCfg *ports.BridgeConfig) error {
	if oldCfg == nil || newCfg == nil {
		return nil
	}

	var hazards []string
	if oldCfg.Stores.Outbox != nil {
		oldSessions := sharedOutboxPartitionSessions(oldCfg)
		newSessions := sharedOutboxPartitionSessions(newCfg)
		if newCfg.Stores.Outbox == nil {
			hazards = append(hazards, "outbox store removal")
			newSessions = nil
		}
		for sid := range oldSessions {
			if _, keeps := newSessions[sid]; keeps {
				continue
			}
			partition := persistence.OutboxPartitionKey(sid, "")
			hazards = append(hazards, fmt.Sprintf("outbox partition %s loses its drainer", partition))
		}
	}

	if oldCfg.Stores.DLQ != nil && newCfg.Stores.DLQ == nil {
		hazards = append(hazards, "dlq store removal")
	}

	if len(hazards) == 0 {
		return nil
	}
	if s.allowDestructiveReload {
		if s.logger != nil {
			s.logger.Warn("supervisor: destructive reload FORCED by WithAllowDestructiveReload; "+
				"durable records may be discarded", "hazards", hazards,
				"attempted_config_version", newCfg.Version)
		}
		return nil
	}
	return fmt.Errorf("bridge: refusing live reload: it can strand durable records [%s]; "+
		"a pending-depth read cannot prove safety because claimed records, eventually-consistent indexes, "+
		"and late ingress are not excluded. Quiesce ingress and perform a stopped migration, or set "+
		"WithAllowDestructiveReload to explicitly discard records",
		strings.Join(hazards, "; "))
}

// reportOrphanedStrand adds best-effort observability after an explicitly forced
// destructive reload that retained the same outbox store but removed a
// partition's drainer. Correctness does not depend on this pending-only query:
// non-forced orphaning reloads are rejected unconditionally above.
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

	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), strandReportBudget)
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
func storeIdentity(role string, sc *ports.StoreConfig) string {
	if sc == nil {
		return ""
	}
	if si, ok := sc.Config.(roleStorageIdentifiedConfig); ok {
		return sc.Type + "|id=" + si.StorageIdentityForRole(role)
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
