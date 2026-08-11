package amqp091

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
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

	// reconcileMu serialises reconcile() across its three drivers — Start,
	// the public Reconcile, and the reconnect loop. Without it a caller-
	// driven Reconcile can interleave with a reconnect-driven reconcile:
	// both open channels, declare exchanges/queues/bindings and recompute
	// activeSubs, so a partially-applied plan or a torn activeSubs view
	// could result. It is a DIFFERENT lock from mu and is always acquired
	// first (reconcileMu → mu, never the reverse); reconcile only ever holds
	// mu for short critical sections while reconcileMu is held, so the
	// broker I/O in declare* does not block the whole session on mu.
	reconcileMu sync.Mutex

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

	// forceReconnect wakes the reconnectLoop to reconnect NOW, out of band
	// from a NotifyClose. Credential/TLS rotation (ApplyCredentials) uses it
	// to drive an immediate reconnect after it force-drops the live
	// connection, instead of relying on the stale connection's async Close
	// eventually firing NotifyClose (which never happens if that Close wedges
	// on a half-dead broker). Buffered (cap 1) and coalesced: multiple
	// rotations while a reconnect is already scheduled collapse to one.
	forceReconnect chan struct{}

	// activeCloses bounds the number of in-flight detached connection-close
	// goroutines (closeConnAsync). Under a sustained outage every reconnect
	// attempt discards a connection whose Close can itself wedge on
	// connection.close-ok; without a cap each attempt would park another
	// close goroutine forever. maxConcurrentCloses is the ceiling.
	activeCloses atomic.Int64

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

	// authFailureCB is the reactive-recovery hook. The
	// CredentialRefresher injects a URI-bound callback via
	// SetAuthFailureCallback; reportAuthFailure invokes it when a live reconnect
	// dial maps a broker error to shared.ErrNotAuthorized (403 access-refused),
	// forcing an immediate re-resolve rather than stalling on revoked
	// credentials until the next poll. atomic.Pointer gives safe publication
	// across the builder goroutine (setter) and the reconnect goroutine (load).
	authFailureCB atomic.Pointer[func(error)]
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
		opts:           opts,
		mode:           mode,
		logger:         logger,
		metrics:        m,
		dial:           defaultDialFromOpts(opts),
		clk:            opts.Clock,
		events:         make(chan ports.SessionEvent, 16),
		activeSubs:     make(map[string]bool),
		reconnected:    make(chan struct{}, 1),
		forceReconnect: make(chan struct{}, 1),
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
// which the explicit/rotated credentials override: the embedded
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

