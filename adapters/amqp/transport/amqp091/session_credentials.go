package amqp091

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
)

// ApplyCredentials rotates the AMQP 0-9-1 session's username/password
// and/or TLS material. New values are stored so the next (re)dial
// picks them up; if the session is currently connected, the existing
// connection is closed so the reconnect loop runs immediately.
//
// Design choice: AMQP 0-9-1 has no re-auth or TLS-re-handshake
// primitive. Rotation is therefore "close then redial" — the
// reconnect loop already runs on connection loss, so we reuse its
// machinery instead of inventing a parallel code path. This trades a
// brief disconnect for simplicity.
//
// Scope:
//   - PasswordCredential.Username/Password → liveCreds and opts.
//   - TLSMaterial → opts.TLS PEM fields. Both "swap existing cert"
//     and "first-time TLS enable" are supported; enabling TLS on a
//     previously non-TLS session rebuilds s.dial so the new dialer
//     actually performs the handshake.
func (s *Session) ApplyCredentials(ctx context.Context, set *domain.CredentialSet) error {
	if set == nil {
		return domain.ErrInvalidPayload.WithMessage("amqp091: nil credential set")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrUnavailable.WithMessage("amqp091: session already closed")
	}

	var credsChanged bool
	if set.Password != nil {
		credsChanged = s.liveCreds.Username != set.Password.Username ||
			s.liveCreds.Password != set.Password.Password
		if credsChanged {
			s.liveCreds = amqpCredentials{
				Username: set.Password.Username,
				Password: set.Password.Password,
			}
			s.opts.Username = set.Password.Username
			s.opts.Password = set.Password.Password
		}
	}

	tlsNewlyEnabled := set.TLS != nil && (s.opts.TLS == nil || !s.opts.TLS.Enable)
	tlsChanged := applyAMQPTLSMaterial(&s.opts.TLS, set.TLS)

	if tlsNewlyEnabled && tlsChanged {
		// Rebuild the dial closure so it actually goes through the TLS
		// code path. The existing dial was built when TLS was disabled
		// and would otherwise ignore the new material.
		s.dial = defaultDialFromOpts(s.opts)
	}

	if !credsChanged && !tlsChanged {
		s.mu.Unlock()
		return nil
	}
	conn := s.conn
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug,
			"amqp091: credentials rotated; forcing reconnect",
			"password_changed", credsChanged,
			"tls_changed", tlsChanged)
	}

	if conn == nil {
		// Not connected yet or already disconnected; nothing to tear
		// down. The next dial attempt consumes the rotated material.
		return nil
	}
	// Closing the connection triggers NotifyClose in reconnectLoop,
	// which calls doReconnect with the updated brokerURL()/TLS.
	if err := conn.Close(); err != nil {
		return MapError(err)
	}
	return nil
}

// applyAMQPTLSMaterial mirrors the paho helper: returns true when the
// session's TLS config was mutated, signalling the caller needs to
// force a reconnect. See the paho analogue for the full rationale.
func applyAMQPTLSMaterial(opts **TLSConfig, mat *domain.TLSMaterial) bool {
	if mat == nil {
		return false
	}
	newCA := ""
	if len(mat.CAPEMs) > 0 {
		newCA = joinAMQPPEMs(mat.CAPEMs)
	}
	if *opts == nil {
		if mat.CertPEM == "" && mat.KeyPEM == "" && newCA == "" && !mat.InsecureSkipVerify {
			return false
		}
		*opts = &TLSConfig{Enable: true}
	}
	cur := *opts
	if cur.CertPEM == mat.CertPEM &&
		cur.KeyPEM == mat.KeyPEM &&
		cur.CACertPEM == newCA &&
		cur.InsecureSkipVerify == mat.InsecureSkipVerify &&
		cur.Enable {
		return false
	}
	cur.CertPEM = mat.CertPEM
	cur.KeyPEM = mat.KeyPEM
	cur.CACertPEM = newCA
	cur.InsecureSkipVerify = mat.InsecureSkipVerify
	cur.Enable = true
	return true
}

func joinAMQPPEMs(pems []string) string {
	switch len(pems) {
	case 0:
		return ""
	case 1:
		return pems[0]
	}
	total := 0
	for _, p := range pems {
		total += len(p) + 1
	}
	buf := make([]byte, 0, total)
	for i, p := range pems {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}
