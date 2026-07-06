package paho

import (
	"context"
	"net/url"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// ConnectionManager returns the underlying autopaho.ConnectionManager.
//
// This accessor lives in the ACL because its return type is an SDK
// pointer; port-side code MUST NOT call it. It is retained because
// existing tests (and the historical Sender code path) use it as the
// only way to reach the live CM. Tests that swap in a stub assign
// Session.cm directly with a pahoConnection (typically a *pahoConn
// wrapping a sentinel autopaho.ConnectionManager).
//
//aclcheck:allow-export
func (s *Session) ConnectionManager() *autopaho.ConnectionManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cm == nil {
		return nil
	}
	return s.cm.Underlying()
}

// classifySubackReasons walks the SUBACK reason codes one-to-one with
// the requested subscriptions and partitions them into accepted and
// rejected. The first rejection's classified BridgeError is returned
// alongside the offending topic so the caller can surface a meaningful
// failure, but EVERY accepted topic is included in the succeeded slice
// so the caller can persist a faithful view of broker state.
//
// Topics whose reason index is out of range (broker returned a short
// SUBACK) are conservatively treated as accepted — matching the
// previous implementation and avoiding gratuitous unsubscribe loops.
func classifySubackReasons(toSub []subscribeSpec, reasons []byte) (
	succeeded []subscribeSpec, firstErr *shared.BridgeError, errTopic string,
) {
	succeeded = make([]subscribeSpec, 0, len(toSub))
	for i, opt := range toSub {
		if i < len(reasons) {
			if berr := MapSubscribeReasonCode(reasons[i]); berr != nil {
				if firstErr == nil {
					firstErr = berr
					errTopic = opt.Topic
				}
				continue
			}
		}
		succeeded = append(succeeded, opt)
	}
	return succeeded, firstErr, errTopic
}

// Start connects to the MQTT broker and emits a SessionConnected event
// once the initial connection is established. Calling Start on an
// already-started session is a no-op (idempotent).
//
// Concurrent Start calls are synchronized: while one attempt is in
// flight, other callers WAIT for its outcome instead of returning a
// false success. If the winner succeeds they return nil (session is
// up); if it fails they retry the attempt themselves; if their context
// expires while waiting they get a definite error. This closes the
// window where a racing Reload observed "started" and silently skipped
// its TLS rebuild.
//
// A Session is single-use: once Close has been called, Start returns
// ErrUnavailable and does NOT attempt a new connection. This prevents
// a "zombie" state where a freshly attached cm coexists with an
// already-closed events channel.
//
// Start lives in the ACL because the entire body builds an
// autopaho.ClientConfig and registers paho-typed callbacks with the
// SDK. The orchestration around it (Reload, Close, Reconcile) sits in
// SDK-free port-side files and drives Start through the
// pahoConnection seam installed below.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	for {
		if s.closed {
			s.mu.Unlock()
			return shared.ErrUnavailable.
				WithMessage("mqtt session is closed; Start is not allowed after Close").
				Wrap(shared.ErrTransportClosedPermanently)
		}
		if s.cm != nil {
			s.mu.Unlock()
			return nil
		}
		if !s.starting {
			break
		}
		// Another Start attempt is in flight: wait for its outcome
		// rather than reporting success for work we did not do.
		done := s.startDone
		s.mu.Unlock()
		select {
		case <-done:
			// Winner finished — loop to observe the result: success
			// (cm != nil → nil), or failure (starting == false, cm ==
			// nil → this caller runs its own attempt).
		case <-ctx.Done():
			return MapError(ctx.Err()).WithMessage("waiting for concurrent Start to finish")
		}
		s.mu.Lock()
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.mu.Unlock()

	// finishStart publishes the attempt's outcome: clears the in-flight
	// flag and wakes every waiter. Deferred-closed channel (not reused)
	// so late waiters holding the old channel still unblock.
	finishStart := func() {
		s.mu.Lock()
		s.starting = false
		close(s.startDone)
		s.mu.Unlock()
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connecting",
			"client_id", s.opts.ClientID,
			"broker_count", len(s.opts.BrokerURLs),
			"session_mode", s.mode,
		)
	}
	connectStart := s.clock().Now()

	// Dial: build the ClientConfig, create the ConnectionManager and await
	// the initial CONNACK. Overridable in tests (connectOverride) so the
	// closed-during-Start re-check and credential-driven Reload can be
	// exercised without a live broker.
	dial := s.dial
	if s.connectOverride != nil {
		dial = s.connectOverride
	}
	conn, cmCancel, err := dial(ctx)
	if err != nil {
		finishStart()
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "mqtt: connect failed",
				"client_id", s.opts.ClientID, "error", err)
		}
		return err
	}

	// Close/Start race guard: dial released s.mu and may have blocked for
	// up to connect_timeout inside AwaitConnection. If Close ran while we
	// were connecting, s.closed is now true — installing this CM would
	// leak a zombie ConnectionManager that autopaho reconnects forever,
	// fighting the replacement session for the ClientID. Tear it down
	// instead. The check + install are one atomic section under s.mu, so
	// it cannot interleave with Close's (closed=true; read cm) section.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cmCancel()
		_ = conn.Disconnect(context.Background())
		finishStart()
		return shared.ErrUnavailable.WithMessage(
			"mqtt session closed during Start; discarded the freshly connected ConnectionManager")
	}
	s.cm = conn
	s.cmCancel = cmCancel
	s.connected = true
	s.starting = false
	close(s.startDone)
	s.mu.Unlock()

	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(MetricMQTTConnectLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connected",
			"client_id", s.opts.ClientID, "connect_latency", elapsed)
	}

	return nil
}

