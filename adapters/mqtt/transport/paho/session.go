package paho

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Session implements ports.Session for MQTT, owning the broker connection,
// ClientID identity, and subscription reconciliation.
type Session struct {
	opts    SessionOptions
	mode    domain.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter

	mu        sync.Mutex
	cm        *autopaho.ConnectionManager
	cmCancel  context.CancelFunc // cancels the CM's background context on Close
	events    chan ports.SessionEvent
	closed    bool
	connected bool // true when autopaho reports connection is up
	starting  bool // guards against concurrent Start() calls (BUG-2 fix)

	// reconcileMu serializes concurrent reconcile calls (e.g. external
	// Reconcile vs OnConnectionUp callback) to prevent interleaved
	// subscribe/unsubscribe operations from corrupting activeSubs.
	reconcileMu sync.Mutex

	// router receives all incoming publishes; Receivers register handlers.
	router *router

	// plan is the last reconciled session plan, re-applied on reconnect.
	plan *domain.SessionPlan

	// activeSubs tracks topics for which SUBSCRIBE has been issued.
	activeSubs map[string]byte // topic -> qos

	// startCtx is a long-lived context derived from context.Background(),
	// used to derive reconnect reconciliation contexts. It is cancelled
	// by Close() via cmCancel, ensuring in-progress reconciliations are
	// aborted on shutdown.
	startCtx context.Context
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an MQTT Session from the given options.
// metrics may be nil; a no-op exporter is used in that case.
func NewSession(opts SessionOptions, mode domain.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	return &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		metrics:    m,
		events:     make(chan ports.SessionEvent, 16),
		router:     newRouter(logger, m),
		activeSubs: make(map[string]byte),
	}
}

// ConnectionManager returns the underlying autopaho.ConnectionManager.
// Receiver and Sender use this to issue subscribe/publish calls.
func (s *Session) ConnectionManager() *autopaho.ConnectionManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cm
}

// Router returns the message router for registering Receiver handlers.
func (s *Session) Router() *router {
	return s.router
}

