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
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt session is closed; Start is not allowed after Close")
	}
	if s.cm != nil {
		s.mu.Unlock()
		return nil
	}
	if s.starting {
		s.mu.Unlock()
		return nil
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
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("parse broker URLs")
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
				if err := s.reconcile(reconCtx, newPahoConn(cm), *plan); err != nil {
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
			return true
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

	if s.opts.TLS != nil && s.opts.TLS.Enable {
		tlsCfg, err := BuildTLSConfig(s.opts.TLS)
		if err != nil {
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
			return shared.ErrUnavailable.Wrap(err).WithMessage("build TLS config")
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
	s.cm = newPahoConn(cm)
	s.cmCancel = cmCancel
	// startCtx was already assigned before NewConnection (BUG-A fix).
	s.connected = true
	s.starting = false
	s.mu.Unlock()

	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(shared.MetricMQTTConnectLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connected",
			"client_id", s.opts.ClientID, "connect_latency", elapsed)
	}

	return nil
}
