package amqp10

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	reconnectInitial = 1 * time.Second
	eventChannelSize = 16
)

// Session implements ports.Session for AMQP 1.0, owning the broker
// connection and AMQP session. It provides automatic reconnection
// with exponential backoff and health monitoring.
type Session struct {
	opts    SessionOptions
	mode    connectivity.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter
	dial    dialFunc
	clk     clock.Clock

	mu        sync.Mutex
	conn      amqpConn
	amqpSess  *amqpSessionLink
	events    chan ports.SessionEvent
	closed    bool
	connected bool
	starting  bool
	plan      *connectivity.SessionPlan

	// receivers tracks live receiver links for health reporting. A
	// receiver registers itself when its Run loop starts; the bool
	// records whether its AMQP link is currently up. Health
	// degrades the service level when a registered receiver's link is
	// down while the session connection itself is still alive.
	receivers map[*Receiver]bool

	// senders tracks live sender links for health reporting, mirroring
	// receivers. A Sender registers when its link is first established
	// and flips the entry down on a link failure. A down
	// sender link degrades ServiceLevel even while the connection and all
	// receivers are healthy, so a broker refusing publishes is visible to
	// readiness probes.
	senders map[*Sender]bool

	// builtLinkCount and builtDurableReceiver enforce the
	// dedicated-session contract for durable AMQP 1.0 receivers at BUILD
	// time. One Session multiplexes every receiver/sender bound
	// to its session_id over a single connection, and closing a durable
	// receiver forces a full connection teardown (the pinned go-amqp
	// cannot non-closing-detach a durable link — a closing detach is an
	// UNSUBSCRIBE), which transiently blips every sibling link. reserveLink
	// refuses to build a durable receiver on a session that already hosts
	// any link, and refuses any link on a session already claimed by a
	// durable receiver, so the blast radius is confined by construction
	// (Dedicated-session contract in UBIQUITOUS.md / doc.go).
	// Guarded by mu.
	builtLinkCount       int
	builtDurableReceiver bool

	// lastErr records the most recent link/connection error observed by
	// a receiver or sender, surfaced via SessionHealth.LastError on any
	// non-Full state so operators see the CAUSE of a degrade, not merely
	// the level. Cleared on a successful (re)connect.
	lastErr error

	// stopMonitor cancels the background health-monitoring goroutine.
	stopMonitor context.CancelFunc
	// monitorDone is closed when the monitor goroutine exits.
	monitorDone chan struct{}
	// reconnectCh is signalled by notifyDisconnect to trigger immediate reconnect.
	reconnectCh chan struct{}

	// startDone / startErr coordinate concurrent Start calls so that
	// only the first caller dials and later callers block on the same
	// outcome instead of returning success while the dial is still
	// running.
	startDone chan struct{}
	startErr  error

	// eventSubs holds per-subscriber channels for fan-out delivery of
	// session lifecycle events.
	eventSubs []chan ports.SessionEvent

	// liveCreds captures the latest applied credentials, consulted on
	// every (re)connect via connect(). ApplyCredentials writes here
	// and drops the current connection so the next dial picks up the
	// rotated material.
	liveCreds amqp10Credentials

	// authFailureCB is the reactive-recovery hook. The
	// CredentialRefresher injects a URI-bound callback via
	// SetAuthFailureCallback; reportAuthFailure invokes it when a live
	// (re)connect maps a dial error to shared.ErrNotAuthorized, forcing an
	// immediate credential re-resolve rather than stalling on revoked
	// credentials until the next poll. atomic.Pointer gives safe publication:
	// the setter runs on the builder goroutine while the load happens on the
	// reconnect/monitor goroutine.
	authFailureCB atomic.Pointer[func(error)]
}

