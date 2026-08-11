package paho

import (
	"context"
	"math/rand/v2"
	"net/url"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

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

	// defense-in-depth: Factory.NewSession validates the plaintext-
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

	// ponytail: — this session relies on autopaho's DEFAULT
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
	// ADR-level alternative and is out of scope here (deferred see
	// scenario-01 docs).
	cfg := autopaho.ClientConfig{
		ServerUrls: serverURLs,
		KeepAlive:  s.opts.KeepAlive,

		// OnConnectionUp fires on every (re)connect. Per the
		// runtime session manager is the SINGLE owner of reconnect
		// reconciliation and failure propagation: it observes the
		// SessionConnected event emitted here and drives Reconcile, whose
		// outcome is authoritative (a rejected re-subscribe propagates out
		// of Manager.Run so the bridge can restart/alarm —).
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

	// Reconnect pacing: a JITTERED EXPONENTIAL base delay derived
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
		if err := applyConnectLimits(cp, rm, maxPayload); err != nil {
			return nil, err
		}
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
			// uses, but must never pass unnoticed in production.
			s.logger.Warn("mqtt: TLS certificate verification DISABLED "+
				"(insecure_skip_verify) — the broker's identity is NOT "+
				"validated; use only on a trusted transport",
				"client_id", s.opts.ClientID)
		}
		cfg.TlsCfg = tlsCfg
	}
	// Own the final connection composition seam so every decrypted inbound byte
	// crosses the bounded raw MQTT guard before Paho's packets.ReadPacket can
	// materialize topic/property/payload representations. AttemptConnection is
	// called for initial connect and every autopaho reconnect generation.
	cfg.AttemptConnection = s.attemptGuardedConnection

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
		// Bounded teardown so a discard-path disconnect cannot block forever if the
		// SDK ignores cancellation of its already-cancelled root.
		disCtx, disCancel := s.discardDisconnectContext()
		_ = cm.Disconnect(disCtx)
		disCancel()
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

// applyConnectLimits populates the MQTT v5 CONNECT properties advertising the
// self-imposed limits the broker must honour: Receive Maximum (in-flight QoS 1/2
// count) and, when a per-message payload ceiling is configured, Maximum Packet
// Size (derived via wirePacketSizeFor). Together they make the validated ingress
// byte model broker-enforced.
//
// A zero receiveMaximum or a zero maxPayloadBytes leaves its respective property
// UNSET (0 is not a legal MQTT v5 value for either, and an unset Maximum Packet
// Size means "no limit" — the prior behaviour). Like applyConnectCredentials it
// takes the SDK *paho.Connect and therefore lives in the ACL.
func applyConnectLimits(cp *pahov5.Connect, receiveMaximum uint16, maxPayloadBytes uint32) error {
	if receiveMaximum == 0 && maxPayloadBytes == 0 {
		return nil
	}
	if cp.Properties == nil {
		cp.Properties = &pahov5.ConnectProperties{}
	}
	if receiveMaximum > 0 {
		rm := receiveMaximum
		cp.Properties.ReceiveMaximum = &rm
	}
	if maxPayloadBytes > 0 {
		mps, err := wirePacketSizeFor(maxPayloadBytes)
		if err != nil {
			return err
		}
		cp.Properties.MaximumPacketSize = &mps
	}
	return nil
}
