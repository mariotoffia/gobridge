package session

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// LeaseState describes the lifecycle state of a lease transition.
type LeaseState int

const (
	LeaseStateNone LeaseState = iota
	LeaseStateAcquired
	LeaseStateRenewed
	LeaseStateLost
	LeaseStateSteppedDown
	LeaseStateReleased
	// LeaseStateReconcileFailed signals a reconcile-on-reconnect failure while
	// the lease was still held and renewing. It is emitted INSTEAD OF
	// LeaseStateLost so a subscription blip is never mis-observed as a lease
	// loss/transfer; the session is surfaced to superviseSession for isolated
	// restart (C7-N2). Appended last to keep the existing iota values stable.
	LeaseStateReconcileFailed
)

// LeaseStateEvent is emitted whenever the lease state changes.
type LeaseStateEvent struct {
	State     LeaseState
	Token     persistence.LeaseToken
	Timestamp time.Time
	Err       error
}

// Manager manages the lifecycle of a single session, including
// lease acquisition, renewal, three-phase step-down, and reconciliation.
type Manager struct {
	sessionID         string
	session           ports.Session
	leaseStore        ports.LeaseStore
	ownerID           string
	exclusive         bool
	connectAfterLease bool
	plan              connectivity.SessionPlan
	leaseTTL          time.Duration
	renewInterval     time.Duration
	renewJitter       time.Duration
	acquirePoll       time.Duration
	renewCallTimeout  time.Duration
	maxRenewFails     int
	stepDownGrace     time.Duration
	endpoints         map[string]string
	metrics           ports.MetricsExporter
	audit             ports.AuditLogger
	logger            *slog.Logger
	clk               clock.Clock

	mu            sync.Mutex
	token         persistence.LeaseToken
	hasLease      bool
	connectedOnce atomic.Bool

	leaseEvents     chan LeaseStateEvent
	leaseEventDrops atomic.Uint64
}

