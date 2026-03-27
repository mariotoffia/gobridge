package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// SessionManager manages the lifecycle of a single session, including
// lease acquisition, renewal, three-phase step-down, and reconciliation.
type SessionManager struct {
	sessionID         string
	session           ports.Session
	leaseStore        ports.LeaseStore
	ownerID           string
	exclusive         bool
	connectAfterLease bool
	plan              domain.SessionPlan
	leaseTTL          time.Duration
	renewInterval     time.Duration
	renewJitter       time.Duration
	maxRenewFails     int
	stepDownGrace     time.Duration
	endpoints         map[string]string
	metrics           ports.MetricsExporter
	audit             ports.AuditLogger
	logger            *slog.Logger

	mu            sync.Mutex
	token         domain.LeaseToken
	hasLease      bool
	connectedOnce bool
}

// NewSessionManagerFromConfig creates a SessionManager from a config struct.
func NewSessionManagerFromConfig(cfg SessionConfig, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger) *SessionManager {
	return newSessionManager(cfg, session, leaseStore, ownerID, logger)
}

func newSessionManagerWithMetrics(cfg SessionConfig, session ports.Session, leaseStore ports.LeaseStore, ownerID string, logger *slog.Logger, metrics ports.MetricsExporter) *SessionManager {
	mgr := newSessionManager(cfg, session, leaseStore, ownerID, logger)
	mgr.metrics = metrics
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
		cfg.RenewInterval = cfg.LeaseTTL / time.Duration(cfg.MaxRenewFails)
		if cfg.RenewInterval < time.Millisecond {
			cfg.RenewInterval = time.Millisecond
		}
		if cfg.RenewInterval >= cfg.LeaseTTL {
			cfg.RenewInterval = cfg.LeaseTTL / 2
			if cfg.RenewInterval < time.Millisecond {
				cfg.RenewInterval = time.Millisecond
			}
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

// Token returns the current lease token and whether the manager holds the lease.
func (m *SessionManager) Token() (domain.LeaseToken, bool) {
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

func (m *SessionManager) runExclusiveDeferred(ctx context.Context) error {
	sessionStarted := false
	iteration := 0
	for {
		token, err := m.acquireLeaseWithRetry(ctx)
		if err != nil {
			return err
		}
		m.setToken(token)

		action := "lease.acquired"
		if iteration > 0 {
			action = "lease.reacquired"
			m.metrics.Counter(domain.MetricLeaseTransfers, 1,
				domain.Tag{Key: domain.TagKeyLeaseID, Value: m.sessionID})
		}
		iteration++
		m.emitLeaseAudit(ctx, action, "success", token, nil)

		m.log(ctx, slog.LevelInfo, "lease acquired (deferred connect)", "version", token.Version)

		if !sessionStarted {
			if err := m.session.Start(ctx); err != nil {
				return err
			}
			sessionStarted = true
		} else {
			health := m.session.Health(ctx)
			if !health.Connected {
				m.log(ctx, slog.LevelWarn, "session disconnected during lease gap, reconnecting")
				if closeErr := m.session.Close(ctx); closeErr != nil {
					m.log(ctx, slog.LevelWarn, "session close failed before restart", "error", closeErr)
				}
				if err := m.session.Start(ctx); err != nil {
					return err
				}
			}
		}

		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			return err
		}

		err = m.renewLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
		m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
	}
}

func (m *SessionManager) runExclusive(ctx context.Context) error {
	iteration := 0
	for {
		token, err := m.acquireLeaseWithRetry(ctx)
		if err != nil {
			return err
		}
		m.setToken(token)

		action := "lease.acquired"
		if iteration > 0 {
			action = "lease.reacquired"
			m.metrics.Counter(domain.MetricLeaseTransfers, 1,
				domain.Tag{Key: domain.TagKeyLeaseID, Value: m.sessionID})
		}
		iteration++
		m.emitLeaseAudit(ctx, action, "success", token, nil)

		m.log(ctx, slog.LevelInfo, "lease acquired", "version", token.Version)

		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			return err
		}

		err = m.renewLoop(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.emitLeaseAudit(ctx, "lease.lost", "failure", token, err)
		m.log(ctx, slog.LevelWarn, "lease lost, will re-acquire", "error", err)
		m.mu.Lock()
		m.hasLease = false
		m.mu.Unlock()
	}
}

func (m *SessionManager) acquireLeaseWithRetry(ctx context.Context) (domain.LeaseToken, error) {
	leaseTag := domain.Tag{Key: domain.TagKeyLeaseID, Value: m.sessionID}
	for {
		start := time.Now()
		token, err := m.leaseStore.Acquire(ctx, m.sessionID, m.ownerID, m.leaseTTL, m.endpoints)
		m.metrics.Timer(domain.MetricLeaseAcquireLatency, time.Since(start), leaseTag)
		if err == nil {
			return token, nil
		}
		m.metrics.Counter(domain.MetricLeaseAcquireFailures, 1, leaseTag)
		m.log(ctx, slog.LevelDebug, "lease acquisition failed, retrying", "error", err)

		select {
		case <-ctx.Done():
			return domain.LeaseToken{}, ctx.Err()
		case <-time.After(m.renewInterval + m.jitter()):
		}
	}
}

func (m *SessionManager) renewLoop(ctx context.Context) error {
	consecutiveFailures := 0
	events := m.session.Events()

	timer := time.NewTimer(m.renewInterval + m.jitter())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := m.handleSessionEvent(ctx, ev); err != nil {
				return err
			}

		case <-timer.C:
			m.mu.Lock()
			token := m.token
			m.mu.Unlock()

			leaseTag := domain.Tag{Key: domain.TagKeyLeaseID, Value: m.sessionID}
			start := time.Now()
			newToken, err := m.leaseStore.Renew(ctx, m.sessionID, token, m.leaseTTL, m.endpoints)
			m.metrics.Timer(domain.MetricLeaseRenewLatency, time.Since(start), leaseTag)

			if err != nil {
				consecutiveFailures++
				m.log(ctx, slog.LevelWarn, "lease renewal failed",
					"failures", consecutiveFailures, "error", err)

				if consecutiveFailures >= m.maxRenewFails {
					m.metrics.Counter(domain.MetricLeaseExpiries, 1, leaseTag)
					return m.stepDown(ctx)
				}
			} else {
				consecutiveFailures = 0
				m.setToken(newToken)
			}

			timer.Reset(m.renewInterval + m.jitter())
		}
	}
}

