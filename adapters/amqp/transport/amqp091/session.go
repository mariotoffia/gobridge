package amqp091

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Session implements ports.Session for AMQP 0-9-1, owning a single
// broker connection with automatic reconnection and exchange/queue/binding
// declaration during Reconcile.
type Session struct {
	opts    SessionOptions
	mode    domain.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter
	dial    dialFunc

	mu        sync.Mutex
	conn      amqpConnection
	events    chan ports.SessionEvent
	closed    bool
	connected bool
	starting  bool

	plan       *domain.SessionPlan
	activeSubs map[string]bool // queue names successfully declared

	// cancel stops the reconnection goroutine on Close.
	cancel context.CancelFunc
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an AMQP 0-9-1 Session from the given options.
// metrics may be nil; a no-op exporter is used in that case.
func NewSession(opts SessionOptions, mode domain.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	opts.applyDefaults()
	return &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		metrics:    m,
		dial:       defaultDialFromOpts(opts),
		events:     make(chan ports.SessionEvent, 16),
		activeSubs: make(map[string]bool),
	}
}

// brokerURL returns the broker URL with credentials injected from opts.
func (s *Session) brokerURL() string {
	return injectCredentials(s.opts.BrokerURL, s.opts.Username, s.opts.Password)
}

// safeBrokerURL returns the broker URL with credentials redacted for logging.
func (s *Session) safeBrokerURL() string {
	return redactURL(s.brokerURL())
}

// Connection returns the underlying AMQP connection. Receiver and Sender
// use this to create channels.
func (s *Session) Connection() amqpConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// Start connects to the AMQP broker and starts the reconnection loop.
// Calling Start on an already-started session is a no-op (idempotent).
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrUnavailable.WithMessage("amqp091: session already closed")
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

	logging.DebugContext(s.logger, ctx, "amqp091: session connecting",
		"broker", s.safeBrokerURL(),
		"session_mode", s.mode,
	)
	connectStart := time.Now()

	connectCtx, connectCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
	defer connectCancel()

	conn, err := s.dialWithTimeout(connectCtx)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		logging.DebugContext(s.logger, ctx, "amqp091: connect failed",
			"broker", s.safeBrokerURL(), "error", err)
		return MapError(err)
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	s.mu.Lock()
	s.conn = conn
	s.connected = true
	s.starting = false
	s.cancel = bgCancel
	s.mu.Unlock()

	safeBroker := s.safeBrokerURL()
	elapsed := time.Since(connectStart)
	s.metrics.Timer(domain.MetricAMQP091ConnectLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: safeBroker})
	logging.DebugContext(s.logger, ctx, "amqp091: session connected",
		"broker", safeBroker, "connect_latency", elapsed)

	// Defer SessionConnected until after reconcile when a plan is present,
	// so consumers don't act on a connection that isn't fully set up.
	s.mu.Lock()
	plan := s.plan
	s.mu.Unlock()

	if plan != nil {
		if err := s.reconcile(ctx, conn, *plan); err != nil {
			logging.DebugContext(s.logger, ctx, "amqp091: reconcile on start failed",
				"broker", safeBroker, "error", err)
		}
	}

	s.pushEvent(ports.SessionConnected, nil)

	go s.reconnectLoop(bgCtx)

	return nil
}

// Reconcile declares exchanges, queues, and bindings from the SessionPlan.
// SubscriptionPlan.Topic maps to the queue name. Supported options per
// subscription: "exchange", "routing_key", "exchange_type" (default "direct"),
// "durable", "auto_delete".
func (s *Session) Reconcile(ctx context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	hasPriorPlan := s.plan != nil
	if len(plan.Subscriptions) > 0 || !hasPriorPlan {
		s.plan = &plan
	}
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return domain.ErrUnavailable.WithMessage("session not started")
	}
	if len(plan.Subscriptions) == 0 && hasPriorPlan {
		return nil
	}

	return s.reconcile(ctx, conn, plan)
}

func (s *Session) reconcile(ctx context.Context, conn amqpConnection, plan domain.SessionPlan) error {
	reconcileStart := time.Now()

	logging.DebugContext(s.logger, ctx, "amqp091: reconcile",
		"subscriptions", len(plan.Subscriptions),
		"publishers", len(plan.Publishers),
	)

	ch, err := conn.Channel()
	if err != nil {
		return MapError(err)
	}
	defer ch.Close()

	for _, sub := range plan.Subscriptions {
		queueName := sub.Topic
		exchangeName, _ := optString(sub.Options, "exchange")
		routingKey, _ := optString(sub.Options, "routing_key")
		exchangeType := "direct"
		if et, ok := optString(sub.Options, "exchange_type"); ok {
			exchangeType = et
		}
		durable, _ := optBool(sub.Options, "durable")
		autoDelete, _ := optBool(sub.Options, "auto_delete")

		if exchangeName != "" {
			if err := ch.ExchangeDeclare(exchangeName, exchangeType, durable, autoDelete, false, false, nil); err != nil {
				return MapError(err)
			}
		}

		_, err := ch.QueueDeclare(queueName, durable, autoDelete, false, false, nil)
		if err != nil {
			return MapError(err)
		}

		if exchangeName != "" {
			if routingKey == "" {
				routingKey = queueName
			}
			if err := ch.QueueBind(queueName, routingKey, exchangeName, false, nil); err != nil {
				return MapError(err)
			}
		}

		s.mu.Lock()
		s.activeSubs[queueName] = true
		s.mu.Unlock()
	}

	for _, pub := range plan.Publishers {
		exchangeName := pub.Topic
		exchangeType := "direct"
		if et, ok := optString(pub.Options, "exchange_type"); ok {
			exchangeType = et
		}
		durable, _ := optBool(pub.Options, "durable")
		autoDelete, _ := optBool(pub.Options, "auto_delete")

		if exchangeName != "" {
			if err := ch.ExchangeDeclare(exchangeName, exchangeType, durable, autoDelete, false, false, nil); err != nil {
				return MapError(err)
			}
		}
	}

	elapsed := time.Since(reconcileStart)
	s.metrics.Timer(domain.MetricAMQP091ReconcileLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.safeBrokerURL()})
	logging.DebugContext(s.logger, ctx, "amqp091: reconcile done",
		"subscriptions", len(plan.Subscriptions),
		"publishers", len(plan.Publishers),
		"duration", elapsed,
	)

	s.pushEvent(ports.SessionReconciled, nil)
	return nil
}