// NewFromConfig creates a Manager from a Config.
func NewFromConfig(cfg Config, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *Manager {
	return newManager(cfg, session, leaseStore, ownerID, logger)
}

// NewWithMetrics creates a Manager from a Config with an explicit
// metrics exporter and clock. Used by composition roots that want to
// pre-wire instrumentation and a deterministic clock before Run.
func NewWithMetrics(cfg Config, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger, metrics ports.MetricsExporter, clk clock.Clock) *Manager {
	mgr := newManager(cfg, session, leaseStore, ownerID, logger)
	if metrics != nil {
		mgr.metrics = metrics
	}
	if clk != nil {
		mgr.clk = clk
	}
	return mgr
}

func newManager(cfg Config, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *Manager {
	defaults := DefaultConfig(cfg.SessionID, cfg.Exclusive)
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaults.LeaseTTL
	}
	if cfg.MaxRenewFails <= 0 {
		cfg.MaxRenewFails = defaults.MaxRenewFails
	}
	// Derive the renewal cadence from the TTL when the operator supplies only
	// LeaseTTL (C3: bridge/convert.go no longer seeds DefaultConfig, so this is
	// now the production path). deriveRenewInterval/deriveRenewJitter target the
	// MaxRenewFails-th renew at ~75% of the TTL, folding jitter into the
	// expiry-margin invariant so renew×maxFails+jitter < ttl with margin.
	//
	// Jitter is derived ONLY when the renew interval was also unset. If the
	// caller pinned RenewInterval it is explicit enough that a zero RenewJitter
	// is honored as "no jitter" (deterministic cadence) rather than reinterpreted
	// as "derive"; an operator wanting spread on a pinned interval sets the
	// lease_renew_jitter field. The C3 production path leaves both zero, so both
	// are derived.
	renewIntervalDerived := cfg.RenewInterval <= 0
	if renewIntervalDerived {
		cfg.RenewInterval = deriveRenewInterval(cfg.LeaseTTL, cfg.MaxRenewFails)
	}
	if cfg.RenewJitter < 0 {
		cfg.RenewJitter = 0
	}
	if cfg.RenewJitter == 0 && renewIntervalDerived {
		cfg.RenewJitter = deriveRenewJitter(cfg.RenewInterval)
	}
	// Defensively enforce the expiry-margin invariant even for explicit configs
	// so a hand-tuned combination can never produce a renew span that reaches
	// the TTL (Config.Validate reports the same violation as a hard error).
	renewInterval, renewJitter, clamped := clampRenewTimings(cfg.LeaseTTL, cfg.RenewInterval, cfg.RenewJitter, cfg.MaxRenewFails)
	if clamped && logger != nil {
		logger.Warn("session lease timings clamped to satisfy the expiry-margin invariant",
			"session_id", cfg.SessionID,
			"lease_ttl", cfg.LeaseTTL,
			"requested_renew_interval", cfg.RenewInterval,
			"requested_renew_jitter", cfg.RenewJitter,
			"clamped_renew_interval", renewInterval,
			"clamped_renew_jitter", renewJitter,
			"max_renew_fails", cfg.MaxRenewFails,
		)
	}
	cfg.RenewInterval = renewInterval
	cfg.RenewJitter = renewJitter
	if cfg.AcquirePollInterval <= 0 {
		cfg.AcquirePollInterval = deriveAcquirePollInterval(cfg.RenewInterval, cfg.LeaseTTL)
	}
	if cfg.RenewCallTimeout <= 0 {
		cfg.RenewCallTimeout = deriveRenewCallTimeout(cfg.RenewInterval)
	}
	if cfg.StepDownGrace <= 0 {
		cfg.StepDownGrace = defaults.StepDownGrace
	}

	return &Manager{
		sessionID:         cfg.SessionID,
		session:           session,
		leaseStore:        leaseStore,
		ownerID:           ownerID,
		exclusive:         cfg.Exclusive,
		connectAfterLease: cfg.ConnectAfterLease,
		plan:              cfg.Plan,
		leaseTTL:          cfg.LeaseTTL,
		renewInterval:     cfg.RenewInterval,
		renewJitter:       cfg.RenewJitter,
		acquirePoll:       cfg.AcquirePollInterval,
		renewCallTimeout:  cfg.RenewCallTimeout,
		maxRenewFails:     cfg.MaxRenewFails,
		stepDownGrace:     cfg.StepDownGrace,
		metrics:           &ports.NoopExporter{},
		audit:             ports.NoopAuditLogger{},
		logger:            logger,
		clk:               clock.System,
		leaseEvents:       make(chan LeaseStateEvent, leaseEventBuffer),
	}
}

// Run starts the session and manages its lifecycle. For exclusive sessions,
// it acquires the lease and runs the renewal loop. It blocks until ctx is
// cancelled or an unrecoverable error occurs.
//
// When ConnectAfterLease is set, the session is not started until the
// lease has been acquired, preventing premature broker connections that
// would displace the current owner.
func (m *Manager) Run(ctx context.Context) error {
	if m.exclusive && m.leaseStore != nil && m.connectAfterLease {
		return m.runExclusiveDeferred(ctx)
	}

	if err := m.session.Start(ctx); err != nil {
		return err
	}

	if m.exclusive && m.leaseStore != nil {
		return m.runExclusive(ctx)
	}

	if err := m.session.Reconcile(ctx, m.plan); err != nil {
		return err
	}

	return m.handleEvents(ctx)
}

// SetMetrics sets the metrics exporter on the manager.
// Must be called before Run; not safe for concurrent use with Run.
func (m *Manager) SetMetrics(metrics ports.MetricsExporter) {
	m.metrics = metrics
}

// SetAudit sets the audit logger on the manager.
// Must be called before Run; not safe for concurrent use with Run.
func (m *Manager) SetAudit(audit ports.AuditLogger) {
	m.audit = audit
}

// SetEndpoints sets the cluster endpoints that identify the local
// instance to peers. Must be called before Run; not safe for concurrent
// use with Run.
func (m *Manager) SetEndpoints(endpoints map[string]string) {
	m.endpoints = endpoints
}

// leaseEventBuffer is the capacity of the LeaseStateChanged channel. Lease
// transitions are low-frequency but observability-critical; the buffer is sized
// so a slow consumer rarely fills it, and pushLeaseEvent coalesces on overflow
// rather than silently dropping (finding L15).
const leaseEventBuffer = 64

// LeaseStateChanged returns a channel that receives lease state transitions.
func (m *Manager) LeaseStateChanged() <-chan LeaseStateEvent { return m.leaseEvents }

// LeaseEventDrops returns the number of lease state transitions that had to
// overwrite an older, still-unconsumed event because the buffer was full. A
// non-zero value means a consumer is not draining LeaseStateChanged promptly;
// the newest transitions are always preserved.
func (m *Manager) LeaseEventDrops() uint64 { return m.leaseEventDrops.Load() }

// pushLeaseEvent delivers a lease transition to LeaseStateChanged. On a full
// buffer it does NOT silently drop the newest event: it evicts the OLDEST
// buffered event (the least relevant — state has moved on since) and enqueues
// the new one, incrementing a drop counter for observability. The current
// state is more valuable to a consumer than a stale one, so overwrite-oldest
// preserves the freshest transitions (finding L15).
func (m *Manager) pushLeaseEvent(state LeaseState, token persistence.LeaseToken, err error) {
	evt := LeaseStateEvent{State: state, Token: token, Timestamp: m.clk.Now(), Err: err}
	for {
		select {
		case m.leaseEvents <- evt:
			return
		default:
		}
		// Buffer full: evict the oldest event and retry. The loop tolerates a
		// concurrent consumer draining between the failed send and the evict.
		select {
		case <-m.leaseEvents:
			m.leaseEventDrops.Add(1)
		default:
		}
	}
}

// Token returns the current lease token and whether the manager holds the lease.
func (m *Manager) Token() (persistence.LeaseToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token, m.hasLease
}

// Close closes the underlying session.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	hadLease := m.hasLease
	token := m.token
	m.hasLease = false
	m.mu.Unlock()

	if hadLease && m.leaseStore != nil {
		if err := m.leaseStore.Release(ctx, m.sessionID, token); err != nil {
			m.log(ctx, slog.LevelWarn, "lease release failed during close", "error", err)
		}
	}
	return m.session.Close(ctx)
}

