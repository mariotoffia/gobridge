package paho

import (
	"context"
	"math/rand/v2"
	"net/url"

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
// pointer; port-side code MUST NOT call it. It is retained solely for
// tests that need to reach the raw CM; production egress now publishes
// through the SDK-free pahoConnection.PublishEnvelope seam via
// Session.connection() (F-2), so the Sender no longer depends on it.
// Tests that swap in a stub assign Session.cm directly with a
// pahoConnection (typically a *pahoConn wrapping a sentinel
// autopaho.ConnectionManager).
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

// classifySubackReasons walks the SUBACK reason codes ONE-TO-ONE with the
// requested subscriptions (by index) and partitions them into accepted and
// rejected. The first rejection's classified BridgeError is returned alongside
// the offending topic so the caller can surface a meaningful failure, but EVERY
// accepted topic is included in the succeeded slice so the caller can persist a
// faithful view of broker state.
//
// The one-to-one indexing TRUSTS the broker to return reason codes in request
// order, one per subscription (MQTT v5 §3.9.3). A broker that returns MORE
// reason codes than requested, or reorders them, is a spec violation this
// function does not defend against beyond the short-SUBACK check below — the
// extra/reordered codes are simply not consulted (loop bounds are toSub). This
// residual trust is a deliberate LOW: all mainstream brokers comply.
//
// Two protocol hazards ARE handled here:
//
//   - Short SUBACK (c4-short-suback): a broker that returns FEWER reason
//     codes than requested subscriptions leaves the tail topics
//     unconfirmed. Those topics have no broker proof of subscription, so
//     they are treated as a FAILURE (not silently assumed accepted) —
//     otherwise the health would report Full while a subscription was
//     never established and silently never delivers.
//   - Granted QoS (c4-qos-downgrade): a success reason code (0x00/0x01/
//     0x02) IS the QoS the broker granted, which may be LOWER than the
//     requested QoS. The succeeded spec carries the GRANTED QoS so the caller
//     can retain broker-observed state for cleanup while keeping that topic out
//     of the contract-active activeSubs map.
func classifySubackReasons(toSub []subscribeSpec, reasons []byte) (
	succeeded []subscribeSpec, firstErr *shared.BridgeError, errTopic string,
) {
	succeeded = make([]subscribeSpec, 0, len(toSub))
	for i, opt := range toSub {
		if i >= len(reasons) {
			// Short SUBACK: no reason code for this topic ⇒ no broker
			// confirmation. Treat it as a failure rather than assuming
			// acceptance (c4-short-suback); do NOT mark it active.
			if firstErr == nil {
				firstErr = shared.ErrProtocolError.WithMessage(
					"mqtt: SUBACK returned fewer reason codes than requested subscriptions")
				errTopic = opt.Topic
			}
			continue
		}
		if berr := MapSubscribeReasonCode(reasons[i]); berr != nil {
			if firstErr == nil {
				firstErr = berr
				errTopic = opt.Topic
			}
			continue
		}
		// Success: carry the GRANTED QoS (the reason-code value) so the caller
		// can distinguish broker-observed from contract-active state.
		granted := opt
		granted.QoS = reasons[i]
		succeeded = append(succeeded, granted)
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
		if s.terminalErr != nil {
			err := s.terminalErr
			s.mu.Unlock()
			return err
		}
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
	s.connectionGeneration++
	connectionGeneration := s.connectionGeneration
	s.connectionUpDone = make(chan struct{})
	s.connectionUpCompleted = false
	s.connectionUpErr = nil
	connectionUpDone := s.connectionUpDone
	if s.eventsClosed {
		// F-1: a prior Reload-failure closed s.events to signal terminal
		// death and trigger this supervisor re-Start. Re-materialise a
		// fresh events channel (same capacity) BEFORE dialing so the
		// reconnect's SessionConnected/SessionReconnecting events land in
		// the new buffer, and so the manager's handleEvents — which
		// re-reads Events() on each Run — does not spin on a closed
		// channel. Done under s.mu; pushEvent observes the cleared flag
		// and the new channel atomically.
		s.events = make(chan ports.SessionEvent, sessionEventsBuffer)
		s.eventsClosed = false
	}
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

	if err := s.loadManagedSubscriptionHistory(ctx); err != nil {
		finishStart()
		return err
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
		s.invalidateConnectionGeneration(connectionGeneration, err)
		finishStart()
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "mqtt: connect failed",
				"client_id", s.opts.ClientID, "error", err)
		}
		return err
	}

	// Test fakes historically model AwaitConnection and callback completion as
	// one operation. Production and explicit delayed-callback tests must signal
	// the barrier from handleConnectionUpGeneration.
	if s.connectOverride != nil && !s.connectOverrideAwaitConnectionUp {
		s.mu.Lock()
		if s.recoveryNeedsSessionPresent {
			s.recoveryNeedsSessionPresent = false
			s.recoverySessionPresent = true
		}
		s.mu.Unlock()
		s.completeConnectionUpBarrier(connectionGeneration, nil)
	}
	select {
	case <-connectionUpDone:
		s.mu.Lock()
		upErr := s.connectionUpErr
		currentGeneration := s.connectionGeneration
		s.mu.Unlock()
		if currentGeneration != connectionGeneration || upErr != nil {
			cmCancel()
			_ = conn.Disconnect(context.Background())
			finishStart()
			if upErr != nil {
				return upErr
			}
			return shared.ErrUnavailable.WithMessage("mqtt: connection generation changed before callback completion")
		}
	case <-ctx.Done():
		s.invalidateConnectionGeneration(connectionGeneration, ctx.Err())
		cmCancel()
		_ = conn.Disconnect(context.Background())
		finishStart()
		return MapError(ctx.Err()).WithMessage("mqtt: await connection-up callback completion")
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
	credsPresent := s.liveCreds.Username != "" || s.liveCreds.Password != ""
	allowPlaintext := s.opts.AllowPlaintextCredentials
	brokerURLs := s.opts.BrokerURLs
	connectionGeneration := s.connectionGeneration
	recoveryConnect := s.recoveryNeedsSessionPresent
	s.mu.Unlock()

	// HIGH-4 defense-in-depth: Factory.NewSession validates the plaintext-
	// credential transport before construction and Session.ApplyCredentials
	// re-gates on rotation, but a DIRECT NewSession caller (bypassing the
	// factory) — or any future path that seeds credentials onto opts — could
	// still reach dial with username/password bound to a plaintext transport.
	// Fail the dial CLOSED here so cleartext credentials can NEVER leave the
	// process on a non-TLS scheme without the explicit opt-in.
	if plaintextCredentialViolation(credsPresent, allowPlaintext, brokerURLs) {
		return nil, nil, errPlaintextCredentials()
	}

	serverURLs, err := parseURLs(s.opts.BrokerURLs)
	if err != nil {
		return nil, nil, shared.ErrInvalidPayload.Wrap(err).WithMessage("parse broker URLs")
	}

	// ponytail: M-6 / HIGH-5 — this session relies on autopaho's DEFAULT
	// IN-MEMORY packet/session store (cfg.Session left nil ⇒ state.NewInMemory
	// in autopaho). CEILING: outbound QoS 1/2 packets IN FLIGHT at process
	// death are LOST (the un-acked PUBLISH / PUBREL state does not survive),
	// and QoS 2 is therefore NOT exactly-once ACROSS a restart. This is NOT
	// fixed by client_id/clean_start=false (those resume BROKER-side session
	// state; the CLIENT-side outbound packet queue is what is volatile here).
	//
	// PRODUCTION CONTRACT: durable, at-least-once EGRESS is the BRIDGE's
	// responsibility, delivered by the shared_outbox / idempotent-replay
	// pattern at the route layer — the sender is invoked from a durable outbox
	// record and re-invoked on replay after a crash, so a lost in-flight
	// publish is re-sent. The MQTT plugin ALONE does not provide durable
	// egress; do NOT rely on MQTT QoS 1/2 as the sole loss-prevention for
	// outbound. The sender reports this via ports.NonDurableEgressReporter
	// (Sender.NonDurableEgress ⇒ true for QoS ≥ 1); the bridge consults it in
	// egressDurabilityAdvisory to warn only when a route's delivery mode would
	// settle the source before this non-durable boundary. Both wired modes
	// (direct_hold, shared_outbox) are loss-safe, so no advisory fires today.
	// docs/transports/mqtt.md documents the requirement. A file-backed
	// session.SessionManager (assigned to cfg.Session) is the deferred,
	// ADR-level alternative and is out of scope here (deferred finding M-6; see
	// scenario-01 docs).
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
		OnConnectionUp: func(_ *autopaho.ConnectionManager, connack *pahov5.Connack) {
			sessionPresent := connack != nil && connack.SessionPresent
			s.handleConnectionUpGenerationWithSessionPresent(connectionGeneration, sessionPresent)
		},
		OnConnectError: func(err error) {
			s.handleConnectError(err)
		},
		OnConnectionDown: func() bool {
			return s.handleConnectionDownGeneration(connectionGeneration)
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

	// Reconnect pacing (M-4): a JITTERED EXPONENTIAL base delay derived
	// from reconnect_delay (floor) and reconnect_max_delay (ceiling), plus
	// an escalating session-takeover penalty so a ClientID collision (two
	// instances mutually kicking each other) backs off instead of storming;
	// see newReconnectBackoff and noteSessionTakeover. Equal-jitter
	// desynchronises a fleet that all lost the same broker, avoiding a
	// thundering-herd reconnect the moment the broker returns.
	cfg.ReconnectBackoff = s.newReconnectBackoff(rand.Float64)

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
	maxPayload := s.opts.MaxPayloadBytes

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
			cfg.CleanStartOnInitialConnection = s.opts.CleanStart && !recoveryConnect
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
		// Advertise the self-imposed resource limits the broker MUST honour:
		// Receive Maximum (in-flight QoS 1/2 count) and — when MaxPayloadBytes
		// is set — Maximum Packet Size, which makes the pending-memory bound
		// receive_maximum × max_payload_bytes broker-ENFORCED (c-mempkt).
		applyConnectLimits(cp, rm, maxPayload)
		s.mu.Lock()
		user := s.liveCreds.Username
		pass := s.liveCreds.Password
		s.mu.Unlock()
		applyConnectCredentials(cp, user, pass)
		return cp, nil
	}

	if tlsOpts != nil && tlsOpts.Enable {
		tlsCfg, err := BuildTLSConfig(tlsOpts)
		if err != nil {
			return nil, nil, shared.ErrUnavailable.Wrap(err).WithMessage("build TLS config")
		}
		if tlsOpts.InsecureSkipVerify && s.logger != nil {
			// insecure_skip_verify disables server-certificate validation,
			// exposing the connection to MITM. It has legitimate test/mesh
			// uses, but must never pass unnoticed in production (L-3).
			s.logger.Warn("mqtt: TLS certificate verification DISABLED "+
				"(insecure_skip_verify) — the broker's identity is NOT "+
				"validated; use only on a trusted transport",
				"client_id", s.opts.ClientID)
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
		connectTimeout = DefaultConnectTimeout
	}
	awaitCtx, awaitCancel := context.WithTimeout(ctx, connectTimeout)
	defer awaitCancel()

	if err := cm.AwaitConnection(awaitCtx); err != nil {
		cmCancel()
		_ = cm.Disconnect(context.Background())
		return nil, nil, MapError(err)
	}

	return newPahoConn(cm, s.metrics), cmCancel, nil
}

// applyConnectCredentials sets the MQTT v5 CONNECT username/password fields
// and their presence flags INDEPENDENTLY.
//
// Minor fix: the previous logic gated the password on a non-empty username,
// so a password-without-username (a common token/JWT auth shape where the
// bearer token rides in the password with no username) was silently
// dropped. MQTT v5 [MQTT-3.1.2-16..21] permits Password Flag = 1 with
// Username Flag = 0, so each flag is now driven solely by whether its own
// value is present.
//
// This helper takes the SDK *paho.Connect and therefore lives in the ACL.
func applyConnectCredentials(cp *pahov5.Connect, user, pass string) {
	if user != "" {
		cp.UsernameFlag = true
		cp.Username = user
	}
	if pass != "" {
		cp.PasswordFlag = true
		cp.Password = []byte(pass)
	}
}

// mqttPacketOverheadAllowance is the byte headroom ADDED to the configured
// max payload when advertising the MQTT v5 Maximum Packet Size. MaximumPacketSize
// bounds the WHOLE packet (fixed header + variable header + properties +
// payload), so advertising exactly max_payload_bytes would make the broker
// reject a legitimate payload of exactly that size. 128 KiB comfortably covers
// the MQTT PUBLISH fixed header (≤5 B), a maximal topic name (≤65 535 B, a
// 2-byte length field), the packet identifier and a bounded set of properties —
// so an application payload up to max_payload_bytes is always admitted while the
// broker still refuses anything materially larger.
const mqttPacketOverheadAllowance uint64 = 128 << 10

// mqttMaxPacketSize is the MQTT v5 Maximum Packet Size ceiling: the largest
// value a Four Byte Integer may carry per the spec (§2.2.2.2), 256 MiB − 1.
// The derived packet-size limit is clamped to this so a huge max_payload_bytes
// cannot advertise an out-of-range (protocol-violating) value.
const mqttMaxPacketSize uint64 = 268_435_455

// maxPacketSizeFor derives the MQTT v5 Maximum Packet Size to advertise from the
// configured maximum application PAYLOAD size, adding mqttPacketOverheadAllowance
// so a payload of exactly maxPayloadBytes is still admitted (the limit covers
// the whole packet, not just the payload). The result is clamped to
// mqttMaxPacketSize. Callers guard maxPayloadBytes > 0 before advertising.
func maxPacketSizeFor(maxPayloadBytes uint32) uint32 {
	sz := uint64(maxPayloadBytes) + mqttPacketOverheadAllowance
	if sz > mqttMaxPacketSize {
		return uint32(mqttMaxPacketSize)
	}
	return uint32(sz)
}

// applyConnectLimits populates the MQTT v5 CONNECT properties advertising the
// self-imposed limits the broker must honour: Receive Maximum (in-flight QoS 1/2
// count) and, when a per-message payload ceiling is configured, Maximum Packet
// Size (derived via maxPacketSizeFor). Both are broker-enforced, together
// bounding worst-case pending memory to receive_maximum × max_payload_bytes.
//
// A zero receiveMaximum or a zero maxPayloadBytes leaves its respective property
// UNSET (0 is not a legal MQTT v5 value for either, and an unset Maximum Packet
// Size means "no limit" — the prior behaviour). Like applyConnectCredentials it
// takes the SDK *paho.Connect and therefore lives in the ACL.
func applyConnectLimits(cp *pahov5.Connect, receiveMaximum uint16, maxPayloadBytes uint32) {
	if receiveMaximum == 0 && maxPayloadBytes == 0 {
		return
	}
	if cp.Properties == nil {
		cp.Properties = &pahov5.ConnectProperties{}
	}
	if receiveMaximum > 0 {
		rm := receiveMaximum
		cp.Properties.ReceiveMaximum = &rm
	}
	if maxPayloadBytes > 0 {
		mps := maxPacketSizeFor(maxPayloadBytes)
		cp.Properties.MaximumPacketSize = &mps
	}
}
