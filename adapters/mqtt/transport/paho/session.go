package paho

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// Session implements ports.Session for MQTT, owning the broker connection,
// ClientID identity, and subscription reconciliation.
type Session struct {
	opts   SessionOptions
	mode   domain.SessionMode
	logger *slog.Logger

	mu     sync.Mutex
	cm     *autopaho.ConnectionManager
	events chan ports.SessionEvent
	closed bool

	// router receives all incoming publishes; Receivers register handlers.
	router *router

	// plan is the last reconciled session plan, re-applied on reconnect.
	plan *domain.SessionPlan

	// activeSubs tracks topics for which SUBSCRIBE has been issued.
	activeSubs map[string]byte // topic -> qos
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an MQTT Session from the given options.
func NewSession(opts SessionOptions, mode domain.SessionMode, logger *slog.Logger) *Session {
	return &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		events:     make(chan ports.SessionEvent, 16),
		router:     newRouter(),
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
// once the initial connection is established.
func (s *Session) Start(ctx context.Context) error {
	serverURLs, err := parseURLs(s.opts.BrokerURLs)
	if err != nil {
		return domain.ErrInvalidPayload.Wrap(err).WithMessage("parse broker URLs")
	}

	// Use a background-derived context for the connection manager so that
	// reconnect callbacks survive after the caller's Start context ends.
	connCtx := context.Background()

	cfg := autopaho.ClientConfig{
		ServerUrls: serverURLs,
		KeepAlive:  s.opts.KeepAlive,

		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *pahov5.Connack) {
			s.pushEvent(ports.SessionConnected, nil)
			s.mu.Lock()
			// Clear activeSubs so reconcile always re-subscribes after
			// a reconnect, in case the broker lost session state.
			s.activeSubs = make(map[string]byte)
			plan := s.plan
			s.mu.Unlock()
			if plan != nil {
				if err := s.reconcile(connCtx, cm, *plan); err != nil && s.logger != nil {
					s.logger.Warn("reconcile on reconnect failed", "error", err)
				}
			}
		},
		OnConnectError: func(err error) {
			s.pushEvent(ports.SessionReconnecting, MapError(err))
		},

		ClientConfig: pahov5.ClientConfig{
			ClientID: s.opts.ClientID,
			Router:   s.router,
		},
	}

	switch s.mode {
	case domain.SessionEphemeral:
		cfg.CleanStartOnInitialConnection = true
		cfg.SessionExpiryInterval = 0
	case domain.SessionPersistent, domain.SessionExclusive:
		cfg.CleanStartOnInitialConnection = s.opts.CleanStart
		cfg.SessionExpiryInterval = s.opts.SessionExpiryInterval
	}

	if s.opts.Username != "" {
		cfg.ConnectUsername = s.opts.Username
		cfg.ConnectPassword = []byte(s.opts.Password)
	}

	if s.opts.TLS != nil && s.opts.TLS.Enable {
		tlsCfg, err := BuildTLSConfig(s.opts.TLS)
		if err != nil {
			return domain.ErrUnavailable.Wrap(err).WithMessage("build TLS config")
		}
		cfg.TlsCfg = tlsCfg
	}

	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return MapError(err)
	}

	connectTimeout := s.opts.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second
	}
	awaitCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := cm.AwaitConnection(awaitCtx); err != nil {
		// Connection failed; disconnect to clean up the autopaho goroutine.
		_ = cm.Disconnect(context.Background())
		return MapError(err)
	}

	s.mu.Lock()
	s.cm = cm
	s.mu.Unlock()

	return nil
}

// Reconcile diffs the desired SessionPlan against current subscriptions and
// issues Subscribe / Unsubscribe to reach the desired state.
func (s *Session) Reconcile(ctx context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	s.plan = &plan
	cm := s.cm
	s.mu.Unlock()

	if cm == nil {
		return domain.ErrUnavailable.WithMessage("session not started")
	}

	return s.reconcile(ctx, cm, plan)
}

func (s *Session) reconcile(ctx context.Context, cm *autopaho.ConnectionManager, plan domain.SessionPlan) error {
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

	// Unsubscribe topics no longer desired
	var toUnsub []string
	for topic := range current {
		if _, ok := desired[topic]; !ok {
			toUnsub = append(toUnsub, topic)
		}
	}

	if len(toUnsub) > 0 {
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
		sa, err := cm.Subscribe(ctx, &pahov5.Subscribe{Subscriptions: toSub})
		if err != nil {
			return MapError(err)
		}
		s.mu.Lock()
		for i, opt := range toSub {
			if i < len(sa.Reasons) {
				if berr := MapSubscribeReasonCode(sa.Reasons[i]); berr != nil {
					s.mu.Unlock()
					return berr.With("topic", opt.Topic)
				}
			}
			s.activeSubs[opt.Topic] = opt.QoS
		}
		s.mu.Unlock()
	}

	return nil
}

// Health returns the current health state of the session.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	cm := s.cm
	s.mu.Unlock()

	if cm == nil {
		return ports.SessionHealth{Connected: false}
	}

	return ports.SessionHealth{Connected: true}
}

// Events returns the channel on which session lifecycle events are emitted.
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

// Close gracefully disconnects the MQTT session. It is safe to call
// Close multiple times.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cm := s.cm
	s.cm = nil
	s.mu.Unlock()

	var disconnErr error
	if cm != nil {
		disconnErr = cm.Disconnect(ctx)
	}

	close(s.events)

	if disconnErr != nil {
		return MapError(disconnErr)
	}
	return nil
}

func (s *Session) pushEvent(t ports.SessionEventType, err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	ev := ports.SessionEvent{Type: t, Err: err, Timestamp: time.Now()}
	select {
	case s.events <- ev:
	default:
		// Drop oldest if buffer is full, then push.
		select {
		case <-s.events:
		default:
		}
		select {
		case s.events <- ev:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// router: multiplexing paho.Router for shared Session
// ---------------------------------------------------------------------------

// router implements paho.Router, dispatching incoming publishes to all
// registered handlers (one per Receiver on this Session).
type router struct {
	mu       sync.RWMutex
	handlers map[string]func(*pahov5.Publish)
}

func newRouter() *router {
	return &router{handlers: make(map[string]func(*pahov5.Publish))}
}

// Route implements paho.Router. It converts packets.Publish to paho.Publish
// and calls all registered handlers.
func (r *router) Route(pb *packets.Publish) {
	pub := pahov5.PublishFromPacketPublish(pb)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.handlers {
		h(pub)
	}
}

func (r *router) Register(id string, h func(*pahov5.Publish)) {
	r.mu.Lock()
	r.handlers[id] = h
	r.mu.Unlock()
}

func (r *router) Unregister(id string) {
	r.mu.Lock()
	delete(r.handlers, id)
	r.mu.Unlock()
}

// paho.Router interface stubs — registration is done via Register/Unregister.
func (r *router) RegisterHandler(_ string, _ pahov5.MessageHandler) {}
func (r *router) UnregisterHandler(_ string)                        {}
func (r *router) SetDebugLogger(_ log.Logger)                       {}

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
