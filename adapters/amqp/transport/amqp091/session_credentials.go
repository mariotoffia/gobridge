package amqp091

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// SetAuthFailureCallback wires the reactive-recovery hook (HIGH-3), satisfying
// the bridge.AuthFailureReporter capability (matched structurally by the
// CredentialRefresher in another module). A nil callback clears it.
func (s *Session) SetAuthFailureCallback(cb func(error)) {
	if cb == nil {
		s.authFailureCB.Store(nil)
		return
	}
	s.authFailureCB.Store(&cb)
}

// reportAuthFailure invokes the injected reactive-recovery callback iff err is
// an authorization failure. Safe to call on every mapped reconnect error: the
// callback delegates to CredentialRefresher.NotifyAuthFailure, which is
// auth-gated and per-URI rate-limited.
func (s *Session) reportAuthFailure(err error) {
	if err == nil || !errors.Is(err, shared.ErrNotAuthorized) {
		return
	}
	if cb := s.authFailureCB.Load(); cb != nil {
		(*cb)(err)
	}
}

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
func (s *Session) ApplyCredentials(ctx context.Context, set *connectivity.CredentialSet) error {
	if set == nil {
		return shared.ErrInvalidPayload.WithMessage("amqp091: nil credential set")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("amqp091: session already closed")
	}

	var credsChanged bool
	if set.Password() != nil {
		credsChanged = s.liveCreds.Username != set.Password().Username() ||
			s.liveCreds.Password != set.Password().Password().Reveal()
		if credsChanged {
			s.liveCreds = amqpCredentials{
				Username: set.Password().Username(),
				Password: set.Password().Password().Reveal(),
			}
			s.opts.Username = set.Password().Username()
			s.opts.Password = set.Password().Password()
		}
	}

	tlsChanged := applyAMQPTLSMaterial(&s.opts.TLS, set.TLS())

	if tlsChanged {
		// applyAMQPTLSMaterial swapped in a fresh *TLSConfig (copy-on-write).
		// Rebuild the dial closure so it captures the new pointer: this
		// covers both first-time TLS enablement (the previous dialer skipped
		// the handshake) and an in-place cert/key/CA rotation (the previous
		// dialer still holds the old snapshot). The old closure keeps its
		// consistent snapshot until any dial in flight completes.
		s.dial = s.buildDial(s.opts)
	}

	if !credsChanged && !tlsChanged {
		s.mu.Unlock()
		return nil
	}
	// Force-detach the stale connection UNDER THE LOCK before releasing it
	// (review #4). Marking the session disconnected and dropping s.conn here —
	// instead of leaving it installed and waiting for the async Close to
	// eventually fire NotifyClose — guarantees a sender that grabs the seam
	// (connectionIfReady) after this point cannot publish on the old connection
	// with the OLD credentials. If we left s.conn installed and relied on the
	// close completing, a Close that wedges on a half-dead broker would strand
	// senders on the stale connection (and stale creds) indefinitely, and
	// reconnect would never start. activeSubs/blocked are cleared to match the
	// NotifyClose-driven disconnect path.
	conn := s.conn
	if conn != nil {
		s.connected = false
		s.conn = nil
		s.activeSubs = make(map[string]bool)
		s.blocked = false
		s.blockedReason = ""
	}
	s.mu.Unlock()

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug,
			"amqp091: credentials rotated; forcing reconnect",
			"password_changed", credsChanged,
			"tls_changed", tlsChanged)
	}
	// Rotation over a broker_url that still embeds userinfo works (the
	// rotated material overrides it on every dial — see
	// injectCredentials) but the embedded, presumably stale secret keeps
	// sitting in the config. Tell the operator instead of staying silent.
	if credsChanged && brokerURLEmbedsUserinfo(s.opts.BrokerURL) {
		s.warnEmbeddedBrokerURLCredentials("the rotated")
	}

	if conn == nil {
		// Not connected yet or already disconnected; nothing to tear
		// down. The next dial attempt consumes the rotated material.
		return nil
	}

	// The connection is already detached from session state; announce the
	// disconnect and EXPLICITLY wake the reconnect loop to redial with the
	// rotated material, rather than relying on the async Close below to fire
	// NotifyClose (which never happens if that Close wedges — review #4). The
	// send is coalesced (buffered cap 1): concurrent rotations collapse to a
	// single scheduled reconnect.
	s.pushEvent(ports.SessionDisconnected, nil)
	select {
	case s.forceReconnect <- struct{}{}:
	default:
	}

	// Close the now-detached connection to unwedge any in-flight SDK call and
	// free the socket. Race the close against ctx: conn.Close() blocks in the
	// SDK until the broker answers connection.close-ok (bounded only by the
	// heartbeat read deadline), so a caller that cancels or deadlines
	// mid-rotation must not be pinned to a half-dead broker. The detached
	// goroutine still completes the underlying close, and reconnect has already
	// been driven above, so progress does not depend on who wins this race.
	// cdone is buffered so the detached goroutine never leaks on ctx win.
	cdone := make(chan error, 1)
	go func() { cdone <- conn.Close() }()
	select {
	case err := <-cdone:
		if err != nil {
			return MapError(err)
		}
		return nil
	case <-ctx.Done():
		return MapError(ctx.Err())
	}
}

// applyAMQPTLSMaterial mirrors the paho helper: returns true when the
// session's TLS config was replaced, signalling the caller needs to
// force a reconnect. See the paho analogue for the full rationale.
//
// Copy-on-write: the new material is written into a FRESH *TLSConfig and
// the pointer is swapped, never mutating the struct a live dial closure
// may be reading. defaultDialFromOpts captures opts by value, but
// opts.TLS is a pointer — an in-flight reconnect dial (session.go
// dialWithTimeout) reads its cert/key/CA fields without the session lock.
// Mutating them in place while ApplyCredentials rotates them is a data
// race that can hand the SDK a torn cert/key pair. Swapping the pointer
// keeps the old snapshot internally consistent for any dial still using
// it and makes the new material visible atomically.
func applyAMQPTLSMaterial(opts **TLSConfig, mat *connectivity.TLSMaterial) bool {
	if mat == nil {
		return false
	}
	newCA := ""
	if len(mat.CAPEMs()) > 0 {
		newCA = joinAMQPPEMs(mat.CAPEMs())
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
	next := &TLSConfig{}
	if cur != nil {
		*next = *cur
	}
	next.CertPEM = shared.NewSecret(mat.CertPEM())
	next.KeyPEM = mat.KeyPEM()
	next.CACertPEM = shared.NewSecret(newCA)
	next.InsecureSkipVerify = mat.InsecureSkipVerify()
	next.Enable = true
	*opts = next
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