// amqp10Credentials is the mutable subset of SessionOptions that can
// be rotated at runtime. SASL username/password live here; TLS material
// is rotated separately via ApplyCredentials, which swaps s.opts.TLS
// (see applyAMQP10TLSMaterial).
type amqp10Credentials struct {
	Username string
	Password string
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an AMQP 1.0 Session from the given options.
//
// LOW-LEVEL constructor. Besides the credential note below, it does NOT
// gate durable receivers on an explicit container_id: applyDefaults
// synthesises a per-instance container_id (unique per replica, stable
// across reconnects, but DIFFERENT across process restarts). The
// restart-safety gate for durable subscriptions lives in
// Factory.NewReceiver, which production always goes through (every link is
// built via the ports.TransportFactory interface). A direct embedder that
// builds a durable receiver on a NewSession-built session with a generated
// container_id will silently lose the subscription across restarts — build
// through Factory in production.
//
// SECURITY NOTE: NewSession applies defaults but does NOT run
// SessionOptions.validate — in particular it does NOT enforce the
// plaintext-PLAIN credential gate (see validatePlainOverPlaintext). That
// gate is enforced at the configuration/factory boundary: build sessions
// through Factory.NewSession or SessionOptionsFromMap, both of which call
// validate. A direct programmatic embedder using NewSession with an
// Address that puts SASL PLAIN over a non-TLS scheme (o.Username set, or
// URL userinfo such as amqp://user:pass@host) is responsible for calling
// opts.validate(usingTLS) itself, or must accept that credentials may be
// sent in cleartext.
func NewSession(opts SessionOptions, mode connectivity.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
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
		clk:         opts.Clock,
		events:      make(chan ports.SessionEvent, eventChannelSize),
		reconnectCh: make(chan struct{}, 1),
		receivers:   make(map[*Receiver]bool),
		senders:     make(map[*Sender]bool),
		liveCreds: amqp10Credentials{
			Username: opts.Username,
			Password: opts.Password.Reveal(),
		},
	}
}

func (s *Session) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

// Conn returns the underlying AMQP connection. Receivers and senders
// use this to identify which connection their links belong to.
func (s *Session) Conn() amqpConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// AMQPSession returns the underlying session link wrapper for link
// creation. Tests may inspect this; production code uses it only to
// open new receiver/sender links.
func (s *Session) AMQPSession() *amqpSessionLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.amqpSess
}

// sessionAndConn returns the session link wrapper together with the
// connection that owns it, captured under a single lock hold. Callers
// creating links MUST use this instead of separate AMQPSession()+Conn()
// calls: a reconnect between the two reads would pair a link with the
// WRONG connection identity, and a later notifyDisconnect carrying the
// stale identity would be silently dropped.
func (s *Session) sessionAndConn() (*amqpSessionLink, amqpConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.amqpSess, s.conn
}

// Start connects to the AMQP 1.0 broker, creates an AMQP session, and
// starts a background goroutine to monitor connection health.
//
// Concurrent callers do not race the connection: the first caller takes
// the "starting" slot and performs the dial; later callers block on the
// same outcome (success, dial error, or session-closed-during-start) so
// that "Start returned nil" reliably means "session is connected".
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.
			WithMessage("amqp10: session already closed").
			Wrap(shared.ErrTransportClosedPermanently)
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
				return shared.ErrUnavailable.WithMessage("amqp10: session closed during start")
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
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: session connecting",
			"address", redactURL(s.opts.Address),
			"session_mode", s.mode,
		)
	}

	connectStart := s.clock().Now()

	if err := s.connect(ctx); err != nil {
		s.mu.Lock()
		s.startErr = err
		s.mu.Unlock()
		return err
	}

	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(MetricAMQP10ConnectLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ContainerID})

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: session connected",
			"address", redactURL(s.opts.Address),
			"connect_latency", elapsed,
		)
	}

	monCtx, monCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	if s.closed {
		// Close() won the race between connect() storing s.conn and
		// this monitor install. Abort the install so we neither leak an
		// immortal monitor goroutine nor return nil for a closed session.
		closedErr := shared.ErrUnavailable.
			WithMessage("amqp10: session closed during start").
			Wrap(shared.ErrTransportClosedPermanently)
		s.startErr = closedErr
		s.mu.Unlock()
		monCancel()
		return closedErr
	}
	s.stopMonitor = monCancel
	s.monitorDone = done
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.monitorLoop(monCtx)
	}()

	return nil
}

