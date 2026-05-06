package paho

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// ApplyCredentials rotates the credential material used on subsequent
// MQTT CONNECT packets and forces a reconnect so the broker picks the
// new auth up promptly.
//
// Semantics:
//   - If the session is not yet started (s.cm == nil), the new
//     credentials are stored and Start() will pick them up from
//     s.liveCreds via ConnectPacketBuilder. No reconnect is attempted.
//   - If the credentials are identical to the current ones (dedup),
//     the call is a no-op — callers are expected to dedup too, but we
//     belt-and-brace here.
//   - Otherwise, liveCreds is updated and Disconnect() is invoked on
//     the ConnectionManager. autopaho's reconnect loop kicks in and
//     issues a fresh CONNECT, which pulls the new username/password
//     via ConnectPacketBuilder.
//
// TLS rotation is supported: when creds.TLS is non-nil and differs
// from s.opts.TLS's PEM material, the session's TLS options are
// updated and Session.Reload is invoked to rebuild the autopaho
// ConnectionManager with a fresh tls.Config. This is strictly more
// expensive than password rotation (the TCP connection is
// re-established and the TLS handshake is redone), so callers should
// rotate TLS only when the underlying material actually changes.
//
// Why reconnect rather than update in-place: autopaho does not expose
// an API to rewrite CONNECT properties on a live connection; MQTT
// itself has no "re-authenticate with a new CONNECT" mechanism without
// enhanced authentication packets, which Paho does not surface here.
// A brief disconnect/reconnect is the portable, correct option.
func (s *Session) ApplyCredentials(ctx context.Context, creds *domain.CredentialSet) error {
	if creds == nil {
		return shared.ErrInvalidPayload.WithMessage("nil credential set")
	}
	var user, pass string
	if creds.Password != nil {
		user = creds.Password.Username
		pass = creds.Password.Password
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("session is closed")
	}
	credsChanged := creds.Password != nil &&
		(s.liveCreds.Username != user || s.liveCreds.Password != pass)
	if credsChanged {
		s.liveCreds = mqttCredentials{Username: user, Password: pass}
	}
	cm := s.cm
	s.mu.Unlock()

	if credsChanged {
		// Also update s.opts so subsequent Start() calls (e.g. after
		// a restart by the Supervisor) see consistent values. This
		// mirrors the user's configuration without reaching into the
		// CM config.
		s.opts.Username = user
		s.opts.Password = pass
	}

	tlsChanged := applyTLSMaterial(&s.opts.TLS, creds.TLS)

	if tlsChanged && cm != nil {
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug,
				"mqtt: TLS material rotated; reloading session",
				"client_id", s.opts.ClientID)
		}
		// Reload rebuilds the CM with the updated TLS config and
		// incidentally replays the rotated password (if any) via
		// ConnectPacketBuilder on the new connect.
		return s.Reload(ctx)
	}

	if !credsChanged {
		return nil
	}

	if cm == nil {
		// Session not started yet; new creds will be picked up on first
		// connect via ConnectPacketBuilder. Nothing more to do.
		return nil
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug,
			"mqtt: applying rotated credentials; reconnecting",
			"client_id", s.opts.ClientID)
	}

	// Bounded disconnect timeout — we don't want to block the credential
	// watcher indefinitely if the broker is unresponsive. Reconnect is
	// driven by autopaho's own loop after Disconnect returns.
	disconnectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := cm.Disconnect(disconnectCtx); err != nil {
		return MapError(err)
	}
	return nil
}

// applyTLSMaterial compares incoming *domain.TLSMaterial PEM bytes
// against the session's current TLS config and updates the config
// in-place if any field differs. Returns true when an actual change
// was applied, so the caller can decide whether to Reload.
//
// Contract:
//   - A nil or all-zero TLSMaterial is a no-op (returns false). The
//     credential refresher may deliver CredentialSets that carry only
//     password material; those must not force a TLS rebuild.
//   - When the session had no TLS at all (*opts.TLS == nil) and the
//     incoming material is non-empty, a new TLSConfig{Enable: true}
//     is allocated and the PEM fields are set. This is the "TLS
//     enabled for the first time by rotation" edge case.
//   - Previously-set *File fields are left alone; the next BuildTLSConfig
//     will prefer PEM material over them. This keeps rollback simple:
//     clearing the PEM fields reverts to file-based material.
func applyTLSMaterial(opts **TLSConfig, mat *domain.TLSMaterial) bool {
	if mat == nil {
		return false
	}
	newCA := ""
	if len(mat.CAPEMs) > 0 {
		// Concatenate multiple CA PEMs into the single CACertPEM field.
		// AppendCertsFromPEM handles a bundle natively, so this is a
		// faithful translation even with more than one CA.
		newCA = joinPEMs(mat.CAPEMs)
	}
	if *opts == nil {
		// Rotation is enabling TLS on a session that had none. The
		// push side is the authoritative source now, so we trust the
		// incoming material to match broker expectations.
		if mat.CertPEM == "" && mat.KeyPEM == "" && newCA == "" && !mat.InsecureSkipVerify {
			return false
		}
		*opts = &TLSConfig{Enable: true}
	}
	cur := *opts
	if cur.CertPEM == mat.CertPEM &&
		cur.KeyPEM == mat.KeyPEM &&
		cur.CACertPEM == newCA &&
		cur.InsecureSkipVerify == mat.InsecureSkipVerify {
		return false
	}
	cur.CertPEM = mat.CertPEM
	cur.KeyPEM = mat.KeyPEM
	cur.CACertPEM = newCA
	cur.InsecureSkipVerify = mat.InsecureSkipVerify
	cur.Enable = true
	return true
}

func joinPEMs(pems []string) string {
	// PEM blocks are newline-delimited; concatenating with a separator
	// newline is safe for AppendCertsFromPEM.
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