// connectionIfReady returns the live AMQP connection ONLY when the session
// is fully connected AND reconciled (connected==true). It returns nil during
// the reconnect window — after the connection is installed (so a concurrent
// Close can tear it down) but BEFORE reconcile has restored the topology.
//
// The sender gates channel-open on this (rather than Connection()) so a
// publish that races the reconcile window sees a transient "not connected"
// (retryable) instead of an unroutable MANDATORY return the incomplete
// topology would otherwise produce: a mandatory publish to a not-yet-rebound
// exchange comes back as a basic.return -> ErrNotFound (Permanent) and would
// DLQ/drop a message that is fine to retry once reconcile rebinds. (A
// missing-exchange 404 is already transient WITHOUT this gate — the SDK nacks
// the pending confirm via deferredConfirmations.Close() rather than surfacing
// the *amqp.Error on the publish path — so the gate's real job is the
// mandatory-return case.) This mirrors the receiver, which already waits for
// SessionConnected/SessionReconciled before consuming.
func (s *Session) connectionIfReady() amqpConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return nil
	}
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
		// Bound the initial topology declaration by ConnectTimeout: the
		// declare calls are ctx-less (see reconcile), so a caller that passes
		// a deadline-less ctx (e.g. context.Background()) would otherwise let
		// a single wedged declare hang Start indefinitely on a half-dead broker.
		reconcileCtx, reconcileCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
		err := s.reconcile(reconcileCtx, conn, *plan)
		reconcileCancel()
		if err != nil {
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
				// connection; only close what we still own. Detached, because
				// a reconcile that failed on a DEADLINE left a half-dead broker
				// whose synchronous Close would itself wedge (it also unblocks
				// the abandoned declare goroutine).
				s.closeConnAsync(conn)
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
//
// Ordering: Reconcile MAY be called before Start. The plan is retained and
// Start applies it during its initial reconcile, so a pre-Start call is a
// no-op that returns nil rather than an error (the runtime legitimately
// configures topology before the connection is opened). Only a call on an
// already-closed session returns ErrUnavailable.
func (s *Session) Reconcile(ctx context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("amqp091: session closed")
	}
	s.plan = &plan
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		// Not started yet: the plan is retained above and Start applies it
		// during its initial reconcile. This is a valid ordering, so report
		// success — the declarations happen on Start.
		//
		// ponytail: cross-transport divergence — the amqp10 sibling returns
		// ErrUnavailable for the same before-Start call. Aligning the two is
		// tracked as an ADR (amqp10 is outside this module's scope).
		return nil
	}

	// Bound the declaration by ConnectTimeout — the declare calls are ctx-less
	// (see reconcile). On the deadline (a half-dead broker), drop the
	// connection so the abandoned declare goroutine unwinds and the reconnect
	// loop redials; the close is detached because a synchronous Close would
	// itself wedge on the same dead broker. A FAST declare failure (a topology
	// or permission error on a HEALTHY connection) is returned WITHOUT dropping
	// the connection — cycling it would only re-fail the same plan.
	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, s.opts.ConnectTimeout)
	err := s.reconcile(reconcileCtx, conn, plan)
	timedOut := reconcileCtx.Err() != nil
	reconcileCancel()
	if err != nil && timedOut {
		s.closeConnAsync(conn)
	}
	return err
}

func (s *Session) reconcile(ctx context.Context, conn amqpConnection, plan connectivity.SessionPlan) error {
	// Serialise all reconciles (Start / public Reconcile / reconnect loop)
	// so their channel opens and topology declarations cannot interleave.
	// Acquired before mu (never the reverse) — see reconcileMu's field doc.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	reconcileStart := s.clock().Now()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "amqp091: reconcile",
			"subscriptions", len(plan.Subscriptions),
			"publishers", len(plan.Publishers),
		)
	}

	// activeSubs is NOT reset here. It is committed from the queue names that
	// topology declaration returns LOCALLY, and only after a successful declare
	// under a generation guard (see below). Resetting up-front would (a) make a
	// FAILED reconcile drop the last-known-good view, and (b) — combined with
	// the in-goroutine write this refactor removed — let a timed-out plan-A
	// declare that later unwinds clobber a newer plan-B's subscriptions
	// On failure activeSubs keeps its last-known-good value.

	// Topology declaration is bounded by ctx: the amqp091-go declare calls
	// (Channel/ExchangeDeclare/QueueDeclare/QueueBind — see acl_session.go)
	// are NOT context-aware and block on a half-dead broker until it answers
	// or the connection dies. Without a bound, a single wedged declare hangs
	// reconnect (connected stays false, receivers stay down), Start, and the
	// public Reconcile past the configured ConnectTimeout. declareTopologyWithin
	// runs the declarations raced against ctx; on the deadline the driver drops
	// the connection so the abandoned SDK call unwinds and the reconnect loop
	// redials (see doReconnect / Start / Reconcile).
	declaredQueues, err := s.declareTopologyWithin(ctx, conn, plan)
	if err != nil {
		return err
	}

	// Commit activeSubs from the LOCALLY-returned queue names, and only if this
	// reconcile's connection is still the installed one (generation guard). The
	// commit happens HERE — never inside the declare goroutine — so a plan-A
	// declare that timed out and later unwinds cannot write stale subscriptions
	// over the connection a newer plan-B installed.
	s.mu.Lock()
	if s.conn == conn {
		next := make(map[string]bool, len(declaredQueues))
		for _, q := range declaredQueues {
			next[q] = true
		}
		s.activeSubs = next
	}
	s.mu.Unlock()

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