func (s *Session) connect(ctx context.Context) error {
	// reactive-recovery chokepoint: every (re)connect — the initial
	// dial and every reconnect after a credential rotation dropped the
	// connection — routes through doConnect. When a hard rotation revoked the
	// old credentials, the redial fails SASL and doConnect maps it to
	// shared.ErrNotAuthorized; reporting here forces an immediate re-resolve
	// instead of stalling on revoked material until the next poll.
	//
	// ponytail: session transports authenticate at connection open, so a
	// credential revocation manifests as a (re)connect auth failure, NOT a
	// per-message send/receive fault (a live send returning NOT_AUTHORIZED is a
	// routing-authorization condition, not a rotation failure). This single
	// chokepoint therefore covers the credential-rotation scenario for all
	// links multiplexed on the session.
	err := s.doConnect(ctx)
	s.reportAuthFailure(err)
	return err
}

func (s *Session) doConnect(ctx context.Context) error {
	// Snapshot the mutable dial inputs under the lock: ApplyCredentials
	// (runtime credential refresher goroutine) writes s.opts.Username/
	// Password and swaps the s.opts.TLS pointer concurrently. Copying the
	// SessionOptions struct here — and never mutating a *TLSConfig in
	// place (see applyAMQP10TLSMaterial) — means the dial reads a stable,
	// immutable options value with no torn cert/key pair.
	s.mu.Lock()
	creds := s.liveCreds
	opts := s.opts
	s.mu.Unlock()

	connectCtx, connectCancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer connectCancel()

	conn, err := s.dial(connectCtx, opts, creds)
	if err != nil {
		return MapError(err)
	}

	sess, err := conn.NewSession(connectCtx)
	if err != nil {
		_ = conn.Close()
		return MapError(err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = sess.Close(connectCtx)
		_ = conn.Close()
		return shared.ErrUnavailable.WithMessage("amqp10: session closed during connect")
	}
	oldConn := s.conn
	oldSess := s.amqpSess
	s.conn = conn
	s.amqpSess = sess
	s.connected = true
	s.lastErr = nil // a fresh connection clears the last recorded fault
	// Senders have no background reattach loop; drop their stale down
	// state so the benign post-reconnect lazy-reattach window does not
	// report the session Degraded.
	s.resetSendersForReconnectLocked()
	s.mu.Unlock()

	if oldSess != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), s.opts.LinkCloseTimeout)
		_ = oldSess.Close(cleanCtx)
		cleanCancel()
	}
	if oldConn != nil {
		_ = oldConn.Close()
	}

	s.pushEvent(ports.SessionConnected, nil)
	return nil
}

// Reconcile stores the desired SessionPlan. AMQP 1.0 has no
// queue/exchange declare operations — links are created lazily when
// receivers and senders start.
func (s *Session) Reconcile(ctx context.Context, plan connectivity.SessionPlan) error {
	reconcileStart := s.clock().Now()

	s.mu.Lock()
	s.plan = &plan
	connected := s.connected
	s.mu.Unlock()

	if !connected {
		return shared.ErrUnavailable.WithMessage("amqp10: session not connected")
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp10: reconcile",
			"subscriptions", len(plan.Subscriptions),
			"publishers", len(plan.Publishers),
		)
	}

	s.pushEvent(ports.SessionReconciled, nil)

	elapsed := s.clock().Since(reconcileStart)
	s.metrics.Timer(MetricAMQP10ReconcileLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ContainerID})

	return nil
}