// Start connects to the MQTT broker and emits a SessionConnected event
// once the initial connection is established. Calling Start on an
// already-started session is a no-op (idempotent).
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.cm != nil {
		s.mu.Unlock()
		return nil
	}
	if s.starting {
		s.mu.Unlock()
		return nil // another goroutine is already starting
	}
	s.starting = true
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connecting",
			"client_id", s.opts.ClientID,
			"broker_count", len(s.opts.BrokerURLs),
			"session_mode", s.mode,
		)
	}
	connectStart := time.Now()

	serverURLs, err := parseURLs(s.opts.BrokerURLs)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("parse broker URLs")
	}

	cfg := autopaho.ClientConfig{
		ServerUrls: serverURLs,
		KeepAlive:  s.opts.KeepAlive,

		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *pahov5.Connack) {
			s.mu.Lock()
			s.connected = true
			s.mu.Unlock()
			s.pushEvent(ports.SessionConnected, nil)
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(context.Background(), logging.LevelDebug, "mqtt: connection up",
					"client_id", s.opts.ClientID)
			}
			s.mu.Lock()
			oldSubs := s.activeSubs
			s.activeSubs = make(map[string]byte)
			plan := s.plan
			parentCtx := s.startCtx
			s.mu.Unlock()
		if plan != nil {
			reconTimeout := s.opts.ReconnectTimeout
			if reconTimeout == 0 {
				reconTimeout = 30 * time.Second
			}
			reconCtx, reconCancel := context.WithTimeout(parentCtx, reconTimeout)
			defer reconCancel()
			if err := s.reconcile(reconCtx, cm, *plan); err != nil {
				s.mu.Lock()
				s.activeSubs = oldSubs
				s.mu.Unlock()
				if s.logger != nil {
					s.logger.Warn("reconcile on reconnect failed", "error", err)
				}
			} else {
				s.pushEvent(ports.SessionReconciled, nil)
			}
		} else {
			s.pushEvent(ports.SessionReconciled, nil)
		}
		},
		OnConnectError: func(err error) {
			s.mu.Lock()
			s.connected = false
			s.mu.Unlock()
			s.pushEvent(ports.SessionReconnecting, MapError(err))
		},
		OnConnectionDown: func() bool {
			s.mu.Lock()
			s.connected = false
			s.mu.Unlock()
			s.pushEvent(ports.SessionDisconnected, nil)
			if logging.DebugEnabled(s.logger) {
				s.logger.Log(context.Background(), logging.LevelDebug, "mqtt: connection down",
					"client_id", s.opts.ClientID)
			}
			return true // always attempt reconnect
		},

		ClientConfig: pahov5.ClientConfig{
			ClientID: s.opts.ClientID,
			Router:   s.router,
		},
	}

	if s.opts.ReconnectDelay > 0 {
		cfg.ReconnectBackoff = autopaho.NewConstantBackoff(s.opts.ReconnectDelay)
	}

	// Set ReceiveMaximum via ConnectPacketBuilder to avoid exceeding the
	// broker's per-client inflight quota under high throughput.
	if s.opts.ReceiveMaximum > 0 {
		rm := s.opts.ReceiveMaximum
		cfg.ConnectPacketBuilder = func(cp *pahov5.Connect, _ *url.URL) (*pahov5.Connect, error) {
			if cp.Properties == nil {
				cp.Properties = &pahov5.ConnectProperties{}
			}
			cp.Properties.ReceiveMaximum = &rm
			return cp, nil
		}
	}

	switch s.mode {
	case domain.SessionEphemeral:
		cfg.CleanStartOnInitialConnection = true
		cfg.SessionExpiryInterval = 0
	case domain.SessionPersistent, domain.SessionExclusive:
		if s.opts.CleanStart && s.mode == domain.SessionExclusive {
			// CleanStart + Exclusive is a misconfiguration: autopaho reconnects
			// with the same Client ID and CleanStart=true, causing the broker to
			// disconnect the existing connection ("session taken over" loop).
			// Override to false and log a warning.
			if s.logger != nil {
				s.logger.Warn("mqtt: CleanStart=true with SessionExclusive is invalid; "+
					"overriding to CleanStart=false to prevent session takeover loop",
					"client_id", s.opts.ClientID)
			}
			cfg.CleanStartOnInitialConnection = false
		} else {
			cfg.CleanStartOnInitialConnection = s.opts.CleanStart
		}
		cfg.SessionExpiryInterval = s.opts.SessionExpiryInterval
	}

	if s.opts.Username != "" {
		cfg.ConnectUsername = s.opts.Username
		cfg.ConnectPassword = []byte(s.opts.Password)
	}

	if s.opts.TLS != nil && s.opts.TLS.Enable {
		tlsCfg, err := BuildTLSConfig(s.opts.TLS)
		if err != nil {
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
			return domain.ErrUnavailable.Wrap(err).WithMessage("build TLS config")
		}
		cfg.TlsCfg = tlsCfg
	}

	// The CM's reconnection loop must outlive the caller's context.
	// We derive a background context that is only cancelled by Close().
	cmCtx, cmCancel := context.WithCancel(context.Background())

	cm, err := autopaho.NewConnection(cmCtx, cfg)
	if err != nil {
		cmCancel()
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return MapError(err)
	}

	connectTimeout := s.opts.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second
	}
	awaitCtx, awaitCancel := context.WithTimeout(ctx, connectTimeout)
	defer awaitCancel()

	if err := cm.AwaitConnection(awaitCtx); err != nil {
		cmCancel()
		_ = cm.Disconnect(context.Background())
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "mqtt: connect failed",
				"client_id", s.opts.ClientID, "error", err)
		}
		return MapError(err)
	}

	s.mu.Lock()
	s.cm = cm
	s.cmCancel = cmCancel
	s.startCtx = cmCtx
	s.connected = true
	s.starting = false
	s.mu.Unlock()

	elapsed := time.Since(connectStart)
	s.metrics.Timer(domain.MetricMQTTConnectLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connected",
			"client_id", s.opts.ClientID, "connect_latency", elapsed)
	}

	return nil
}

// Reconcile diffs the desired SessionPlan against current subscriptions and
// issues Subscribe / Unsubscribe to reach the desired state.
//
// When the new plan has no subscriptions and a plan is already set (from a
// prior Reconcile call), the call is a no-op. This prevents a SessionManager
// with an empty plan from unsubscribing externally-managed topics.
func (s *Session) Reconcile(ctx context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	hasPriorPlan := s.plan != nil
	if len(plan.Subscriptions) > 0 || !hasPriorPlan {
		s.plan = &plan
	}
	cm := s.cm
	s.mu.Unlock()

	if cm == nil {
		return domain.ErrUnavailable.WithMessage("session not started")
	}

	if len(plan.Subscriptions) == 0 && hasPriorPlan {
		return nil
	}

	return s.reconcile(ctx, cm, plan)
}

