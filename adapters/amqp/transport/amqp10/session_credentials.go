package amqp10

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/logging"
)

// ApplyCredentials rotates the AMQP 1.0 session's SASL credentials
// and/or TLS material. New values are stored so the next (re)dial
// picks them up; if the session is currently connected, the existing
// connection is closed so the reconnect loop runs immediately.
//
// Design choice: go-amqp has no in-band re-auth primitive. The safest
// path is therefore "close then reconnect" — the existing reconnect
// loop already handles connection loss, so ApplyCredentials just
// signals it. Trading a brief disconnect for simplicity mirrors the
// AMQP 0-9-1 implementation.
//
// Scope:
//   - PasswordCredential → liveCreds + opts.
//   - TLSMaterial → opts.TLS PEM fields. The AMQP10 connect() path
//     re-reads s.opts.TLS on every dial and calls BuildTLSConfig
//     freshly, so mutating fields in place suffices for both
//     cert/CA swap and first-time TLS enable.
func (s *Session) ApplyCredentials(ctx context.Context, set *domain.CredentialSet) error {
	if set == nil {
		return domain.ErrInvalidPayload.WithMessage("amqp10: nil credential set")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.ErrUnavailable.WithMessage("amqp10: session already closed")
	}

	var credsChanged bool
	if set.Password != nil {
		credsChanged = s.liveCreds.Username != set.Password.Username ||
			s.liveCreds.Password != set.Password.Password
		if credsChanged {
			s.liveCreds = amqp10Credentials{
				Username: set.Password.Username,
				Password: set.Password.Password,
			}
			s.opts.Username = set.Password.Username
			s.opts.Password = set.Password.Password
		}
	}

	tlsChanged := applyAMQP10TLSMaterial(&s.opts.TLS, set.TLS)

	if !credsChanged && !tlsChanged {
		s.mu.Unlock()
		return nil
	}
	conn := s.conn
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug,
			"amqp10: credentials rotated; forcing reconnect",
			"password_changed", credsChanged,
			"tls_changed", tlsChanged)
	}

	if conn == nil {
		// Not connected yet or already disconnected; nothing to tear
		// down. The next dial consumes the rotated material.
		return nil
	}
	// Closing the connection triggers the monitor loop to reconnect
	// with the updated credentials/TLS.
	if err := conn.Close(); err != nil {
		return MapError(err)
	}
	return nil
}

// applyAMQP10TLSMaterial mirrors the paho/amqp091 helpers. See the
// paho analogue for the full rationale.
func applyAMQP10TLSMaterial(opts **TLSConfig, mat *domain.TLSMaterial) bool {
	if mat == nil {
		return false
	}
	newCA := ""
	if len(mat.CAPEMs) > 0 {
		newCA = joinAMQP10PEMs(mat.CAPEMs)
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

func joinAMQP10PEMs(pems []string) string {
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