func (m *Manager) handleEvents(ctx context.Context) error {
	events := m.session.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// An unexpected Events-channel close (not driven by ctx
				// cancellation) means the underlying session died. Treat it as
				// a session FAILURE so superviseSession restarts this one
				// session in isolation, instead of the previous silent "clean
				// stop" that let a non-exclusive session die permanently with
				// no restart and no error (finding L14).
				return fmt.Errorf("runtime: session-manager: %w", errSessionEventsClosed)
			}
			if err := m.handleSessionEvent(ctx, ev); err != nil {
				return fmt.Errorf("runtime: session-manager: handle session event: %w", err)
			}
		}
	}
}

func (m *Manager) handleSessionEvent(ctx context.Context, ev ports.SessionEvent) error {
	sessionTag := shared.Tag{Key: shared.TagKeySessionID, Value: m.sessionID}
	switch ev.Type {
	case ports.SessionConnected:
		m.log(ctx, slog.LevelInfo, "session connected")
		if m.connectedOnce.Swap(true) {
			m.metrics.Counter(shared.MetricMQTTReconnects, 1, sessionTag)
		}
		if logging.DebugEnabled(m.logger) {
			m.logger.Log(ctx, logging.LevelDebug, "session reconcile",
				"session_id", m.sessionID,
				"subscription_count", len(m.plan.Subscriptions),
			)
		}
		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			m.log(ctx, slog.LevelError, "reconcile failed on reconnect", "error", err)
			m.metrics.Counter(shared.MetricReconcileFailures, 1, sessionTag)
			return fmt.Errorf("runtime: session-manager: reconcile on reconnect: %w", err)
		}

	case ports.SessionDisconnected:
		m.log(ctx, slog.LevelWarn, "session disconnected", "error", ev.Err)

	case ports.SessionReconnecting:
		m.log(ctx, slog.LevelInfo, "session reconnecting")

	case ports.SessionError:
		m.log(ctx, slog.LevelError, "session error", "error", ev.Err)
	}
	return nil
}

func (m *Manager) setToken(token persistence.LeaseToken) {
	m.mu.Lock()
	m.token = token
	m.hasLease = true
	m.mu.Unlock()
}

func (m *Manager) jitter() time.Duration {
	if m.renewJitter <= 0 {
		return 0
	}
	half := m.renewJitter / 2
	return time.Duration(rand.Int64N(int64(m.renewJitter))) - half
}

// clampedInterval returns renewInterval + jitter, floored at 1ms to
// prevent near-zero or negative timer durations when jitter exceeds
// the renewal interval.
func (m *Manager) clampedInterval() time.Duration {
	return max(m.renewInterval+m.jitter(), time.Millisecond)
}

// acquirePollDelay returns the standby lease-acquisition poll interval with a
// small de-synchronising jitter. Standbys poll on a DEDICATED cadence
// (m.acquirePoll), decoupled from the (typically much larger) owner renew
// interval, so a takeover is not delayed by up to a full renew interval
// (finding M6). The ±25% jitter spreads competing standbys so they do not
// stampede the lease store on expiry. Floored at 1ms.
func (m *Manager) acquirePollDelay() time.Duration {
	base := m.acquirePoll
	if base <= 0 {
		base = m.renewInterval
	}
	if base <= 0 {
		return time.Millisecond
	}
	spread := base / 2 // full width of the jitter window (±25% of base)
	var j time.Duration
	if spread > 0 {
		j = time.Duration(rand.Int64N(int64(spread))) - spread/2
	}
	return max(base+j, time.Millisecond)
}

func (m *Manager) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if m.logger == nil || !m.logger.Enabled(ctx, level) {
		return
	}
	allArgs := append([]any{"session_id", m.sessionID}, args...)
	m.logger.Log(ctx, level, msg, allArgs...)
}

func (m *Manager) emitLeaseAudit(ctx context.Context, action, outcome string, token persistence.LeaseToken, err error) {
	detail := map[string]any{
		"owner_id": m.ownerID,
		"version":  token.Version,
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	m.audit.Log(ctx, ports.AuditEvent{
		Timestamp:  m.clk.Now().UTC(),
		Action:     action,
		Actor:      m.ownerID,
		Resource:   "lease",
		ResourceID: m.sessionID,
		Outcome:    outcome,
		Detail:     detail,
	})
}
