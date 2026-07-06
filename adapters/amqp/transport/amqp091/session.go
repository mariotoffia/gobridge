package amqp091

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Session implements ports.Session for AMQP 0-9-1, owning a single
// broker connection with automatic reconnection and exchange/queue/binding
// declaration during Reconcile.
type Session struct {
	opts    SessionOptions
	mode    connectivity.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter
	dial    dialFunc
	// dialBuilder rebuilds the dial closure from options when credential or
	// TLS rotation changes the material (see ApplyCredentials). Defaults to
	// defaultDialFromOpts; kept as a field so tests can inject a hermetic
	// builder that reads TLS material without opening a real socket.
	dialBuilder func(SessionOptions) dialFunc
	clk         clock.Clock

	mu        sync.Mutex
	conn      amqpConnection
	events    chan ports.SessionEvent
	closed    bool
	connected bool
	starting  bool

	// blocked tracks whether the broker currently has TCP backpressure
	// engaged (connection.blocked) due to a resource alarm, with the
	// server-supplied reason. Surfaced in Health so broker memory/disk
	// pushback is not misread as ordinary send timeouts. Guarded by mu.
	blocked       bool
	blockedReason string

	plan       *connectivity.SessionPlan
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
// connectivity.CredentialSet for the future extension point.
type amqpCredentials struct {
	Username string
	Password string
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an AMQP 0-9-1 Session from the given options.
// metrics may be nil; a no-op exporter is used in that case.
func NewSession(opts SessionOptions, mode connectivity.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	opts.applyDefaults()
	s := &Session{
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
			Password: opts.Password.Reveal(),
		},
	}
	// Surface a config conflict that is otherwise silent: explicit
	// credentials override any userinfo embedded in broker_url on every
	// dial (see injectCredentials), so embedded userinfo alongside a
	// configured username is dead config — likely a stale secret sitting
	// in YAML that misleads the next reader.
	if opts.Username != "" && brokerURLEmbedsUserinfo(opts.BrokerURL) {
		s.warnEmbeddedBrokerURLCredentials("the configured")
	}
	return s
}

// warnEmbeddedBrokerURLCredentials warns that broker_url embeds userinfo
// which the explicit/rotated credentials override (F8): the embedded
// values are never sent to the broker, so they should be removed from
// the URL. Emitted once at construction (config conflict) and once per
// credential rotation (rotation over a stale embedded secret). Only the
// redacted URL is logged.
func (s *Session) warnEmbeddedBrokerURLCredentials(source string) {
	if s.logger == nil {
		return
	}
	s.logger.Warn("amqp091: broker_url embeds credentials (userinfo) that are overridden by "+
		source+" username/password on every dial; remove the userinfo from broker_url",
		"broker", redactURL(s.opts.BrokerURL))
}

// brokerURL returns the broker URL with credentials injected from the
// most recently applied credential material (see ApplyCredentials).
func (s *Session) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// buildDial rebuilds the dial closure from opts, honouring an injected
// dialBuilder when present (tests) and otherwise using the SDK-backed
// defaultDialFromOpts.
func (s *Session) buildDial(opts SessionOptions) dialFunc {
	if s.dialBuilder != nil {
		return s.dialBuilder(opts)
	}
	return defaultDialFromOpts(opts)
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
// When a SessionPlan is already installed (Reconcile was called before
// Start), Start reconciles it before reporting the session connected.
// A reconcile failure fails Start: the connection is torn down, the
// error is returned, and Start may be retried. This surfaces a broken
// topology (e.g. an unbindable queue that would silently drop routed
// messages) instead of degrading Health as the only evidence.
//
// Concurrent callers do not race the connection: the first caller takes
// the "starting" slot and performs the dial; later callers block on the
// same outcome (success, dial/reconcile error, or
// session-closed-during-start) so that "Start returned nil" reliably
// means "session is connected and reconciled".
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.
			WithMessage("amqp091: session already closed").
			Wrap(shared.ErrTransportClosedPermanently)
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
				return shared.ErrUnavailable.WithMessage("amqp091: session closed during start")
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Checked after "starting": the connection is installed mid-Start
	// (before reconcile completes), so a concurrent caller must join the
	// in-flight Start above rather than observe the half-started conn
	// and return success early.
	if s.conn != nil {
		s.mu.Unlock()
		return nil
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

	// Re-check closed under the lock AFTER the dial: Close may have run
	// while the dial was in flight. Installing the connection on a
	// closed session would leak a live TCP connection (and any consumers
	// opened on it) until process exit, because Close has already taken
	// and released its snapshot of s.conn.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		closedErr := shared.ErrUnavailable.WithMessage("amqp091: session closed during start")
		s.mu.Lock()
		s.startErr = closedErr
		s.mu.Unlock()
		return closedErr
	}
	// Install the connection so a concurrent Close tears it down, but
	// keep connected=false until reconcile has restored the topology:
	// Health must not report Connected (and no SessionConnected event
	// may fire) while queues/bindings are still missing, or a receiver
	// races the reconcile, consumes from an undeclared queue, and dies
	// on a permanent 404.
	s.conn = conn
	plan := s.plan
	s.mu.Unlock()

	safeBroker := s.safeBrokerURL()
	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(MetricAMQP091ConnectLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: safeBroker})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp091: session connected",
			"broker", safeBroker, "connect_latency", elapsed)
	}

	// Defer SessionConnected until after reconcile when a plan is present,
	// so consumers don't act on a connection that isn't fully set up.
	if plan != nil {
		if err := s.reconcile(ctx, conn, *plan); err != nil {
			// A failed initial reconcile means the declared topology is
			// NOT in place: messages published to the missing binding are
			// silently unroutable. Surface it — fail Start so the caller
			// (runtime or embedder) sees the misconfiguration instead of
			// a Degraded service level as the only evidence. Start can be
			// retried; the session is unwound to its pre-Start state.
			mappedErr := MapError(err)
			if s.logger != nil {
				s.logger.Error("amqp091: reconcile on start failed; failing Start",
					"broker", safeBroker, "error", err)
			}
			s.mu.Lock()
			ownsConn := s.conn == conn
			if ownsConn {
				s.conn = nil
			}
			s.startErr = mappedErr
			s.mu.Unlock()
			if ownsConn {
				// If Close ran meanwhile it already took (and closed) the
				// connection; only close what we still own.
				_ = conn.Close()
			}
			return mappedErr
		}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	if s.closed {
		// Close ran during reconcile; it already closed the installed
		// connection. Do not start background goroutines on a closed
		// session.
		s.mu.Unlock()
		bgCancel()
		closedErr := shared.ErrUnavailable.WithMessage("amqp091: session closed during start")
		s.mu.Lock()
		s.startErr = closedErr
		s.mu.Unlock()
		return closedErr
	}
	s.connected = true
	s.cancel = bgCancel
	s.bgDone = done
	s.mu.Unlock()

	s.pushEvent(ports.SessionConnected, nil)

	// Observe broker flow control for this connection. The watcher exits
	// when the connection's blocked stream closes or bgCtx is cancelled,
	// so its lifetime is bounded by the connection / session.
	go s.watchBlocked(bgCtx, conn)

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
func (s *Session) Reconcile(ctx context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	s.plan = &plan
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return shared.ErrUnavailable.WithMessage("session not started")
	}

	return s.reconcile(ctx, conn, plan)
}