func (s *Session) reconcile(ctx context.Context, cm *autopaho.ConnectionManager, plan domain.SessionPlan) error {
	reconcileStart := time.Now()
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	desired := make(map[string]byte, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		desired[sub.Topic] = byte(sub.QoS)
	}

	s.mu.Lock()
	current := make(map[string]byte, len(s.activeSubs))
	for k, v := range s.activeSubs {
		current[k] = v
	}
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: reconcile",
			"client_id", s.opts.ClientID,
			"desired", len(desired),
			"active", len(current),
		)
	}

	// Unsubscribe topics no longer desired
	var toUnsub []string
	for topic := range current {
		if _, ok := desired[topic]; !ok {
			toUnsub = append(toUnsub, topic)
		}
	}

	if len(toUnsub) > 0 {
		if logging.TraceEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelTrace, "mqtt: unsubscribing",
				"client_id", s.opts.ClientID, "topics", toUnsub)
		}
		if _, err := cm.Unsubscribe(ctx, &pahov5.Unsubscribe{Topics: toUnsub}); err != nil {
			return MapError(err)
		}
		s.mu.Lock()
		for _, t := range toUnsub {
			delete(s.activeSubs, t)
		}
		s.mu.Unlock()
	}

	// Subscribe to new or changed topics
	var toSub []pahov5.SubscribeOptions
	for topic, qos := range desired {
		curQoS, exists := current[topic]
		if !exists || curQoS != qos {
			toSub = append(toSub, pahov5.SubscribeOptions{Topic: topic, QoS: qos})
		}
	}

	if len(toSub) > 0 {
		if logging.TraceEnabled(s.logger) {
			topics := make([]string, len(toSub))
			for i, sub := range toSub {
				topics[i] = sub.Topic
			}
			s.logger.Log(ctx, logging.LevelTrace, "mqtt: subscribing",
				"client_id", s.opts.ClientID, "topics", topics)
		}
		sa, err := cm.Subscribe(ctx, &pahov5.Subscribe{Subscriptions: toSub})
		if err != nil {
			return MapError(err)
		}
		var succeeded []pahov5.SubscribeOptions
		for i, opt := range toSub {
			if i < len(sa.Reasons) {
				if berr := MapSubscribeReasonCode(sa.Reasons[i]); berr != nil {
					s.mu.Lock()
					for _, sub := range succeeded {
						s.activeSubs[sub.Topic] = sub.QoS
					}
					s.mu.Unlock()
					return berr.With("topic", opt.Topic)
				}
			}
			succeeded = append(succeeded, opt)
		}
		s.mu.Lock()
		for _, opt := range succeeded {
			s.activeSubs[opt.Topic] = opt.QoS
		}
		s.mu.Unlock()
	}

	elapsed := time.Since(reconcileStart)
	s.metrics.Timer(domain.MetricMQTTReconcileLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: reconcile done",
			"client_id", s.opts.ClientID,
			"unsubscribed", len(toUnsub),
			"subscribed", len(toSub),
			"duration", elapsed,
		)
	}

	return nil
}

// Health returns the current health state of the session, including
// subscription and handler readiness.
//
// Ready is true when the session is connected to the broker (connectivity only).
//
// ServiceLevel describes operational completeness:
//   - Full: all desired subscriptions active and handlers registered (when expected)
//   - Degraded: connected but not all desired subscriptions are active
//   - None: not connected, or no subscriptions/handlers registered
//
// For sender-only sessions (no subscriptions), ServiceLevel is Full when connected.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	cm := s.cm
	plan := s.plan
	activeCount := len(s.activeSubs)
	connected := cm != nil && s.connected
	s.mu.Unlock()
	wantedCount := 0
	if plan != nil {
		wantedCount = len(plan.Subscriptions)
	}
	handlerCount := s.router.HandlerCount()

	var sl ports.ServiceLevel
	switch {
	case !connected:
		sl = ports.ServiceLevelNone
	case wantedCount == 0:
		// Sender-only session: no subscriptions expected.
		sl = ports.ServiceLevelFull
	case activeCount == wantedCount && handlerCount > 0:
		sl = ports.ServiceLevelFull
	case activeCount == 0 && handlerCount == 0:
		sl = ports.ServiceLevelNone
	default:
		sl = ports.ServiceLevelDegraded
	}

	rm := s.opts.ReceiveMaximum
	if rm == 0 {
		rm = 65535 // MQTT v5 default when not explicitly configured
	}

	return ports.SessionHealth{
		Connected:           connected,
		SubscriptionsWanted: wantedCount,
		SubscriptionsActive: activeCount,
		HandlersRegistered:  handlerCount,
		ReceiveMaximum:      rm,
		Ready:               connected,
		ServiceLevel:        sl,
	}
}

// Events returns the channel on which session lifecycle events are emitted.
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

// Close gracefully disconnects the MQTT session. It is safe to call
// Close multiple times.
//
// Ordering invariant (do not reorder):
//  1. Set s.closed = true under mutex — prevents pushEvent from sending.
//  2. Call cm.Disconnect — may trigger OnConnectError, which calls
//     pushEvent, but the s.closed guard returns early (safe re-entrancy).
//  3. Close s.events channel — safe because step 1 guarantees no
//     concurrent sender can reach the channel send.
func (s *Session) Close(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session closing",
			"client_id", s.opts.ClientID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.connected = false
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.mu.Unlock()

	var disconnErr error
	if cm != nil {
		disconnErr = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	done := make(chan struct{})
	go func() { s.router.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if s.logger != nil {
			s.logger.Warn("Close: context expired while waiting for in-flight handlers")
		}
	}
	close(s.events)

	if disconnErr != nil {
		return MapError(disconnErr)
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
		// Drop oldest event to make room.
		select {
		case <-s.events:
		default:
		}
		select {
		case s.events <- ev:
		default:
			// Event channel still full after draining oldest; event lost.
			s.metrics.Counter(domain.MetricMQTTEventDropped, 1)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseURLs(raw []string) ([]*url.URL, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one broker URL is required")
	}
	out := make([]*url.URL, len(raw))
	for i, s := range raw {
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid broker URL %q: %w", s, err)
		}
		out[i] = u
	}
	return out, nil
}
