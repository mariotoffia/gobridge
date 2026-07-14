package paho

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
// an authorization failure. Safe to call on every mapped connect error: the
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

// handleConnectError is the body of autopaho's OnConnectError callback,
// extracted so the HIGH-3 reactive-recovery chokepoint is unit-testable without
// a live broker. autopaho surfaces a rejected CONNECT here; after a hard
// rotation revoked the old credentials the broker answers CONNACK 0x86/0x87
// (bad user/pass, not authorized), which MapError classifies as
// shared.ErrNotAuthorized. Reporting it forces an immediate re-resolve instead
// of reconnecting forever with the revoked material; reportAuthFailure filters
// non-auth connect errors.
//
// ponytail: MQTT authenticates only at CONNECT, so a credential revocation
// manifests as a connect error — this is the single funnel; per-topic
// SUBACK/PUBACK not-authorized codes are routing authorization, not credential
// rotation.
func (s *Session) handleConnectError(err error) {
	s.mu.Lock()
	s.connected = false
	s.subscriptionsSatisfied = false
	s.mu.Unlock()
	mapped := mapConnectError(err)
	s.reportAuthFailure(mapped)
	s.pushEvent(ports.SessionReconnecting, mapped)
}

// mapConnectError classifies an autopaho connect failure. A server CONNACK deny
// carries the auth verdict in its reason code (0x86 bad user/pass, 0x87 not
// authorized) — the generic MapError never sees it (it only inspects net errors
// / message substrings), so a credential revocation would misclassify as
// ErrUnavailable and NEVER trigger the reactive re-resolve. connackReasonCode
// (the ACL SDK seam) extracts the plain reason byte; route it through
// MapDisconnectReasonCode (which maps 0x86/0x87 → shared.ErrNotAuthorized);
// everything else defers to MapError.
func mapConnectError(err error) *shared.BridgeError {
	if code, ok := connackReasonCode(err); ok {
		if be := MapDisconnectReasonCode(code); be != nil {
			return be
		}
	}
	return MapError(err)
}

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
//   - Otherwise, liveCreds is updated and Session.Reload rebuilds the
//     ConnectionManager, issuing a fresh CONNECT that pulls the new
//     username/password via ConnectPacketBuilder. Reload — NOT a bare
//     cm.Disconnect — is required: in paho.golang v0.23.0 Disconnect
//     cancels the CM root context terminally (autopaho never reconnects
//     and skips OnConnectionDown), which would silently kill the session
//     while Health() still reported Full.
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
// A Reload (disconnect + rebuild + reconnect) is the portable, correct
// option.
func (s *Session) ApplyCredentials(ctx context.Context, creds *connectivity.CredentialSet) error {
	if creds == nil {
		return shared.ErrInvalidPayload.WithMessage("nil credential set")
	}
	var user, pass string
	if creds.Password() != nil {
		user = creds.Password().Username()
		pass = creds.Password().Password().Reveal()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("session is closed")
	}
	credsChanged := creds.Password() != nil &&
		(s.liveCreds.Username != user || s.liveCreds.Password != pass)
	if credsChanged {
		// HIGH-4 (runtime rotation is the hole the static/deferred gates miss):
		// a rotation that would put username/password on a PLAINTEXT transport
		// is gated identically to static config. Validate the CANDIDATE creds
		// BEFORE mutating liveCreds/opts so a rejected rotation leaves the
		// session's credential material — and its TLS material — UNCHANGED.
		// Only reached when credsChanged, so a dedup no-op never newly rejects.
		candidateHasCreds := user != "" || pass != ""
		if plaintextCredentialViolation(candidateHasCreds, s.opts.AllowPlaintextCredentials, s.opts.BrokerURLs) {
			s.mu.Unlock()
			return errPlaintextCredentials()
		}
		s.liveCreds = mqttCredentials{Username: user, Password: pass}
		// Also update s.opts so subsequent Start() calls (e.g. after
		// a restart by the Supervisor) see consistent values. Mutated
		// under s.mu because a supervisor-restarted Start reads these
		// fields concurrently.
		s.opts.Username = user
		s.opts.Password = shared.NewSecret(pass)
	}

	// TLS rotation is copy-on-write: applyTLSMaterial allocates a NEW
	// *TLSConfig and swaps the pointer under s.mu, so a concurrent
	// Start that snapshotted the previous pointer keeps reading
	// immutable material (no torn tls.Config), and the next
	// Start/Reload picks up the rotated pointer atomically.
	tlsChanged := applyTLSMaterial(&s.opts.TLS, creds.TLS())
	cm := s.cm
	s.mu.Unlock()

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
		//
		// ponytail: cm==nil also matches the brief window where a
		// Reload-failure has torn the CM down and the supervisor re-Start
		// has not yet re-installed one (F-1). Returning nil is still
		// correct there — credsChanged already updated s.liveCreds and
		// s.opts above (under s.mu), and the imminent re-Start's dial
		// re-seeds liveCreds from s.opts, so the rotated credentials are
		// applied on the next CONNECT. The F-1 events-close restart bounds
		// this window (the session no longer stays dead), so this is a
		// transient "picked up on next connect", not a false success
		// against a permanently dead session.
		return nil
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug,
			"mqtt: applying rotated credentials; reloading session",
			"client_id", s.opts.ClientID)
	}

	// Reload rebuilds the ConnectionManager and issues a fresh CONNECT
	// that pulls the rotated username/password via ConnectPacketBuilder.
	//
	// Why Reload and NOT cm.Disconnect: in paho.golang v0.23.0
	// ConnectionManager.Disconnect cancels the CM root context, which is
	// TERMINAL — autopaho's mainLoop breaks and never reconnects, and it
	// skips OnConnectionDown so s.connected would stay true. The session
	// would be silently dead while Health() still reported Full. Reload
	// tears the old CM down (Disconnect + cmCancel) AND rebuilds a fresh
	// one, matching the TLS-rotation path above.
	return s.Reload(ctx)
}