// declareOutcome carries the result of an abandonable topology declaration:
// the queue names successfully declared and the terminal error (nil on
// success). It travels on a buffered channel so an abandoned declare goroutine
// can deposit its result and exit without blocking — and, crucially, WITHOUT
// mutating session state. activeSubs is committed by reconcile from these
// returned names under a generation guard, so a timed-out plan-A declare that
// later unwinds cannot write stale subscriptions over a newer plan-B.
type declareOutcome struct {
	queues []string
	err    error
}

// declareTopologyWithin runs the plan's exchange/queue/binding declarations
// bounded by ctx and returns the names of the queues successfully declared.
// The amqp091-go declare calls (Channel/ExchangeDeclare/QueueDeclare/QueueBind)
// are NOT context-aware and block on a half-dead broker until it answers or the
// connection dies, so the work runs on a goroutine raced against ctx. done is
// BUFFERED (cap 1) so the abandoned goroutine's send never blocks and it exits
// on its own once the driver drops the connection and the wedged SDK call
// unwinds — no goroutine leak, and no session-state mutation from the abandoned
// goroutine. On the deadline the mapped ctx error is returned so the caller
// drops the connection and retries within the configured timeout.
func (s *Session) declareTopologyWithin(ctx context.Context, conn amqpConnection, plan connectivity.SessionPlan) ([]string, error) {
	done := make(chan declareOutcome, 1)
	go func() {
		queues, err := s.declareTopology(conn, plan)
		done <- declareOutcome{queues: queues, err: err}
	}()

	select {
	case out := <-done:
		return out.queues, out.err
	case <-ctx.Done():
		// Prefer a declaration that completed in the same instant the deadline
		// fired over reporting a spurious timeout.
		select {
		case out := <-done:
			return out.queues, out.err
		default:
		}
		if s.logger != nil {
			s.logger.Warn("amqp091: topology declaration exceeded deadline; "+
				"abandoning and dropping the connection",
				"broker", s.safeBrokerURL(), "error", ctx.Err())
		}
		return nil, MapError(ctx.Err())
	}
}

// declareTopology performs the plan's exchange/queue/binding declarations and
// returns the queue names it successfully declared (for reconcile to commit
// into activeSubs). Subscription declares are FATAL — you cannot consume from a
// queue that cannot be declared — so the first error aborts and the queues
// declared so far are returned alongside it (reconcile discards them on error).
// Publisher-exchange declares are BEST-EFFORT (see the inline rationale). It
// runs on the goroutine declareTopologyWithin spawns so the ctx-less SDK calls
// can be abandoned on the deadline; it MUST NOT mutate session state.
func (s *Session) declareTopology(conn amqpConnection, plan connectivity.SessionPlan) ([]string, error) {
	queues := make([]string, 0, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		queueName, err := s.declareSubscription(conn, sub)
		if err != nil {
			return queues, err
		}
		queues = append(queues, queueName)
	}

	for _, pub := range plan.Publishers {
		if err := s.declarePublisher(conn, pub); err != nil {
			// Publisher-exchange auto-declare is BEST-EFFORT, unlike
			// subscription declare above (which is fatal — you cannot consume
			// from a queue that cannot be declared). The sender never declared
			// its exchange before; publishing to an externally-managed or
			// least-privilege exchange worked without it. An active re-declare
			// of such an exchange legitimately fails (PRECONDITION_FAILED on a
			// topology mismatch, ACCESS_REFUSED without configure permission),
			// yet publishing to it still works. Aborting reconcile here would
			// take a previously-working publish route DOWN. So warn + meter and
			// continue: a genuinely-absent exchange the bridge cannot create
			// still fails visibly at publish time (404 -> retry/DLQ), exactly as
			// it did before this auto-declare existed (ADV).
			s.metrics.Counter(MetricAMQP091PublisherDeclareFailed, 1,
				shared.Tag{Key: shared.TagKeyEntity, Value: pub.Topic})
			if s.logger != nil {
				s.logger.Warn("amqp091: publisher exchange auto-declare failed; "+
					"continuing (publish still works if the exchange already exists)",
					"exchange", pub.Topic, "error", err)
			}
		}
	}

	return queues, nil
}

