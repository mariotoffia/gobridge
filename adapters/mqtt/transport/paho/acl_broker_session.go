package paho

import (
	"context"
	"errors"
	"net/url"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// endBrokerSession ends the broker session of this session's client ID
// (ADR 0024). It opens one short connection as that client ID with Clean Start
// set and a Session Expiry Interval of 0, then disconnects normally: Clean
// Start makes the broker discard the session it kept, and expiry 0 makes it
// discard the new one when the connection closes (MQTT 5 §3.1.2.4, §3.1.2.11.2).
// On MQTT 3.1.1 the translator sends the same CONNECT as CleanSession=1, which
// does both. A DISCONNECT cannot do this on its own: MQTT 3.1.1 has no session
// expiry to send. The caller must have closed the session's own connection
// first: a clean-start connection while the session is connected would take
// over its client ID.
//
// The connection reaches the broker the way every session connection does:
// the session's broker URLs in order, its credentials, its TLS settings, the
// broker proxy, and attemptGuardedConnection's MQTT 3.1.1 translator and
// ingress guard. Its CONNECT announces the session's Receive Maximum and
// Maximum Packet Size. It carries no Will, subscribes to nothing and publishes
// nothing. As for the session's first connection, connect_timeout bounds the
// whole attempt and reconnect_timeout each broker URL; ctx can only shorten
// them.
func (s *Session) endBrokerSession(ctx context.Context) error {
	if s.protocolErr != nil {
		return s.protocolErr
	}
	s.mu.Lock()
	tlsOpts := s.opts.TLS
	user, pass := s.opts.Username, s.opts.Password.Reveal()
	plaintextErr := s.opts.validatePlaintextCredentials()
	s.mu.Unlock()

	clientID := s.opts.ClientID
	if clientID == "" {
		return shared.ErrInvalidConfig.WithMessage("mqtt: ending a broker session needs the session's client ID")
	}
	if plaintextErr != nil {
		return plaintextErr
	}
	serverURLs, err := parseURLs(s.opts.BrokerURLs)
	if err != nil {
		return shared.ErrInvalidConfig.Wrap(err).WithMessage("parse broker URLs")
	}
	connect, err := s.brokerSessionEndConnect(clientID, user, pass)
	if err != nil {
		return MapError(err)
	}
	cfg := autopaho.ClientConfig{ConnectTimeout: s.opts.ReconnectTimeout}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = DefaultReconnectAttemptTimeout
	}
	if tlsOpts != nil && tlsOpts.Enable {
		tlsCfg, err := BuildTLSConfig(tlsOpts)
		if err != nil {
			return shared.ErrUnavailable.Wrap(err).WithMessage("build TLS config")
		}
		cfg.TlsCfg = tlsCfg
	}

	ctx, cancel := context.WithTimeout(ctx, s.connectTimeout())
	defer cancel()
	var errs []error
	for _, serverURL := range serverURLs {
		err := s.endBrokerSessionAt(ctx, cfg, serverURL, connect)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// brokerSessionEndConnect builds the CONNECT that ends the broker session:
// clean start, Session Expiry Interval 0, no Will, and the session's
// credentials and announced limits.
func (s *Session) brokerSessionEndConnect(clientID, user, pass string) (*pahov5.Connect, error) {
	noExpiry := uint32(0)
	connect := &pahov5.Connect{
		ClientID:   clientID,
		CleanStart: true,
		KeepAlive:  s.opts.KeepAlive,
		Properties: &pahov5.ConnectProperties{SessionExpiryInterval: &noExpiry},
	}
	if err := applyConnectLimits(connect, s.opts.ReceiveMaximum, s.opts.MaxPayloadBytes); err != nil {
		return nil, err
	}
	applyConnectCredentials(connect, user, pass)
	return connect, nil
}

// endBrokerSessionAt is endBrokerSession against one broker URL, bounded by
// cfg.ConnectTimeout as autopaho bounds each connection attempt.
func (s *Session) endBrokerSessionAt(ctx context.Context, cfg autopaho.ClientConfig, serverURL *url.URL,
	connect *pahov5.Connect,
) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	conn, err := s.attemptGuardedConnection(ctx, cfg, serverURL)
	if err != nil {
		return MapError(err)
	}
	client := pahov5.NewClient(pahov5.ClientConfig{
		ClientID:      connect.ClientID,
		Conn:          conn,
		PacketTimeout: s.packetTimeout(),
	})
	connack, err := client.Connect(ctx, connect)
	if err != nil {
		if connack != nil {
			// The same typed refusal autopaho reports, so MapError classifies
			// the CONNACK reason code (0x86 and 0x87 are ErrNotAuthorized).
			err = autopaho.NewConnackError(err, connack)
		}
		return MapError(err)
	}
	// The accepted clean-start CONNECT already ended the broker state: Clean
	// Start dropped the old session and expiry 0 (CleanSession=1) ends the new
	// one when the connection closes, so a failed DISCONNECT changes nothing.
	_ = client.Disconnect(&pahov5.Disconnect{ReasonCode: packets.DisconnectNormalDisconnection})
	return nil
}
