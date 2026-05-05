package amqp091

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
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
	clk     clock.Clock

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
	// bgDone is closed when the background reconnect goroutine exits.
	bgDone chan struct{}
	// startDone is created when a Start call begins the connection
	// process and is closed by that caller once dial completes (with
	// startErr capturing the outcome). Concurrent callers wait on it
	// instead of returning success while the connection is still being
	// established.
	startDone chan struct{}
	startErr  error

	// reconnected signals the reconnectLoop when doReconnect has
	// re-established the connection, replacing the 250ms poll.
	reconnected chan struct{}

	// eventSubs holds per-subscriber channels for fan-out delivery of
	// session lifecycle events. Reading from the legacy Events() channel
	// drains the shared buffer, so every Receiver and observer that
	// needs reconnect notifications must Subscribe to receive its own
	// independent stream.
	eventSubs []chan ports.SessionEvent

	// liveCreds captures the latest applied credentials, consulted on
	// every (re)connect via brokerURL(). The reconnect goroutine reads
	// it while the mutex is not held for the dial, so it races with
	// ApplyCredentials by design: the next dial attempt picks up the
	// new values, and in-flight dials with stale credentials are fine
	// (they just fail auth and retry).
	liveCreds amqpCredentials
}

// amqpCredentials is the mutable subset of SessionOptions that can be
// rotated at runtime. TLS material requires a full reconnect with a
// new dialer and is out of scope here; see TLSMaterial on
// domain.CredentialSet for the future extension point.
type amqpCredentials struct {
	Username string
	Password string
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
		opts:        opts,
		mode:        mode,
		logger:      logger,
		metrics:     m,
		dial:        defaultDialFromOpts(opts),
		clk:         opts.Clock,
		events:      make(chan ports.SessionEvent, 16),
		activeSubs:  make(map[string]bool),
		reconnected: make(chan struct{}, 1),
		liveCreds: amqpCredentials{
			Username: opts.Username,
			Password: opts.Password,
		},
	}
}

// brokerURL returns the broker URL with credentials injected from the
// most recently applied credential material (see ApplyCredentials).
func (s *Session) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

func (s *Session) brokerURL() string {
	s.mu.Lock()
	u, p := s.liveCreds.Username, s.liveCreds.Password
	s.mu.Unlock()
	return injectCredentials(s.opts.BrokerURL, u, p)
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
//
// Concurrent callers do not race the connection: the first caller takes
// the "starting" slot and performs the dial; later callers block on the
// same outcome (success, dial error, or session-closed-during-start) so
// that "Start returned nil" reliably means "session is connected".
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
		startDone := s.startDone
		s.mu.Unlock()
		if startDone == nil {
			return nil
		}
		select {
		case <-startDone:
			s.mu.Lock()
			err := s.startErr
			closed := s.closed
			s.mu.Unlock()
			if closed && err == nil {
				return domain.ErrUnavailable.WithMessage("amqp091: session closed during start")
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.startErr = nil
	startDone := s.startDone
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		close(startDone)
	}()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp091: session connecting",
			"broker", s.safeBrokerURL(),
			"session_mode", s.mode,
		)
	}
	connectStart := s.clock().Now()

	connectCtx, connectCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
	defer connectCancel()

	conn, err := s.dialWithTimeout(connectCtx)
	if err != nil {
		mappedErr := MapError(err)
		s.mu.Lock()
		s.startErr = mappedErr
		s.mu.Unlock()
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "amqp091: connect failed",
				"broker", s.safeBrokerURL(), "error", err)
		}
		return mappedErr
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.conn = conn
	s.connected = true
	s.cancel = bgCancel
	s.bgDone = done
	s.mu.Unlock()

	safeBroker := s.safeBrokerURL()
	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(domain.MetricAMQP091ConnectLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: safeBroker})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp091: session connected",
			"broker", safeBroker, "connect_latency", elapsed)
	}

	// Defer SessionConnected until after reconcile when a plan is present,
	// so consumers don't act on a connection that isn't fully set up.
	s.mu.Lock()
	plan := s.plan
	s.mu.Unlock()

	if plan != nil {
		if err := s.reconcile(ctx, conn, *plan); err != nil {
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(ctx, logging.LevelDebug, "amqp091: reconcile on start failed",
					"broker", safeBroker, "error", err)
			}
		}
	}

	s.pushEvent(ports.SessionConnected, nil)

	go func() {
		defer close(done)
		s.reconnectLoop(bgCtx)
	}()

	return nil
}