// closeConnAsync closes conn without blocking the caller. A synchronous
// conn.Close() on a half-dead broker blocks in the SDK (it waits for the
// connection.close-ok, ultimately bounded only by the heartbeat read
// deadline), which would stall reconnect retries, Start, credential rotation
// and topology-declaration give-up. The close is detached and deadline-bounded:
// its only job is to unwedge any in-flight SDK call and free the socket.
//
// It is also CAPPED: under a sustained outage every reconnect
// attempt discards a connection whose Close can itself wedge waiting for
// connection.close-ok. A plain fire-and-forget go conn.Close() would park a new
// goroutine on every attempt and leak unboundedly — the exact outage-shape leak
// fixed, reintroduced on the close side. Two bounds apply: each close
// runs under conn.CloseDeadline so it cannot park past the dial timeout even if
// the broker never answers, and at most maxConcurrentCloses close goroutines
// run at once — beyond that the connection is dropped without an explicit close
// (the OS reaps the socket when the process/GC releases it, and the broker
// reaps the peer when heartbeats stop), which is strictly better than an
// unbounded goroutine pile-up.
func (s *Session) closeConnAsync(conn amqpConnection) {
	if conn == nil {
		return
	}
	if s.activeCloses.Add(1) > maxConcurrentCloses {
		s.activeCloses.Add(-1)
		return
	}
	deadline := s.clock().Now().Add(dialTimeout(s.opts))
	go func() {
		defer s.activeCloses.Add(-1)
		_ = conn.CloseDeadline(deadline)
	}()
}

