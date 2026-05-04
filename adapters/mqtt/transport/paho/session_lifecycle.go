package paho

import (
	"context"
	"net/url"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// Start connects to the MQTT broker and emits a SessionConnected event
// once the initial connection is established. Calling Start on an
// already-started session is a no-op (idempotent).
//
// A Session is single-use: once Close has been called, Start returns
// ErrUnavailable and does NOT attempt a new connection. This prevents
// a "zombie" state where a freshly attached cm coexists with an
// already-closed events channel.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrUnavailable.WithMessage("mqtt session is closed; Start is not allowed after Close")
	}
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
	connectStart := s.clock().Now()

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

	// Seed liveCreds from the initial options so the first CONNECT picks
	// them up via the ConnectPacketBuilder. ApplyCredentials can later
	// mutate this record without touching the cfg struct.
	s.mu.Lock()
	s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password}
	s.mu.Unlock()

	switch s.mode {
	case domain.SessionEphemeral:
		cfg.CleanStartOnInitialConnection = true
		cfg.SessionExpiryInterval = 0
		ephemeralCleanStart = true
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

	// BUG-A fix: assign s.startCtx BEFORE NewConnection so that the
	// OnConnectionUp callback (which may fire from an autopaho goroutine
	// before AwaitConnection returns) always observes a non-nil parent
	// context when deriving the reconcile timeout. Otherwise, if a prior
	// Reconcile call had stashed s.plan, the callback would panic on
	// context.WithTimeout(nil, ...).
	s.mu.Lock()
	s.startCtx = cmCtx
	s.mu.Unlock()

	cm, err := autopaho.NewConnection(cmCtx, cfg)
	if err != nil {
		cmCancel()
		s.mu.Lock()
		s.startCtx = nil
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
		s.startCtx = nil
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
	// startCtx was already assigned before NewConnection (BUG-A fix).
	s.connected = true
	s.starting = false
	s.mu.Unlock()

	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(domain.MetricMQTTConnectLatency, elapsed,
		domain.Tag{Key: domain.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connected",
			"client_id", s.opts.ClientID, "connect_latency", elapsed)
	}

	return nil
}

// Reload tears down the current ConnectionManager and re-runs Start
// with the session's current options. It is intended for rotation
// scenarios that cannot be applied to an existing CM — most notably
// TLS material, which requires a new tls.Config baked into a fresh
// autopaho configuration.
//
// Semantics:
//   - Session state is preserved: the plan, router, and event channel
//     survive the teardown. Subscribers stay subscribed (activeSubs
//     is re-materialised by reconcile when the new CM connects).
//   - liveCreds is preserved, so username/password rotation that has
//     already been applied via ApplyCredentials is not lost.
//   - Close-after-Reload semantics match Close: once closed, Reload
//     returns ErrUnavailable.
//
// Why not named Restart: "Restart" would imply a user-initiated
// restart of the session's configured lifecycle, including re-parsing
// options. Reload is narrower — the options are not re-read, only the
// TLS handshake + transport layer are rebuilt.
func (s *Session) Reload(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrUnavailable.WithMessage("mqtt session is closed; Reload is not allowed after Close")
	}
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.connected = false
	s.mu.Unlock()

	if cm != nil {
		_ = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	return s.Start(ctx)
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