// dial builds the autopaho ClientConfig, creates the ConnectionManager
// and blocks until the initial CONNACK (bounded by connect_timeout). It
// returns the SDK-free pahoConnection seam plus the cancel func that
// tears down the CM's background context. This is the production
// implementation of the Start dial seam (see Session.connectOverride);
// it lives in the ACL because the whole body is SDK-typed. Returning the
// pahoConnection interface is the whole point of the seam — it keeps the
// SDK ConnectionManager out of session.go — so the ireturn rule is waived.
//
//nolint:ireturn // adapter-internal dial seam; returning the interface is its purpose (category 5)
func (s *Session) dial(ctx context.Context) (pahoConnection, context.CancelFunc, error) {
	// Snapshot the TLS options pointer under the mutex: ApplyCredentials
	// swaps in a NEW *TLSConfig on rotation (copy-on-write), so holding
	// this snapshot gives dial immutable TLS material even if a rotation
	// lands mid-connect. Seed liveCreds from the current options so the
	// first CONNECT picks them up via the ConnectPacketBuilder.
	s.mu.Lock()
	tlsOpts := s.opts.TLS
	s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password.Reveal()}
	s.mu.Unlock()

	serverURLs, err := parseURLs(s.opts.BrokerURLs)
	if err != nil {
		return nil, nil, shared.ErrInvalidPayload.Wrap(err).WithMessage("parse broker URLs")
	}

	cfg := autopaho.ClientConfig{
		ServerUrls: serverURLs,
		KeepAlive:  s.opts.KeepAlive,

		// OnConnectionUp fires on every (re)connect. Per finding C7, the
		// runtime session manager is the SINGLE owner of reconnect
		// reconciliation and failure propagation: it observes the
		// SessionConnected event emitted here and drives Reconcile, whose
		// outcome is authoritative (a rejected re-subscribe propagates out
		// of Manager.Run so the bridge can restart/alarm — finding S9).
		// This callback therefore only resets local subscription state and
		// signals SessionConnected; it MUST NOT reconcile inline. See
		// handleConnectionUp for the reset-before-signal ordering that lets
		// the manager observe an empty subscription set on reconnect.
		OnConnectionUp: func(_ *autopaho.ConnectionManager, _ *pahov5.Connack) {
			s.handleConnectionUp()
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
			return true
		},

		ClientConfig: pahov5.ClientConfig{
			// ClientID is the MQTT session identity. The broker keys a
			// persistent session (subscriptions + queued QoS 1/2 messages)
			// to this ID, so the identity strategy differs by mode:
			//   - Exclusive (clustered failover): the ClientID MUST be
			//     STABLE and SHARED across all instances of the logical
			//     session. When the active instance dies and a standby
			//     reconnects with the SAME ClientID, the broker performs
			//     "session takeover" and hands the resumed session — and
			//     its queued messages — to the new owner. A unique-per-
			//     instance ClientID would create a SEPARATE broker session,
			//     so queued QoS messages would never reach the standby and
			//     failover continuity is lost.
			//   - Persistent (single instance, durable across restarts):
			//     stable ClientID, CleanStart=false; the same process
			//     resumes its own session.
			//   - Ephemeral: ClientID may be unique; CleanStart=true forces
			//     a fresh session on every connect (no continuity expected).
			// The lease (one active owner) is what makes a shared Exclusive
			// ClientID safe — at most one holder connects at a time.
			ClientID: s.opts.ClientID,

			// Manual acknowledgment is the load-bearing delivery
			// guarantee of this adapter: the client does NOT auto-ack
			// when the publish callback returns; the PUBACK/PUBCOMP is
			// sent when the runtime settles the Delivery (Delivery.Ack
			// after outbox persist / pipeline completion). The paho
			// client serialises acks in receive order internally, so
			// out-of-order settlement is safe. See delivery.go and
			// acl_router.go. NOTE: Router must NOT also be set — the SDK
			// would then dispatch every publish twice.
			EnableManualAcknowledgment: true,
			OnPublishReceived: []func(pahov5.PublishReceived) (bool, error){
				s.router.onPublishReceived,
			},

			// Server-initiated DISCONNECT observer: feeds session-takeover
			// (0x8E) damping so two instances sharing a client_id cannot
			// silently kick each other forever. autopaho invokes this in
			// its own goroutine.
			OnServerDisconnect: func(d *pahov5.Disconnect) {
				var code byte
				if d != nil {
					code = d.ReasonCode
				}
				s.handleServerDisconnect(code)
			},
		},
	}

	// Will (LWT): registered with the broker at CONNECT so peers can
	// detect ungraceful death of this bridge instance. Validated in
	// NewSession/Config.Validate.
	if w := s.opts.Will; w != nil {
		cfg.WillMessage = &pahov5.WillMessage{
			Topic:   w.Topic,
			Payload: []byte(w.Payload),
			QoS:     w.QoS,
			Retain:  w.Retain,
		}
	}

	// Reconnect pacing: base delay from reconnect_delay (SDK default
	// 10s), plus an escalating session-takeover penalty so a ClientID
	// collision (two instances mutually kicking each other) backs off
	// instead of storming; see noteSessionTakeover.
	base := autopaho.NewConstantBackoff(10 * time.Second)
	if s.opts.ReconnectDelay > 0 {
		base = autopaho.NewConstantBackoff(s.opts.ReconnectDelay)
	}
	cfg.ReconnectBackoff = func(attempt int) time.Duration {
		return base(attempt) + s.takeoverPenalty()
	}

	// reconnect_timeout bounds each individual (re)connect attempt —
	// dial, TLS handshake and CONNACK — inside autopaho's reconnect
	// loop (SDK default 10s). This is distinct from connect_timeout,
	// which bounds how long Start waits for the FIRST connection.
	if s.opts.ReconnectTimeout > 0 {
		cfg.ConnectTimeout = s.opts.ReconnectTimeout
	}

	// ConnectPacketBuilder customisations are accumulated here and merged
	// into a single builder below.
	//
	// ephemeralCleanStart: autopaho's CleanStartOnInitialConnection only
	// sends CleanStart=true on the FIRST connect; subsequent reconnects
	// send CleanStart=false. For SessionEphemeral that is wrong: after a
	// broker restart the server-side session is gone, but the client
	// claims to resume → the broker replies CONNACK SessionPresent=false
	// while the client's in-flight packet tracking still holds stale
	// state, causing QoS ≥ 1 publishes to hang waiting for a PUBACK
	// that will never arrive. Fix: force CleanStart=true on EVERY
	// connect via ConnectPacketBuilder.
	ephemeralCleanStart := false
	rm := s.opts.ReceiveMaximum

	switch s.mode {
	case connectivity.SessionEphemeral:
		cfg.CleanStartOnInitialConnection = true
		cfg.SessionExpiryInterval = 0
		ephemeralCleanStart = true
	case connectivity.SessionPersistent, connectivity.SessionExclusive:
		if s.opts.CleanStart && s.mode == connectivity.SessionExclusive {
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

	// Always install a ConnectPacketBuilder so credential rotation (via
	// ApplyCredentials) takes effect on the NEXT CONNECT — autopaho
	// invokes this hook once per connect attempt, picking up whatever is
	// currently in s.liveCreds. This replaces the static
	// cfg.ConnectUsername / cfg.ConnectPassword assignment.
	cfg.ConnectPacketBuilder = func(cp *pahov5.Connect, _ *url.URL) (*pahov5.Connect, error) {
		if ephemeralCleanStart {
			cp.CleanStart = true
		}
		if rm > 0 {
			if cp.Properties == nil {
				cp.Properties = &pahov5.ConnectProperties{}
			}
			cp.Properties.ReceiveMaximum = &rm
		}
		s.mu.Lock()
		user := s.liveCreds.Username
		pass := s.liveCreds.Password
		s.mu.Unlock()
		if user != "" {
			cp.UsernameFlag = true
			cp.Username = user
			cp.PasswordFlag = true
			cp.Password = []byte(pass)
		}
		return cp, nil
	}

	if tlsOpts != nil && tlsOpts.Enable {
		tlsCfg, err := BuildTLSConfig(tlsOpts)
		if err != nil {
			return nil, nil, shared.ErrUnavailable.Wrap(err).WithMessage("build TLS config")
		}
		cfg.TlsCfg = tlsCfg
	}

	// The CM's reconnection loop must outlive the caller's context.
	// We derive a background context that is only cancelled by Close().
	cmCtx, cmCancel := context.WithCancel(context.Background())

	cm, err := autopaho.NewConnection(cmCtx, cfg)
	if err != nil {
		cmCancel()
		return nil, nil, MapError(err)
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
		return nil, nil, MapError(err)
	}

	return newPahoConn(cm), cmCancel, nil
}
