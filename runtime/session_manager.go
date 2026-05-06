package runtime

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
)

// LeaseStateEvent is emitted whenever the lease state changes.
type LeaseStateEvent struct {
	State     LeaseState
	Token     persistence.LeaseToken
	Timestamp time.Time
	Err       error
}

// SessionManager manages the lifecycle of a single session, including
// lease acquisition, renewal, three-phase step-down, and reconciliation.
type SessionManager struct {
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

	leaseEvents chan LeaseStateEvent
}

// NewSessionManagerFromConfig creates a SessionManager from a config struct.
func NewSessionManagerFromConfig(cfg SessionConfig, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *SessionManager {
	return newSessionManager(cfg, session, leaseStore, ownerID, logger)
}

func newSessionManagerWithMetrics(cfg SessionConfig, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger, metrics ports.MetricsExporter, clk clock.Clock) *SessionManager {
	mgr := newSessionManager(cfg, session, leaseStore, ownerID, logger)
	mgr.metrics = metrics
	if clk != nil {
		mgr.clk = clk
	}
	return mgr
}

func newSessionManager(cfg SessionConfig, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *SessionManager {
	defaults := DefaultSessionConfig(cfg.SessionID, cfg.Exclusive)
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = defaults.LeaseTTL
	}
	if cfg.MaxRenewFails == 0 {
		cfg.MaxRenewFails = defaults.MaxRenewFails
	}
	if cfg.RenewInterval == 0 {
		cfg.RenewInterval = max(cfg.LeaseTTL/time.Duration(cfg.MaxRenewFails), time.Millisecond)
		if cfg.RenewInterval >= cfg.LeaseTTL {
			cfg.RenewInterval = max(cfg.LeaseTTL/2, time.Millisecond)
		}
	}
	if cfg.StepDownGrace == 0 {
		cfg.StepDownGrace = defaults.StepDownGrace
	}

	return &SessionManager{
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
		maxRenewFails:     cfg.MaxRenewFails,
		stepDownGrace:     cfg.StepDownGrace,
		metrics:           &ports.NoopExporter{},
		audit:             ports.NoopAuditLogger{},
		logger:            logger,
		clk:               clock.System,
		leaseEvents:       make(chan LeaseStateEvent, 16),
	}
}

// Run starts the session and manages its lifecycle. For exclusive sessions,
// it acquires the lease and runs the renewal loop. It blocks until ctx is
// cancelled or an unrecoverable error occurs.
//
// When ConnectAfterLease is set, the session is not started until the
// lease has been acquired, preventing premature broker connections that
// would displace the current owner.
func (m *SessionManager) Run(ctx context.Context) error {
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

// SetMetrics sets the metrics exporter on the session manager.
// Must be called before Run; not safe for concurrent use with Run.
func (m *SessionManager) SetMetrics(metrics ports.MetricsExporter) {
	m.metrics = metrics
}

// SetAudit sets the audit logger on the session manager.
// Must be called before Run; not safe for concurrent use with Run.
func (m *SessionManager) SetAudit(audit ports.AuditLogger) {
	m.audit = audit
}

// LeaseStateChanged returns a channel that receives lease state transitions.
func (m *SessionManager) LeaseStateChanged() <-chan LeaseStateEvent { return m.leaseEvents }

func (m *SessionManager) pushLeaseEvent(state LeaseState, token persistence.LeaseToken, err error) {
	evt := LeaseStateEvent{State: state, Token: token, Timestamp: m.clk.Now(), Err: err}
	select {
	case m.leaseEvents <- evt:
	default:
	}
}

// Token returns the current lease token and whether the manager holds the lease.
func (m *SessionManager) Token() (persistence.LeaseToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token, m.hasLease
}

// Close closes the underlying session.
func (m *SessionManager) Close(ctx context.Context) error {
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

func (m *SessionManager) handleEvents(ctx context.Context) error {
	events := m.session.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := m.handleSessionEvent(ctx, ev); err != nil {
				return fmt.Errorf("runtime: session-manager: handle session event: %w", err)
			}
		}
	}
}

func (m *SessionManager) handleSessionEvent(ctx context.Context, ev ports.SessionEvent) error {
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

func (m *SessionManager) setToken(token persistence.LeaseToken) {
	m.mu.Lock()
	m.token = token
	m.hasLease = true
	m.mu.Unlock()
}

func (m *SessionManager) jitter() time.Duration {
	if m.renewJitter <= 0 {
		return 0
	}
	half := m.renewJitter / 2
	return time.Duration(rand.Int64N(int64(m.renewJitter))) - half
}

// clampedInterval returns renewInterval + jitter, floored at 1ms to
// prevent near-zero or negative timer durations when jitter exceeds
// the renewal interval.
func (m *SessionManager) clampedInterval() time.Duration {
	return max(m.renewInterval+m.jitter(), time.Millisecond)
}

func (m *SessionManager) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if m.logger == nil || !m.logger.Enabled(ctx, level) {
		return
	}
	allArgs := append([]any{"session_id", m.sessionID}, args...)
	m.logger.Log(ctx, level, msg, allArgs...)
}

func (m *SessionManager) emitLeaseAudit(ctx context.Context, action, outcome string, token persistence.LeaseToken, err error) {
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