// Health returns the current health state of the session.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	connected := s.connected && s.conn != nil
	plan := s.plan
	activeCount := len(s.activeSubs)
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
	case activeCount >= wantedCount:
		sl = ports.ServiceLevelFull
	case activeCount == 0:
		sl = ports.ServiceLevelNone
	default:
		sl = ports.ServiceLevelDegraded
	}

	return ports.SessionHealth{
		Connected:           connected,
		SubscriptionsWanted: wantedCount,
		SubscriptionsActive: activeCount,
		Ready:               connected,
		ServiceLevel:        sl,
	}
}

// Events returns the channel on which session lifecycle events are emitted.
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

// Close gracefully closes the AMQP connection and stops the reconnection
// loop. It is safe to call Close multiple times.
func (s *Session) Close(_ context.Context) error {
	logging.Debug(s.logger, "amqp091: session closing",
		"broker", s.safeBrokerURL())

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.connected = false
	conn := s.conn
	s.conn = nil
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	var closeErr error
	if conn != nil {
		closeErr = conn.Close()
	}

	close(s.events)

	if closeErr != nil {
		return MapError(closeErr)
	}
	return nil
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
			s.metrics.Counter(domain.MetricAMQP091EventDropped, 1)
		}
	}
}

const (
	reconnectInitial = 1 * time.Second
	reconnectMax     = 30 * time.Second
	reconnectMult    = 2.0
)

func (s *Session) reconnectLoop(ctx context.Context) {
	for {
		s.mu.Lock()
		conn := s.conn
		closed := s.closed
		s.mu.Unlock()

		if closed {
			return
		}
		if conn == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}

		notifyCh := conn.NotifyClose(make(chan *amqp.Error, 1))

		select {
		case <-ctx.Done():
			return
		case amqpErr, ok := <-notifyCh:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			s.connected = false
			s.conn = nil
			s.activeSubs = make(map[string]bool)
			s.mu.Unlock()

			var connErr error
			if ok && amqpErr != nil {
				connErr = amqpErr
			}

			s.pushEvent(ports.SessionDisconnected, MapError(connErr))
			logging.Debug(s.logger, "amqp091: connection lost, reconnecting",
				"broker", s.safeBrokerURL(), "error", connErr)

			s.doReconnect(ctx)
		}
	}
}

func (s *Session) doReconnect(ctx context.Context) {
	delay := s.opts.ReconnectDelay
	if delay == 0 {
		delay = reconnectInitial
	}

	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		s.pushEvent(ports.SessionReconnecting, nil)
		safeBroker := s.safeBrokerURL()
		s.metrics.Counter(domain.MetricAMQP091Reconnects, 1,
			domain.Tag{Key: domain.TagKeySessionID, Value: safeBroker})

		jitter := time.Duration(rand.Int64N(int64(delay) / 4))
		sleepDur := delay + jitter

		select {
		case <-ctx.Done():
			return
		case <-time.After(sleepDur):
		}

		connectCtx, connectCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
		conn, err := s.dialWithTimeout(connectCtx)
		connectCancel()

		if err != nil {
			logging.Debug(s.logger, "amqp091: reconnect attempt failed",
				"broker", safeBroker,
				"attempt", attempt+1,
				"error", err,
			)
			delay = time.Duration(math.Min(
				float64(delay)*reconnectMult,
				float64(reconnectMax),
			))
			continue
		}

		s.mu.Lock()
		s.conn = conn
		s.connected = true
		plan := s.plan
		s.mu.Unlock()

		logging.Debug(s.logger, "amqp091: reconnected",
			"broker", safeBroker, "attempt", attempt+1)

		if plan != nil {
			reconCtx, reconCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
			if err := s.reconcile(reconCtx, conn, *plan); err != nil {
				if s.logger != nil {
					s.logger.Warn("amqp091: reconcile on reconnect failed",
						"error", err)
				}
			}
			reconCancel()
		}

		s.pushEvent(ports.SessionConnected, nil)

		return
	}
}

func (s *Session) dialWithTimeout(ctx context.Context) (amqpConnection, error) {
	type result struct {
		conn amqpConnection
		err  error
	}

	ch := make(chan result, 1)
	go func() {
		conn, err := s.dial(s.brokerURL())
		ch <- result{conn, err}
	}()

	select {
	case <-ctx.Done():
		// The goroutine's dial may still succeed after we give up.
		// Drain the channel in the background and close any leaked connection.
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("amqp091: dial timeout: %w", ctx.Err())
	case r := <-ch:
		return r.conn, r.err
	}
}