// stepDown implements the three-phase step-down protocol:
// 1. Stop claiming new outbox entries (clear hasLease)
// 2. Wait grace period for in-flight completions
// 3. Release the lease
func (m *SessionManager) stepDown(ctx context.Context) error {
	m.log(ctx, slog.LevelWarn, "stepping down from lease")

	m.mu.Lock()
	token := m.token
	m.hasLease = false
	m.mu.Unlock()

	m.emitLeaseAudit(ctx, "lease.stepdown", "success", token, nil)

	graceCtx, graceCancel := context.WithTimeout(context.Background(), m.stepDownGrace)
	defer graceCancel()

	select {
	case <-graceCtx.Done():
	case <-ctx.Done():
	}

	if m.leaseStore != nil {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		if err := m.leaseStore.Release(releaseCtx, m.sessionID, token); err != nil {
			m.emitLeaseAudit(ctx, "lease.release", "failure", token, err)
			m.log(ctx, slog.LevelWarn, "lease release failed during step-down", "error", err)
		} else {
			m.emitLeaseAudit(ctx, "lease.release", "success", token, nil)
		}
	}

	return errors.New("lease lost after renewal failures")
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
				return err
			}
		}
	}
}

func (m *SessionManager) handleSessionEvent(ctx context.Context, ev ports.SessionEvent) error {
	sessionTag := domain.Tag{Key: domain.TagKeySessionID, Value: m.sessionID}
	switch ev.Type {
	case ports.SessionConnected:
		m.log(ctx, slog.LevelInfo, "session connected")
		if m.connectedOnce {
			m.metrics.Counter(domain.MetricMQTTReconnects, 1, sessionTag)
		}
		m.connectedOnce = true
		if err := m.session.Reconcile(ctx, m.plan); err != nil {
			m.log(ctx, slog.LevelError, "reconcile failed on reconnect", "error", err)
			m.metrics.Counter(domain.MetricReconcileFailures, 1, sessionTag)
			return fmt.Errorf("reconcile on reconnect: %w", err)
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

func (m *SessionManager) setToken(token domain.LeaseToken) {
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

func (m *SessionManager) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if m.logger == nil {
		return
	}
	allArgs := append([]any{"session_id", m.sessionID}, args...)
	m.logger.Log(ctx, level, msg, allArgs...)
}

func (m *SessionManager) emitLeaseAudit(ctx context.Context, action, outcome string, token domain.LeaseToken, err error) {
	detail := map[string]any{
		"owner_id": m.ownerID,
		"version":  token.Version,
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	m.audit.Log(ctx, ports.AuditEvent{
		Timestamp:  time.Now().UTC(),
		Action:     action,
		Actor:      m.ownerID,
		Resource:   "lease",
		ResourceID: m.sessionID,
		Outcome:    outcome,
		Detail:     detail,
	})
}