// Health returns the current health state of the session.
//
// The service level reflects receiver LINK state, not just
// connectivity. When the connection is alive but one or more registered
// receivers have a detached link (e.g. mid-reconnect), the session
// reports Degraded with a reduced active count instead of falsely
// claiming Full. The desired count is the larger of the reconciled
// subscription plan and the number of registered receivers, so existing
// behaviour (no receivers registered) is preserved exactly.
//
// the reported active count is additionally clamped to the
// link-derived up count (registered receivers whose link is live), so a
// plan wanting more subscriptions than there are self-establishing
// receivers cannot over-report active during startup.
func (s *Session) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	connected := s.connected
	plan := s.plan
	lastErr := s.lastErr
	wanted := 0
	if plan != nil {
		wanted = len(plan.Subscriptions)
	}
	registered := len(s.receivers)
	if registered > wanted {
		wanted = registered
	}
	downCount := 0
	for _, up := range s.receivers {
		if !up {
			downCount++
		}
	}
	senderDown := false
	for _, up := range s.senders {
		if !up {
			senderDown = true
			break
		}
	}
	s.mu.Unlock()

	// linkUp is the link-derived count of registered receivers whose AMQP
	// link is actually established. Health clamps the reported active count
	// to it (G) so a plan wanting more subscriptions than there are
	// self-establishing receivers cannot over-report active during startup.
	linkUp := registered - downCount

	var sl ports.ServiceLevel
	active := wanted
	switch {
	case !connected:
		sl = ports.ServiceLevelNone
		active = 0
	case downCount == 0 && !senderDown:
		sl = ports.ServiceLevelFull
	default:
		// A down receiver OR a down sender link degrades the session even
		// while the connection is alive.
		sl = ports.ServiceLevelDegraded
		active = wanted - downCount
		if active < 0 {
			active = 0
		}
	}
	if active > linkUp {
		active = linkUp
	}

	// a plan that wants more subscriptions than are currently active
	// (receivers still starting, or fewer receivers registered than the
	// reconciled plan requires) is NOT Full even when every registered
	// receiver's link is up. Report Degraded so readiness reflects the
	// missing subscriptions rather than over-reporting Full.
	if connected && active < wanted {
		sl = ports.ServiceLevelDegraded
	}

	health := ports.SessionHealth{
		Connected:           connected,
		SubscriptionsWanted: wanted,
		SubscriptionsActive: active,
		Ready:               connected,
		ServiceLevel:        sl,
	}
	// Surface the underlying cause on any non-Full state so a readiness
	// probe / operator sees WHY the session degraded, not just that it did.
	if sl != ports.ServiceLevelFull {
		health.LastError = lastErr
	}
	return health
}

// reserveLink records a build-time link reservation on this session and
// enforces the dedicated-session contract for durable AMQP 1.0 receivers
// durableReceiver reports whether the link being built is a
// durable receiver (durability_mode > 0).
//
// It returns a non-nil error when the reservation would place a durable
// receiver on a session that already hosts a link, or place any link on a
// session already claimed by a durable receiver. Because a durable
// receiver's close forces a full connection teardown that blips every
// sibling link, the safe topology is one durable receiver alone on its
// own session; this refusal makes that topology mandatory at build time
// rather than a documentation-only recommendation.
//
// Called once per link at factory build time. Links are built once per
// session lifetime — Reconcile stores the plan and never rebuilds links —
// so the counter is not perturbed by reconnects.
func (s *Session) reserveLink(durableReceiver bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case durableReceiver && s.builtLinkCount > 0:
		return shared.ErrInvalidPayload.WithMessage(
			"durable receiver (durability_mode > 0) requires a dedicated session (its own session_id): " +
				"the session already hosts another receiver or sender, and closing a durable receiver forces " +
				"a full connection teardown that blips every sibling link — give the durable receiver its own " +
				"session_id (dedicated-session contract)")
	case !durableReceiver && s.builtDurableReceiver:
		return shared.ErrInvalidPayload.WithMessage(
			"session already hosts a durable receiver (durability_mode > 0), which requires a dedicated " +
				"session (its own session_id): move this receiver or sender to a different session so a durable " +
				"close cannot blip it (dedicated-session contract)")
	case durableReceiver:
		s.builtDurableReceiver = true
	}
	s.builtLinkCount++
	return nil
}

// registerReceiver records r as a live receiver whose link health feeds
// Session.Health. The link starts down until markReceiverLink reports it
// up. Receivers register at Run start and unregister when Run exits.
func (s *Session) registerReceiver(r *Receiver) {
	s.mu.Lock()
	if s.receivers == nil {
		s.receivers = make(map[*Receiver]bool)
	}
	s.receivers[r] = false
	s.mu.Unlock()
}

// unregisterReceiver removes r from health tracking when its Run loop
// exits.
func (s *Session) unregisterReceiver(r *Receiver) {
	s.mu.Lock()
	delete(s.receivers, r)
	s.mu.Unlock()
}