// applyTLSMaterial compares incoming *connectivity.TLSMaterial PEM bytes
// against the session's current TLS config and, if any field differs,
// swaps in a NEW *TLSConfig (copy-on-write — the existing struct is
// never mutated). Returns true when a change was applied, so the
// caller can decide whether to Reload. The caller must hold s.mu while
// invoking this so the pointer swap is atomic with respect to Start's
// snapshot of s.opts.TLS.
//
// Contract:
//   - A nil or all-zero TLSMaterial is a no-op (returns false). The
//     credential refresher may deliver CredentialSets that carry only
//     password material; those must not force a TLS rebuild.
//   - When the session had no TLS at all (*opts.TLS == nil) and the
//     incoming material is non-empty, a new TLSConfig{Enable: true}
//     is created and the PEM fields are set. This is the "TLS
//     enabled for the first time by rotation" edge case.
//   - Previously-set *File fields are carried over unchanged; the next
//     BuildTLSConfig will prefer PEM material over them. This keeps
//     rollback simple: clearing the PEM fields reverts to file-based
//     material.
func applyTLSMaterial(opts **TLSConfig, mat *connectivity.TLSMaterial) bool {
	if mat == nil {
		return false
	}
	newCA := ""
	if len(mat.CAPEMs()) > 0 {
		// Concatenate multiple CA PEMs into the single CACertPEM field.
		// AppendCertsFromPEM handles a bundle natively, so this is a
		// faithful translation even with more than one CA.
		newCA = joinPEMs(mat.CAPEMs())
	}
	if *opts == nil {
		// Rotation is enabling TLS on a session that had none. The
		// push side is the authoritative source now, so we trust the
		// incoming material to match broker expectations.
		if mat.CertPEM() == "" && mat.KeyPEM().IsZero() && newCA == "" && !mat.InsecureSkipVerify() {
			return false
		}
		*opts = &TLSConfig{Enable: true}
	}
	cur := *opts
	if cur.CertPEM.Reveal() == mat.CertPEM() &&
		cur.KeyPEM.Equal(mat.KeyPEM()) &&
		cur.CACertPEM.Reveal() == newCA &&
		cur.InsecureSkipVerify == mat.InsecureSkipVerify() {
		return false
	}
	next := *cur // copy-on-write: file paths etc. carry over
	next.CertPEM = shared.NewSecret(mat.CertPEM())
	next.KeyPEM = mat.KeyPEM()
	next.CACertPEM = shared.NewSecret(newCA)
	next.InsecureSkipVerify = mat.InsecureSkipVerify()
	next.Enable = true
	*opts = &next
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