// Reconcile declares exchanges, queues, and bindings from the SessionPlan.
// SubscriptionPlan.Topic maps to the queue name. Supported options per
// subscription: "exchange", "routing_key", "exchange_type" (default "direct"),
// "durable", "auto_delete".
//
// The supplied plan unconditionally replaces the prior plan, including
// publisher-only updates and plans that intentionally clear all
// subscriptions. Pass an empty SessionPlan{} to clear; pass a plan with
// only Publishers to declare exchanges without changing subscriptions
// from a prior call (the prior plan's subscriptions are dropped).
func (s *Session) Reconcile(ctx context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	s.plan = &plan
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return domain.ErrUnavailable.WithMessage("session not started")
	}

	return s.reconcile(ctx, conn, plan)
}

func (s *Session) reconcile(ctx context.Context, conn amqpConnection, plan domain.SessionPlan) error {
	reconcileStart := s.clock().Now()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp091: reconcile",
			"subscriptions", len(plan.Subscriptions),
			"publishers", len(plan.Publishers),
		)
	}

	// activeSubs is recomputed from the new plan rather than appended to
	// so that subscriptions removed from the plan are reflected in
	// Health() reporting.
	s.mu.Lock()
	s.activeSubs = make(map[string]bool, len(plan.Subscriptions))
	s.mu.Unlock()

	for _, sub := range plan.Subscriptions {
		if err := s.declareSubscription(conn, sub); err != nil {
			return err
		}
	}

	for _, pub := range plan.Publishers {
		if err := s.declarePublisher(conn, pub); err != nil {
			return err
		}
	}

	elapsed := s.clock().Since(reconcileStart)
	s.metrics.Timer(domain.MetricAMQP091ReconcileLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.safeBrokerURL()})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp091: reconcile done",
			"subscriptions", len(plan.Subscriptions),
			"publishers", len(plan.Publishers),
			"duration", elapsed,
		)
	}

	s.pushEvent(ports.SessionReconciled, nil)
	return nil
}

// declareSubscription opens a fresh AMQP channel for a single subscription's
// declarations (exchange, queue, bind). A separate channel per subscription
// ensures that a PRECONDITION_FAILED error on one declaration does not
// poison the channel for subsequent subscriptions in the same plan, since
// AMQP closes the channel on any soft error.
func (s *Session) declareSubscription(conn amqpConnection, sub domain.SubscriptionPlan) error {
	queueName := sub.Topic
	exchangeName, _ := optString(sub.Options, "exchange")
	routingKey, _ := optString(sub.Options, "routing_key")
	exchangeType := "direct"
	if et, ok := optString(sub.Options, "exchange_type"); ok {
		exchangeType = et
	}
	durable, _ := optBool(sub.Options, "durable")
	autoDelete, _ := optBool(sub.Options, "auto_delete")

	ch, err := conn.Channel()
	if err != nil {
		return MapError(err)
	}
	defer func() { _ = ch.Close() }()

	if exchangeName != "" {
		if err := ch.ExchangeDeclare(exchangeName, exchangeType, durable, autoDelete); err != nil {
			return MapError(err)
		}
	}

	if err := ch.QueueDeclare(queueName, durable, autoDelete); err != nil {
		return MapError(err)
	}

	if exchangeName != "" {
		if routingKey == "" {
			routingKey = queueName
		}
		if err := ch.QueueBind(queueName, routingKey, exchangeName); err != nil {
			return MapError(err)
		}
	}

	s.mu.Lock()
	s.activeSubs[queueName] = true
	s.mu.Unlock()
	return nil
}

// declarePublisher opens a fresh channel for a single publisher's exchange
// declaration. See declareSubscription for the rationale.
func (s *Session) declarePublisher(conn amqpConnection, pub domain.PublisherPlan) error {
	exchangeName := pub.Topic
	if exchangeName == "" {
		return nil
	}
	exchangeType := "direct"
	if et, ok := optString(pub.Options, "exchange_type"); ok {
		exchangeType = et
	}
	durable, _ := optBool(pub.Options, "durable")
	autoDelete, _ := optBool(pub.Options, "auto_delete")

	ch, err := conn.Channel()
	if err != nil {
		return MapError(err)
	}
	defer func() { _ = ch.Close() }()

	if err := ch.ExchangeDeclare(exchangeName, exchangeType, durable, autoDelete); err != nil {
		return MapError(err)
	}
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
//
// The returned channel is shared. Each event is delivered to exactly one
// reader, so callers that need to react independently to lifecycle events
// (for example, multiple receivers waiting for SessionConnected after a
// reconnect) MUST use Subscribe instead.
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

// Subscribe returns a private buffered channel that receives every
// subsequent session lifecycle event, plus an unsubscribe function that
// removes the subscription and closes the channel. Use this when more
// than one consumer needs to observe session events; the legacy Events
// channel delivers each value to a single reader and would otherwise
// starve other consumers.
//
// The returned channel has buffer size 16. If a slow subscriber lets it
// fill up, additional events are dropped (non-blocking send) so a slow
// reader cannot stall pushEvent or other subscribers.
//
// Unsubscribe is safe to call at most once and after the session has
// been closed; it is also safe to invoke from defer.
func (s *Session) Subscribe() (<-chan ports.SessionEvent, func()) {
	ch := make(chan ports.SessionEvent, 16)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	s.eventSubs = append(s.eventSubs, ch)
	s.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			s.mu.Lock()
			removed := false
			for i, sub := range s.eventSubs {
				if sub == ch {
					s.eventSubs = append(s.eventSubs[:i], s.eventSubs[i+1:]...)
					removed = true
					break
				}
			}
			s.mu.Unlock()
			// Only close the channel if we removed it ourselves;
			// otherwise Close() already drained and closed it.
			if removed {
				close(ch)
			}
		})
	}
	return ch, unsub
}