// declareSubscription opens a fresh AMQP channel for a single subscription's
// declarations (exchange, queue, bind) and returns the declared queue name.
// A separate channel per subscription ensures that a PRECONDITION_FAILED error
// on one declaration does not poison the channel for subsequent subscriptions
// in the same plan, since AMQP closes the channel on any soft error. It does
// NOT mutate activeSubs — reconcile commits the returned name under a
// generation guard so an abandoned declare goroutine cannot write stale
// state.
func (s *Session) declareSubscription(conn amqpConnection, sub connectivity.SubscriptionPlan) (string, error) {
	queueName := sub.Topic
	decl := subscriptionParams(sub)

	ch, err := conn.Channel()
	if err != nil {
		return "", MapError(err)
	}
	defer func() { _ = ch.Close() }()

	if decl.exchange != "" {
		if err := ch.ExchangeDeclare(decl.exchange, decl.exchangeType, decl.durable, decl.autoDelete, decl.exchangeArgs); err != nil {
			return "", MapError(err)
		}
	}

	if err := ch.QueueDeclare(queueName, decl.durable, decl.autoDelete, decl.queueArgs); err != nil {
		return "", MapError(err)
	}

	if decl.exchange != "" {
		routingKey := decl.routingKey
		if routingKey == "" {
			routingKey = queueName
		}
		if err := ch.QueueBind(queueName, routingKey, decl.exchange, decl.bindArgs); err != nil {
			return "", MapError(err)
		}
	}

	return queueName, nil
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
//
// Close honours ctx: conn.Close() is raced against ctx.Done() because the
// SDK's connection teardown can block ~20-30s on a half-dead broker (waiting
// for a Connection.Close-Ok that never arrives). When ctx fires first the
// detached goroutine still completes the underlying close; Close stops
// waiting so shutdown respects the caller's deadline (mirrors Receiver.Close).
func (s *Session) Close(ctx context.Context) error {
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
		// The reconnect goroutine may be parked in an SDK topology call
		// (dial/declare) that ignores ctx — the amqp091-go client's declares
		// are not context-aware — so an unconditional wait could overrun the
		// caller's deadline by up to a heartbeat. Race its exit against ctx.
		//
		// Leak/double-close safety does NOT depend on this wait: doReconnect
		// re-checks s.closed under s.mu after every dial and closes any
		// connection it owns (guarded by s.conn==conn), and Close's detached
		// conn.Close() below only ever closes the connection it captured
		// under the lock — so returning early here cannot leak a connection
		// or double-close one as the goroutine unwinds. pushEvent is also
		// s.closed-guarded under s.mu, so an in-flight event from the
		// unwinding goroutine cannot send on the closed events channel.
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	var closeErr error
	if conn != nil {
		// Race conn.Close() against ctx so a half-dead broker cannot wedge
		// shutdown for the SDK's ~20-30s handshake timeout. The detached
		// goroutine still completes the underlying close.
		cdone := make(chan error, 1)
		go func() { cdone <- conn.Close() }()
		select {
		case closeErr = <-cdone:
		case <-ctx.Done():
			closeErr = ctx.Err()
		}
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

	// maxConcurrentCloses bounds the number of in-flight detached
	// connection-close goroutines (closeConnAsync). Under a sustained outage
	// every reconnect attempt discards a connection whose Close can itself
	// wedge waiting for connection.close-ok; capping the close goroutines keeps
	// that from leaking one goroutine per attempt. A small constant
	// is plenty: closes are transient and CloseDeadline-bounded, so the queue
	// drains as fast as the dial timeout.
	maxConcurrentCloses = 4
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
			case <-s.forceReconnect:
				// Credential/TLS rotation dropped the live connection and asked
				// for an immediate reconnect. s.conn is already nil
				// (ApplyCredentials cleared it under the lock), so redial now
				// rather than waiting for the stale connection's async Close to
				// fire NotifyClose — which never happens if that Close wedges on
				// a half-dead broker.
				s.doReconnect(ctx)
			}
			continue
		}

		notifyCh := conn.NotifyClose()

		select {
		case <-ctx.Done():
			return
		case <-s.forceReconnect:
			// Rotation forced a reconnect while we were watching a now-stale
			// connection. ApplyCredentials already dropped s.conn and started
			// its detached close, so drive the reconnect immediately instead of
			// blocking on this connection's NotifyClose.
			s.doReconnect(ctx)
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

			// Absorb any pending forceReconnect token: this NotifyClose-driven
			// reconnect already accomplishes what a concurrent rotation asked
			// for, so a leftover token must not drive a SECOND doReconnect that
			// would overwrite — and leak — the freshly reconnected connection
			// (doReconnect installs without closing the previous s.conn).
			select {
			case <-s.forceReconnect:
			default:
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
			// reactive-recovery chokepoint: every reconnect attempt
			// redials here, and a hard rotation that revoked the old
			// credentials fails the dial with 403 access-refused, which
			// MapError classifies as shared.ErrNotAuthorized. Report it so the
			// refresher forces an immediate re-resolve instead of retrying the
			// same revoked material until the next poll. reportAuthFailure
			// filters non-auth dial errors (connection refused, timeout).
			//
			// ponytail: AMQP 0-9-1 authenticates at connection.open, so a
			// credential revocation manifests as a reconnect auth failure — the
			// single funnel for every channel/consumer/publisher on the
			// connection — not a per-message fault.
			s.reportAuthFailure(MapError(err))
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
					// Detached: a reconcile that failed on a DEADLINE left a
					// half-dead broker whose synchronous Close would wedge and
					// stall this retry loop (it also unblocks the abandoned
					// declare goroutine). Fire-and-forget is safe — we already
					// dropped s.conn, so the next attempt redials.
					s.closeConnAsync(conn)
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