// markReceiverLink updates the link-up state of an already-registered
// receiver. Unknown receivers are ignored so a late callback after
// unregister cannot resurrect an entry, and direct handleLinkError calls
// in unit tests (where the receiver never ran) stay no-ops.
func (s *Session) markReceiverLink(r *Receiver, up bool) {
	s.mu.Lock()
	if _, ok := s.receivers[r]; ok {
		s.receivers[r] = up
	}
	s.mu.Unlock()
}

// markAllReceiversDownLocked flips every registered receiver to
// link-down. The caller must hold s.mu. Used on connection loss so
// Health reflects the outage until receivers re-establish their links
// after reconnect.
func (s *Session) markAllReceiversDownLocked() {
	for r := range s.receivers {
		s.receivers[r] = false
	}
}

// registerSender records sn as a live sender whose link health feeds
// Session.Health. Called when a Sender establishes its link;
// the entry is removed by unregisterSender on Close.
func (s *Session) registerSender(sn *Sender) {
	s.mu.Lock()
	if s.senders == nil {
		s.senders = make(map[*Sender]bool)
	}
	s.senders[sn] = true
	s.mu.Unlock()
}

// unregisterSender removes sn from health tracking when it is closed.
func (s *Session) unregisterSender(sn *Sender) {
	s.mu.Lock()
	delete(s.senders, sn)
	s.mu.Unlock()
}

// markSenderLink updates the link-up state of an already-registered
// sender. Unknown senders are ignored, mirroring markReceiverLink.
func (s *Session) markSenderLink(sn *Sender, up bool) {
	s.mu.Lock()
	if _, ok := s.senders[sn]; ok {
		s.senders[sn] = up
	}
	s.mu.Unlock()
}

// markAllSendersDownLocked flips every registered sender to link-down.
// The caller must hold s.mu. Used on connection loss.
func (s *Session) markAllSendersDownLocked() {
	for sn := range s.senders {
		s.senders[sn] = false
	}
}

// resetSendersForReconnectLocked drops every registered sender entry on
// a fresh connection. The caller must hold s.mu.
//
// Unlike receivers — whose Run loop re-attaches and calls
// markReceiverLink(r, true) with no traffic after a reconnect — a Sender
// has NO background reattach path: its link is re-created lazily on the
// next application Send. Leaving senders in the down state stamped by
// notifyDisconnect would therefore report the session Degraded (with a
// nil LastError, since connect() clears it) for the entire post-reconnect
// window until the next Send — a false positive that could pull a healthy
// low-traffic egress pod out of rotation on a ServiceLevel readiness
// probe. Cleared entries re-register (up) on the next Send via
// createLink; only a genuine handleSendFailure (broker refusing a
// publish) then degrades the session.
func (s *Session) resetSendersForReconnectLocked() {
	for sn := range s.senders {
		delete(s.senders, sn)
	}
}

// noteLinkError records the classified cause of a link/connection fault
// so Session.Health can surface it via LastError on a non-Full state.
// A nil error is ignored.
func (s *Session) noteLinkError(err error) {
	if err == nil {
		return
	}
	be := MapError(err)
	s.mu.Lock()
	s.lastErr = be
	s.mu.Unlock()
}

// Events returns the channel on which session lifecycle events are emitted.
func (s *Session) Events() <-chan ports.SessionEvent {
	return s.events
}

// Subscribe returns a private buffered channel of lifecycle events plus
// an unsubscribe function. See AMQP 0-9-1 documentation for the
// semantics; both transports implement the same contract.
func (s *Session) Subscribe() (<-chan ports.SessionEvent, func()) {
	ch := make(chan ports.SessionEvent, eventChannelSize)

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

// Close gracefully disconnects the AMQP 1.0 session.
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
	subs := s.eventSubs
	s.eventSubs = nil
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
	for _, sub := range subs {
		close(sub)
	}
	return firstErr
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
					"amqp10: event dropped, channel full",
					"event_type", t,
				)
			}
			s.metrics.Counter(MetricAMQP10EventDropped, 1)
		}
	}

	for _, sub := range s.eventSubs {
		select {
		case sub <- ev:
		default:
			s.metrics.Counter(MetricAMQP10EventDropped, 1)
		}
	}
}