// subscriberCount reports the number of active Subscribe channels.
// Intended for tests.
func (s *Session) subscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.eventSubs)
}

// Close gracefully closes the AMQP connection and stops the reconnection
// loop. It is safe to call Close multiple times.
func (s *Session) Close(_ context.Context) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelTrace,
			"amqp091: session close initiated",
			"broker", s.safeBrokerURL())
	}
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(context.Background(), logging.LevelDebug, "amqp091: session closing",
			"broker", s.safeBrokerURL())
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
	cancel := s.cancel
	s.cancel = nil
	done := s.bgDone
	s.bgDone = nil
	subs := s.eventSubs
	s.eventSubs = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	var closeErr error
	if conn != nil {
		closeErr = conn.Close()
	}

	close(s.events)
	for _, sub := range subs {
		close(sub)
	}

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

	ev := ports.SessionEvent{Type: t, Err: err, Timestamp: s.clock().Now()}
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
					"amqp091: event dropped, channel full",
					"event_type", t,
				)
			}
			s.metrics.Counter(domain.MetricAMQP091EventDropped, 1)
		}
	}

	for _, sub := range s.eventSubs {
		select {
		case sub <- ev:
		default:
			s.metrics.Counter(domain.MetricAMQP091EventDropped, 1)
		}
	}
}

const (
	reconnectInitial = 1 * time.Second
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
			case <-s.reconnected:
			}
			continue
		}

		notifyCh := conn.NotifyClose()

		select {
		case <-ctx.Done():
			return
		case connErr, ok := <-notifyCh:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			s.connected = false
			s.conn = nil
			s.activeSubs = make(map[string]bool)
			s.mu.Unlock()

			if !ok {
				connErr = nil
			}

			var evErr error
			if connErr != nil {
				evErr = MapError(connErr)
			}
			s.pushEvent(ports.SessionDisconnected, evErr)
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(context.Background(), logging.LevelDebug, "amqp091: connection lost, reconnecting",
					"broker", s.safeBrokerURL(), "error", connErr)
			}

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
		if logging.TraceEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelTrace, "amqp091: reconnect attempt starting",
				"broker", safeBroker,
				"attempt", attempt+1,
				"delay", delay,
			)
		}
		s.metrics.Counter(domain.MetricAMQP091Reconnects, 1,
			domain.Tag{Key: domain.TagKeySessionID, Value: safeBroker})

		jitter := time.Duration(float64(delay) * 0.25 * (2*rand.Float64() - 1))
		sleepDur := delay + jitter

		select {
		case <-ctx.Done():
			return
		case <-s.clock().After(sleepDur):
		}

		connectCtx, connectCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
		conn, err := s.dialWithTimeout(connectCtx)
		connectCancel()

		if err != nil {
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(context.Background(), logging.LevelDebug, "amqp091: reconnect attempt failed",
					"broker", safeBroker,
					"attempt", attempt+1,
					"error", err,
				)
			}
			delay = time.Duration(math.Min(
				float64(delay)*s.opts.ReconnectMultiplier,
				float64(s.opts.ReconnectMaxDelay),
			))
			continue
		}

		s.mu.Lock()
		s.conn = conn
		s.connected = true
		plan := s.plan
		s.mu.Unlock()

		if logging.DebugEnabled(s.logger) {
			s.logger.Log(context.Background(), logging.LevelDebug, "amqp091: reconnected",
				"broker", safeBroker, "attempt", attempt+1)
		}

		if plan != nil {
			reconCtx, reconCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
			err := s.reconcile(reconCtx, conn, *plan)
			reconCancel()
			if err != nil {
				if s.logger != nil {
					s.logger.Warn("amqp091: reconcile on reconnect failed",
						"error", err)
				}
			}
		}

		s.pushEvent(ports.SessionConnected, nil)

		select {
		case s.reconnected <- struct{}{}:
		default:
		}

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
				_ = r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("amqp091: dial timeout: %w", ctx.Err())
	case r := <-ch:
		return r.conn, r.err
	}
}
