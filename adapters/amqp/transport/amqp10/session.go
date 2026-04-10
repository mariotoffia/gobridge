package amqp10

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	reconnectInitial    = 1 * time.Second
	reconnectMax        = 30 * time.Second
	reconnectMultiplier = 2.0
	eventChannelSize    = 16
)

// Session implements ports.Session for AMQP 1.0, owning the broker
// connection and AMQP session. It provides automatic reconnection
// with exponential backoff and health monitoring.
type Session struct {
	opts    SessionOptions
	mode    domain.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter
	dial    dialFunc

	mu        sync.Mutex
	conn      amqpConn
	amqpSess  *amqp.Session
	events    chan ports.SessionEvent
	closed    bool
	connected bool
	starting  bool
	plan      *domain.SessionPlan

	// stopMonitor cancels the background health-monitoring goroutine.
	stopMonitor context.CancelFunc
	// monitorDone is closed when the monitor goroutine exits.
	monitorDone chan struct{}
	// reconnectCh is signalled by notifyDisconnect to trigger immediate reconnect.
	reconnectCh chan struct{}
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an AMQP 1.0 Session from the given options.
func NewSession(opts SessionOptions, mode domain.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	opts.applyDefaults()
	return &Session{
		opts:        opts,
		mode:        mode,
		logger:      logger,
		metrics:     m,
		dial:        defaultDial,
		events:      make(chan ports.SessionEvent, eventChannelSize),
		reconnectCh: make(chan struct{}, 1),
	}
}

// Conn returns the underlying AMQP connection. Receivers and senders
// use this to check connection liveness.
func (s *Session) Conn() amqpConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// AMQPSession returns the underlying *amqp.Session for link creation.
func (s *Session) AMQPSession() *amqp.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.amqpSess
}

// Start connects to the AMQP 1.0 broker, creates an AMQP session, and
// starts a background goroutine to monitor connection health.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrUnavailable.WithMessage("amqp10: session already closed")
	}
	if s.conn != nil {
		s.mu.Unlock()
		return nil
	}
	if s.starting {
		s.mu.Unlock()
		return nil
	}
	s.starting = true
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: session connecting",
			"address", redactURL(s.opts.Address),
			"session_mode", s.mode,
		)
	}

	connectStart := time.Now()

	if err := s.connect(ctx); err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return err
	}

	elapsed := time.Since(connectStart)
	s.metrics.Timer(domain.MetricAMQP10ConnectLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ContainerID})

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: session connected",
			"address", redactURL(s.opts.Address),
			"connect_latency", elapsed,
		)
	}

	monCtx, monCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	s.stopMonitor = monCancel
	s.monitorDone = done
	s.starting = false
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.monitorLoop(monCtx)
	}()

	return nil
}

func (s *Session) connect(ctx context.Context) error {
	connOpts := &amqp.ConnOptions{
		IdleTimeout:  s.opts.IdleTimeout,
		MaxFrameSize: s.opts.MaxFrameSize,
	}
	if s.opts.ContainerID != "" {
		connOpts.ContainerID = s.opts.ContainerID
	}
	if s.opts.Username != "" {
		connOpts.SASLType = amqp.SASLTypePlain(s.opts.Username, s.opts.Password)
	}

	if s.opts.TLS != nil && s.opts.TLS.Enable {
		tlsCfg, err := BuildTLSConfig(s.opts.TLS)
		if err != nil {
			return domain.ErrInvalidPayload.Wrap(err).
				WithMessage("amqp10: invalid TLS configuration")
		}
		connOpts.TLSConfig = tlsCfg
	}

	connectCtx, connectCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
	defer connectCancel()

	conn, err := s.dial(connectCtx, s.opts.Address, connOpts)
	if err != nil {
		return MapError(err)
	}

	sess, err := conn.NewSession(connectCtx, nil)
	if err != nil {
		_ = conn.Close()
		return MapError(err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if sess != nil {
			_ = sess.Close(connectCtx)
		}
		_ = conn.Close()
		return domain.ErrUnavailable.WithMessage("amqp10: session closed during connect")
	}
	oldConn := s.conn
	oldSess := s.amqpSess
	s.conn = conn
	s.amqpSess = sess
	s.connected = true
	s.mu.Unlock()

	if oldSess != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = oldSess.Close(cleanCtx)
		cleanCancel()
	}
	if oldConn != nil {
		_ = oldConn.Close()
	}

	s.pushEvent(ports.SessionConnected, nil)
	return nil
}

// Reconcile stores the desired SessionPlan. Unlike AMQP 0-9-1, AMQP 1.0
// does not have queue/exchange declare operations — links are created
// lazily when receivers and senders start.
func (s *Session) Reconcile(ctx context.Context, plan domain.SessionPlan) error {
	reconcileStart := time.Now()

	s.mu.Lock()
	s.plan = &plan
	connected := s.connected
	s.mu.Unlock()

	if !connected {
		return domain.ErrUnavailable.WithMessage("amqp10: session not connected")
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: reconcile",
			"subscriptions", len(plan.Subscriptions),
			"publishers", len(plan.Publishers),
		)
	}

	s.pushEvent(ports.SessionReconciled, nil)

	elapsed := time.Since(reconcileStart)
	s.metrics.Timer(domain.MetricAMQP10ReconcileLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ContainerID})

	return nil
}