func (s *Session) reconcile(ctx context.Context, conn amqpConnection, plan connectivity.SessionPlan) error {
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
			// Publisher-exchange auto-declare is BEST-EFFORT, unlike
			// subscription declare above (which is fatal — you cannot consume
			// from a queue that cannot be declared). The sender never declared
			// its exchange before F1-P3; publishing to an externally-managed or
			// least-privilege exchange worked without it. An active re-declare
			// of such an exchange legitimately fails (PRECONDITION_FAILED on a
			// topology mismatch, ACCESS_REFUSED without configure permission),
			// yet publishing to it still works. Aborting reconcile here would
			// take a previously-working publish route DOWN. So warn + meter and
			// continue: a genuinely-absent exchange the bridge cannot create
			// still fails visibly at publish time (404 -> retry/DLQ), exactly as
			// it did before this auto-declare existed (ADV-F1-P3).
			s.metrics.Counter(MetricAMQP091PublisherDeclareFailed, 1,
				shared.Tag{Key: shared.TagKeyEntity, Value: pub.Topic})
			if s.logger != nil {
				s.logger.Warn("amqp091: publisher exchange auto-declare failed; "+
					"continuing (publish still works if the exchange already exists)",
					"exchange", pub.Topic, "error", err)
			}
		}
	}

	elapsed := s.clock().Since(reconcileStart)
	s.metrics.Timer(MetricAMQP091ReconcileLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.safeBrokerURL()})
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
func (s *Session) declareSubscription(conn amqpConnection, sub connectivity.SubscriptionPlan) error {
	queueName := sub.Topic
	decl := subscriptionParams(sub)

	ch, err := conn.Channel()
	if err != nil {
		return MapError(err)
	}
	defer func() { _ = ch.Close() }()

	if decl.exchange != "" {
		if err := ch.ExchangeDeclare(decl.exchange, decl.exchangeType, decl.durable, decl.autoDelete, decl.exchangeArgs); err != nil {
			return MapError(err)
		}
	}

	if err := ch.QueueDeclare(queueName, decl.durable, decl.autoDelete, decl.queueArgs); err != nil {
		return MapError(err)
	}

	if decl.exchange != "" {
		routingKey := decl.routingKey
		if routingKey == "" {
			routingKey = queueName
		}
		if err := ch.QueueBind(queueName, routingKey, decl.exchange, decl.bindArgs); err != nil {
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
func (s *Session) declarePublisher(conn amqpConnection, pub connectivity.PublisherPlan) error {
	exchangeName := pub.Topic
	if exchangeName == "" {
		return nil
	}
	decl := publisherParams(pub)

	ch, err := conn.Channel()
	if err != nil {
		return MapError(err)
	}
	defer func() { _ = ch.Close() }()

	if err := ch.ExchangeDeclare(exchangeName, decl.exchangeType, decl.durable, decl.autoDelete, decl.exchangeArgs); err != nil {
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
	activeTopics := make([]string, 0, len(s.activeSubs))
	for topic := range s.activeSubs {
		activeTopics = append(activeTopics, topic)
	}
	blocked := s.blocked
	blockedReason := s.blockedReason
	s.mu.Unlock()

	// Deterministic order so callers (and tests) never observe map
	// iteration randomness.
	sort.Strings(activeTopics)

	wantedCount := 0
	if plan != nil {
		wantedCount = len(plan.Subscriptions)
	}

	var sl ports.ServiceLevel
	switch {
	case !connected:
		sl = ports.ServiceLevelNone
	case blocked:
		// Connected, but the broker has flow control engaged (resource
		// alarm): publishes will stall. Report degraded so operators see
		// the cause rather than mistaking it for send timeouts.
		sl = ports.ServiceLevelDegraded
	case wantedCount == 0:
		sl = ports.ServiceLevelFull
	case activeCount >= wantedCount:
		sl = ports.ServiceLevelFull
	case activeCount == 0:
		sl = ports.ServiceLevelNone
	default:
		sl = ports.ServiceLevelDegraded
	}

	var lastErr error
	if connected && blocked {
		lastErr = shared.ErrBrokerBusy.WithMessage(
			"amqp091: broker flow control engaged: " + blockedReason)
	}

	return ports.SessionHealth{
		Connected:           connected,
		LastError:           lastErr,
		SubscriptionsWanted: wantedCount,
		SubscriptionsActive: activeCount,
		ActiveTopics:        activeTopics,
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
			s.metrics.Counter(MetricAMQP091EventDropped, 1)
		}
	}

	for _, sub := range s.eventSubs {
		select {
		case sub <- ev:
		default:
			s.metrics.Counter(MetricAMQP091EventDropped, 1)
		}
	}
}

const (
	reconnectInitial = 1 * time.Second
)

// watchBlocked observes broker connection.blocked / connection.unblocked
// notifications for one connection and reflects them into Session state
// (consulted by Health), a counter metric, and a log line. RabbitMQ raises
// these when a memory or disk resource alarm engages TCP backpressure;
// without this, a blocked broker looks like a string of send timeouts.
//
// The watcher exits when the broker closes the blocked stream (connection
// teardown) or ctx is cancelled, so its lifetime is bounded by the
// connection it watches. It deliberately does not clear blocked state on
// exit: the reconnect/disconnect path clears it, so a connection that
// drops while blocked does not strand a stale alarm.
func (s *Session) watchBlocked(ctx context.Context, conn amqpConnection) {
	blockedCh := conn.NotifyBlocked()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-blockedCh:
			if !ok {
				return
			}
			if !s.setBlocked(conn, b.Active, b.Reason) {
				// Stale notification from a watcher whose connection has
				// been superseded by a reconnect: ignore it entirely so a
				// dropped connection cannot pin the healthy one to Degraded
				// or emit phantom flow-control metrics/logs.
				continue
			}
			s.metrics.Counter(MetricAMQP091Blocked, 1,
				shared.Tag{Key: shared.TagKeySessionID, Value: s.safeBrokerURL()})
			if s.logger == nil {
				continue
			}
			if b.Active {
				s.logger.Warn("amqp091: broker engaged connection flow control (resource alarm)",
					"broker", s.safeBrokerURL(), "reason", b.Reason)
			} else if logging.DebugEnabled(s.logger) {
				s.logger.Log(ctx, logging.LevelDebug,
					"amqp091: broker cleared connection flow control",
					"broker", s.safeBrokerURL())
			}
		}
	}
}

// setBlocked records a broker flow-control transition observed by conn's
// watcher. It reports whether the write was applied: a notification from a
// connection that is no longer current is ignored (returns false), so a
// watcher bound to a dropped connection cannot pin the freshly reconnected
// (healthy) connection to Degraded + ErrBrokerBusy with a buffered
// {Active:true} it delivers after the reconnect. The current connection
// identity acts as the generation token — once s.conn advances to a new
// connection, every stale watcher write is rejected.
func (s *Session) setBlocked(conn amqpConnection, active bool, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn != s.conn {
		return false
	}
	s.blocked = active
	s.blockedReason = reason
	return true
}

// blockedState reports whether the broker currently has flow control
// (connection.blocked) engaged, together with the server-supplied
// reason. The sender consults it to fail fast with ErrBrokerBusy instead
// of wedging on a publish the SDK cannot cancel: v1.10.0's
// PublishWithDeferredConfirmWithContext ignores ctx, so while the broker
// holds TCP backpressure a publish blocks indefinitely — and, done under
// the sender mutex, wedges every other publisher past its deadline. Health
// already surfaces the same state (see Health); this exposes it on the hot
// path without duplicating the mutex bookkeeping.
func (s *Session) blockedState() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked, s.blockedReason
}

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
			// A dropped connection cannot be flow-controlled; clear any
			// lingering blocked state so a reconnect starts clean and
			// Health does not report stale broker pushback.
			s.blocked = false
			s.blockedReason = ""
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
		s.metrics.Counter(MetricAMQP091Reconnects, 1,
			shared.Tag{Key: shared.TagKeySessionID, Value: safeBroker})

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

		// Re-check closed under the lock AFTER the dial: Close may have
		// run while the dial was in flight. Installing the connection on
		// a closed session would leak a live TCP connection (plus any
		// consumers later opened on it) until process exit.
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		// Install the connection so a concurrent Close tears it down,
		// but keep connected=false (and defer SessionConnected) until
		// reconcile has restored the topology. Otherwise a receiver's
		// health probe wins the race against reconcile, consumes from a
		// not-yet-redeclared queue, and dies on a permanent 404.
		s.conn = conn
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
				// The topology is NOT restored — do not report the
				// session connected on it. Drop this connection and
				// retry the whole reconnect (dial + reconcile) with
				// backoff; a transient window (e.g. broker still
				// settling after restart) heals on a later attempt,
				// and a persistent failure keeps Health at
				// ServiceLevelNone instead of luring consumers onto a
				// broken topology.
				if s.logger != nil {
					s.logger.Error("amqp091: reconcile on reconnect failed; retrying reconnect",
						"broker", safeBroker, "attempt", attempt+1, "error", err)
				}
				s.mu.Lock()
				ownsConn := s.conn == conn
				if ownsConn {
					s.conn = nil
				}
				closed := s.closed
				s.mu.Unlock()
				if ownsConn {
					_ = conn.Close()
				}
				if closed {
					return
				}
				delay = time.Duration(math.Min(
					float64(delay)*s.opts.ReconnectMultiplier,
					float64(s.opts.ReconnectMaxDelay),
				))
				continue
			}
		}

		s.mu.Lock()
		if s.closed {
			// Close ran during reconcile; it already closed the
			// installed connection.
			s.mu.Unlock()
			return
		}
		s.connected = true
		s.mu.Unlock()

		// Re-arm the flow-control watcher for the new connection; the
		// previous watcher exited when the old connection's blocked
		// stream closed.
		go s.watchBlocked(ctx, conn)

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

	// Snapshot the dial func under the lock before spawning: ApplyCredentials
	// reassigns s.dial (credential/TLS rotation rebuilds the dialer) under the
	// same lock. Reading the field from the goroutine below without this
	// snapshot races that write. brokerURL() takes the lock itself.
	s.mu.Lock()
	dial := s.dial
	s.mu.Unlock()

	ch := make(chan result, 1)
	go func() {
		conn, err := dial(s.brokerURL())
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
