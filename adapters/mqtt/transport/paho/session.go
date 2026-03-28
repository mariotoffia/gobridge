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

	// startCtx is the parent context from Start(), used to derive
	// reconnect contexts so that session cancellation also cancels
	// in-progress reconciliations.
	startCtx context.Context
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

	cfg := autopaho.ClientConfig{
		ServerUrls: serverURLs,
		KeepAlive:  s.opts.KeepAlive,

		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *pahov5.Connack) {
			s.pushEvent(ports.SessionConnected, nil)
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
					// Restore the old subscription state so that the next
					// reconnect delta calculation remains correct.
					s.mu.Lock()
					s.activeSubs = oldSubs
					s.mu.Unlock()
					if s.logger != nil {
						s.logger.Warn("reconcile on reconnect failed", "error", err)
					}
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
	s.startCtx = ctx
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
//
// Ordering invariant (do not reorder):
//  1. Set s.closed = true under mutex — prevents pushEvent from sending.
//  2. Call cm.Disconnect — may trigger OnConnectError, which calls
//     pushEvent, but the s.closed guard returns early (safe re-entrancy).
//  3. Close s.events channel — safe because step 1 guarantees no
//     concurrent sender can reach the channel send.
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
// registered handlers (one per Receiver on this Session). A WaitGroup
// tracks in-flight handler goroutines so Session.Close can await them.
type router struct {
	mu       sync.RWMutex
	wg       sync.WaitGroup
	handlers map[string]func(*pahov5.Publish)
}

func newRouter() *router {
	return &router{handlers: make(map[string]func(*pahov5.Publish))}
}

// Route implements paho.Router. It converts packets.Publish to paho.Publish
// and dispatches to all registered handlers concurrently. Each handler
// receives an independent copy of the Publish -- both the struct and the
// Payload slice are copied so that handlers can safely inspect (but should
// not mutate) the data without racing on shared backing arrays.
func (r *router) Route(pb *packets.Publish) {
	pub := pahov5.PublishFromPacketPublish(pb)
	r.mu.RLock()
	handlers := make([]func(*pahov5.Publish), 0, len(r.handlers))
	for _, h := range r.handlers {
		handlers = append(handlers, h)
	}
	r.mu.RUnlock()
	r.wg.Add(len(handlers))
	for _, h := range handlers {
		p := *pub
		if pub.Payload != nil {
			p.Payload = make([]byte, len(pub.Payload))
			copy(p.Payload, pub.Payload)
		}
		go func(handler func(*pahov5.Publish)) {
			defer r.wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					// Handler panicked; absorb to avoid crashing the process.
					// wg.Done is still called via the outer defer.
				}
			}()
			handler(&p)
		}(h)
	}
}

// Wait blocks until all in-flight handler goroutines have returned.
func (r *router) Wait() { r.wg.Wait() }

// Register adds a handler for the given ID. Handlers receive an
// independent copy of the Publish struct and Payload per invocation.
// The Properties pointer is still shared across goroutines; handlers
// MUST NOT modify Properties fields. Violations cause data races
// under concurrent dispatch.
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