// Health returns the current health state of the session.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	connected := s.connected
	plan := s.plan
	s.mu.Unlock()

	wantedCount := 0
	if plan != nil {
		wantedCount = len(plan.Subscriptions)
	}

	var sl ports.ServiceLevel
	switch {
	case !connected:
		sl = ports.ServiceLevelNone
	case wantedCount == 0:
		sl = ports.ServiceLevelFull
	default:
		sl = ports.ServiceLevelFull
	}

	return ports.SessionHealth{
		Connected:           connected,
		SubscriptionsWanted: wantedCount,
		SubscriptionsActive: wantedCount,
		Ready:               connected,
		ServiceLevel:        sl,
	}
}

// Events returns the channel on which session lifecycle events are emitted.
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

// Close gracefully disconnects the AMQP 1.0 session. It is safe to call
// Close multiple times.
func (s *Session) Close(ctx context.Context) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: session close initiated",
			"address", redactURL(s.opts.Address))
	}
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: session closing",
			"address", redactURL(s.opts.Address))
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.connected = false
	conn := s.conn
	s.conn = nil
	sess := s.amqpSess
	s.amqpSess = nil
	stopMon := s.stopMonitor
	s.stopMonitor = nil
	done := s.monitorDone
	s.monitorDone = nil
	s.mu.Unlock()

	if stopMon != nil {
		stopMon()
	}
	if done != nil {
		<-done
	}

	var firstErr error
	if sess != nil {
		if err := sess.Close(ctx); err != nil {
			firstErr = MapError(err)
		}
	}
	if conn != nil {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = MapError(err)
		}
	}

	close(s.events)
	return firstErr
}

func (s *Session) pushEvent(t ports.SessionEventType, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}

	ev := ports.SessionEvent{Type: t, Err: err, Timestamp: time.Now()}
	select {
	case s.events <- ev:
	default:
		select {
		case <-s.events:
		default:
		}
		select {
		case s.events <- ev:
		default:
			if logging.TraceEnabled(s.logger) {
				s.logger.Log(context.Background(), logging.LevelTrace,
					"amqp10: event dropped, channel full",
					"event_type", t,
				)
			}
			s.metrics.Counter(domain.MetricAMQP10EventDropped, 1)
		}
	}
}

// monitorLoop watches for connection loss and attempts automatic
// reconnection with exponential backoff.
func (s *Session) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.reconnectCh:
			s.tryReconnect(ctx)
		case <-ticker.C:
			s.tryReconnect(ctx)
		}
	}
}

func (s *Session) tryReconnect(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		s.handleReconnect(ctx)
	} else if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "amqp10: tryReconnect skipped, connection alive")
	}
}

func (s *Session) handleReconnect(ctx context.Context) {
	s.pushEvent(ports.SessionReconnecting, nil)
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelDebug, "amqp10: attempting reconnect",
			"address", redactURL(s.opts.Address))
	}

	delay := s.opts.ReconnectDelay
	if delay <= 0 {
		delay = reconnectInitial
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		connectCtx, connectCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
		err := s.connect(connectCtx)
		connectCancel()

		if err == nil {
			s.metrics.Counter(domain.MetricAMQP10Reconnects, 1,
				domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ContainerID})
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(context.Background(), logging.LevelDebug, "amqp10: reconnected",
					"address", redactURL(s.opts.Address))
			}

			s.mu.Lock()
			plan := s.plan
			s.mu.Unlock()
			if plan != nil {
				reconCtx, reconCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
				reconcileErr := s.Reconcile(reconCtx, *plan)
				reconCancel()
				if reconcileErr != nil {
					if s.logger != nil {
						s.logger.Warn("amqp10: reconcile on reconnect failed",
							"error", reconcileErr)
					}
				}
			}
			return
		}

		s.pushEvent(ports.SessionError, err)

		jitter := time.Duration(float64(delay) * 0.25 * (2*rand.Float64() - 1))
		sleepDur := delay + jitter

		select {
		case <-ctx.Done():
			return
		case <-time.After(sleepDur):
		}

		delay = time.Duration(float64(delay) * reconnectMultiplier)
		if delay > reconnectMax {
			delay = reconnectMax
		}
	}
}

// redactURL masks any userinfo (credentials) in a broker URL so it
// is safe for logging.
func redactURL(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

// notifyDisconnect is called by receivers/senders when they detect
// a link or session error indicating connection loss. It clears the
// connection state so the monitor goroutine triggers reconnection.
// The failedConn parameter identifies which connection instance failed;
// if the session has already reconnected to a new connection, the stale
// notification is ignored to prevent destroying a valid connection.
func (s *Session) notifyDisconnect(failedConn amqpConn, err error) {
	s.mu.Lock()
	if s.closed || s.conn == nil || s.conn != failedConn {
		s.mu.Unlock()
		return
	}

	conn := s.conn
	s.conn = nil
	s.amqpSess = nil
	s.connected = false
	s.mu.Unlock()

	_ = conn.Close()

	var evErr error
	if err != nil {
		evErr = MapError(err)
	}
	s.pushEvent(ports.SessionDisconnected, evErr)

	select {
	case s.reconnectCh <- struct{}{}:
	default:
	}
}