// monitorLoop watches for connection loss and attempts automatic
// reconnection. It selects on the connection's Done() channel for
// immediate disconnect detection. The configurable ticker
// (SessionOptions.ConnectionMonitorFallback) is only a reconnect
// backstop: tryReconnect no-ops while s.conn is still non-nil, so the
// ticker neither probes nor detects a silently half-dropped-but-still-
// installed connection — that half-open case is surfaced by the AMQP
// idle_timeout (SessionOptions.IdleTimeout), not by this ticker.
func (s *Session) monitorLoop(ctx context.Context) {
	fallback := s.clock().NewTicker(s.opts.ConnectionMonitorFallback)
	defer fallback.Stop()

	for {
		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()

		var connDone <-chan struct{}
		if conn != nil {
			connDone = conn.Done()
		}

		if connDone != nil {
			select {
			case <-ctx.Done():
				return
			case <-s.reconnectCh:
				s.tryReconnect(ctx)
			case <-connDone:
				s.handleConnLost(ctx, conn)
			case <-fallback.C():
				s.tryReconnect(ctx)
			}
		} else {
			select {
			case <-ctx.Done():
				return
			case <-s.reconnectCh:
				s.tryReconnect(ctx)
			case <-fallback.C():
				s.tryReconnect(ctx)
			}
		}
	}
}

// tryReconnect is the fallback/reconnect-signal handler. It only acts
// when the connection is ALREADY known to be down (s.conn == nil): it is
// a backstop that retries the dial after a prior reconnect attempt has
// not yet succeeded. It deliberately does NOT probe a live connection —
// when s.conn is still non-nil it is a no-op — so it cannot detect a
// silent half-open drop (that is the AMQP idle_timeout's job). See
// monitorLoop and SessionOptions.ConnectionMonitorFallback.
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

// handleConnLost is invoked by the monitor loop when a live connection's
// Done() channel fires. It mirrors notifyDisconnect — clearing the
// connection so the subsequent reconnect actually dials — but is driven
// by the SDK's own liveness signal rather than a link error.
//
// Previously the monitor called tryReconnect on a Done()
// wakeup, but tryReconnect observed a still-non-nil s.conn, skipped the
// reconnect, and the loop immediately re-selected on the already-closed
// Done() channel — a tight busy-spin while Health still reported
// Connected=true. Clearing the connection here breaks that spin and
// drives a real reconnect.
//
// The lost guard makes this converge with notifyDisconnect: if a link
// error already swapped in a fresh connection (or the session closed),
// the stale Done() wakeup is a no-op.
func (s *Session) handleConnLost(ctx context.Context, lost amqpConn) {
	s.mu.Lock()
	if s.closed || s.conn != lost {
		s.mu.Unlock()
		return
	}
	s.conn = nil
	s.amqpSess = nil
	s.connected = false
	s.markAllReceiversDownLocked()
	// mark senders down too, symmetric with notifyDisconnect. A
	// Done()-driven connection loss invalidates sender links exactly as a
	// link-error-driven one does; omitting this skews Session.Health
	// (senders reported up on a dead connection until their next Send).
	s.markAllSendersDownLocked()
	s.mu.Unlock()

	_ = lost.Close()
	s.pushEvent(ports.SessionDisconnected, nil)
	s.handleReconnect(ctx)
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
			s.metrics.Counter(MetricAMQP10Reconnects, 1,
				shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ContainerID})
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
		case <-s.clock().After(sleepDur):
		}

		delay = time.Duration(float64(delay) * s.opts.ReconnectMultiplier)
		if delay > s.opts.ReconnectMaxDelay {
			delay = s.opts.ReconnectMaxDelay
		}
	}
}

// notifyDisconnect is called by receivers/senders when they detect
// a link or session error indicating connection loss. It clears the
// connection state so the monitor goroutine triggers reconnection.
// The failedConn parameter identifies which connection instance failed;
// if the session has already reconnected to a new connection, the stale
// notification is ignored.
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
	if err != nil {
		s.lastErr = MapError(err)
	}
	s.markAllReceiversDownLocked()
	s.markAllSendersDownLocked()
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
