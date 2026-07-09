package amqp10

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// ApplyCredentials rotates the AMQP 1.0 session's SASL credentials
// and/or TLS material. New values are stored so the next (re)dial
// picks them up; if the session is currently connected, the existing
// connection is closed so the reconnect loop runs immediately.
//
// SECURITY (c7-plain-plaintext): a credential rotation that would newly
// expose SASL PLAIN over a non-TLS scheme is REFUSED (fail-closed) via
// validatePlainOverPlaintext — the session keeps its last-good creds and
// no cleartext dial is issued. This closes the last credential-injection
// point the build-time gate does not cover.
//
// Design choice: go-amqp has no in-band re-auth primitive. The safest
// path is therefore "close then reconnect" — the existing reconnect
// loop already handles connection loss, so ApplyCredentials just
// signals it. Trading a brief disconnect for simplicity mirrors the
// AMQP 0-9-1 implementation.
//
// Scope:
//   - PasswordCredential → liveCreds + opts.
//   - TLSMaterial → opts.TLS. The AMQP10 connect() path re-reads
//     s.opts.TLS on every dial and calls BuildTLSConfig freshly.
//     applyAMQP10TLSMaterial swaps in a fresh *TLSConfig (rather than
//     mutating the current one in place) so an in-flight dial keeps
//     reading its immutable snapshot — see finding 2.
func (s *Session) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("amqp10: nil credential set")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("amqp10: session already closed")
	}

	var credsChanged bool
	if set.Password() != nil {
		credsChanged = s.liveCreds.Username != set.Password().Username() ||
			s.liveCreds.Password != set.Password().Password().Reveal()
		if credsChanged {
			// Snapshot the last-good credentials so a REFUSED rotation
			// leaves the session running on them — never a partial state
			// where Username is set but the dial was rejected.
			prevLiveCreds := s.liveCreds
			prevUsername := s.opts.Username
			prevPassword := s.opts.Password

			s.liveCreds = amqp10Credentials{
				Username: set.Password().Username(),
				Password: set.Password().Password().Reveal(),
			}
			s.opts.Username = set.Password().Username()
			s.opts.Password = set.Password().Password()

			// c7-plain-plaintext holds on the RUNTIME rotation path too,
			// not just the config/factory build boundary. go-amqp infers
			// SASL PLAIN from a non-empty Username, so a rotation that
			// injects a username into a plaintext amqp:// session (which
			// may have PASSED the build gate precisely because it had NO
			// username then) would newly ship the credentials in cleartext
			// on the next dial. Fail closed: refuse the rotation, restore
			// the last-good credentials, and DO NOT force a re-dial — the
			// session keeps running on its prior creds rather than leaking
			// the new ones. validatePlainOverPlaintext already passes for
			// TLS schemes and honors allow_insecure_plain, so an amqps://
			// or explicitly-opted-in operator is unaffected.
			if err := s.opts.validatePlainOverPlaintext(); err != nil {
				s.liveCreds = prevLiveCreds
				s.opts.Username = prevUsername
				s.opts.Password = prevPassword
				s.mu.Unlock()
				return err
			}
		}
	}

	tlsChanged := applyAMQP10TLSMaterial(&s.opts.TLS, set.TLS())

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

// applyAMQP10TLSMaterial applies rotated TLS material to *opts. To stay
// race-free with an in-flight connect() reading the current *TLSConfig,
// it NEVER mutates the pointed-to config in place: on a change it builds
// a fresh *TLSConfig (preserving any file-based fields) and swaps the
// pointer, so a concurrent dial keeps reading its immutable snapshot
// (finding 2). Returns true when a swap occurred.
func applyAMQP10TLSMaterial(opts **TLSConfig, mat *connectivity.TLSMaterial) bool {
	if mat == nil {
		return false
	}
	newCA := ""
	if len(mat.CAPEMs()) > 0 {
		newCA = joinAMQP10PEMs(mat.CAPEMs())
	}
	cur := *opts
	if cur == nil {
		if mat.CertPEM() == "" && mat.KeyPEM().Reveal() == "" && newCA == "" && !mat.InsecureSkipVerify() {
			return false
		}
	} else if cur.CertPEM.Reveal() == mat.CertPEM() &&
		cur.KeyPEM.Equal(mat.KeyPEM()) &&
		cur.CACertPEM.Reveal() == newCA &&
		cur.InsecureSkipVerify == mat.InsecureSkipVerify() &&
		cur.Enable {
		return false
	}
	// Build a NEW config rather than mutating cur in place. Copy the
	// existing struct first so file-based fields (CACertFile/CertFile/
	// KeyFile) survive a PEM rotation.
	next := &TLSConfig{}
	if cur != nil {
		*next = *cur
	}
	next.Enable = true
	next.CertPEM = shared.NewSecret(mat.CertPEM())
	next.KeyPEM = mat.KeyPEM()
	next.CACertPEM = shared.NewSecret(newCA)
	next.InsecureSkipVerify = mat.InsecureSkipVerify()
	*opts = next
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
