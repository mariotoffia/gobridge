package amqp10

import (
	"errors"
	"testing"

	"github.com/Azure/go-amqp"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestApplyCredentials_BeforeStart_UpdatesLiveCreds verifies that
// rotating credentials on a not-yet-connected session stores them so
// the first dial picks them up via connect(). Uses an amqps:// (TLS)
// scheme so the c7-plain-plaintext runtime gate permits the PLAIN
// credential rotation (a plaintext username rotation is fail-closed —
// see TestApplyCredentials_PlainOverPlaintext_Refused).
func TestApplyCredentials_BeforeStart_UpdatesLiveCreds(t *testing.T) {
	s := NewSession(SessionOptions{
		Address:  "amqps://broker.example:5671/",
		Username: "u-old",
		Password: shared.NewSecret("p-old"),
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("u-new", "p-new"), nil))
	require.NoError(t, err)

	s.mu.Lock()
	got := s.liveCreds
	s.mu.Unlock()
	require.Equal(t, "u-new", got.Username)
	require.Equal(t, "p-new", got.Password)
	require.Equal(t, "u-new", s.opts.Username)
	require.Equal(t, "p-new", s.opts.Password.Reveal())
}

// TestApplyCredentials_NilSet_Rejected pins the boundary check.
func TestApplyCredentials_NilSet_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqp://broker.example:5672/"},
		connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(), nil)
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
}

// TestApplyCredentials_TLSMaterial_StashesOnOpts verifies TLS
// rotation updates s.opts.TLS in-place. The AMQP10 connect() path
// reads opts.TLS on each dial, so mutation is sufficient.
func TestApplyCredentials_TLSMaterial_StashesOnOpts(t *testing.T) {
	s := NewSession(SessionOptions{
		Address:  "amqp://broker.example:5672/",
		Username: "u",
		Password: shared.NewSecret("p"),
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	require.NoError(t, s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(nil, tlsMat("--- CERT ---", "--- KEY ---", []string{"--- CA ---"}, false))))
	s.mu.Lock()
	got := s.liveCreds
	optsTLS := s.opts.TLS
	s.mu.Unlock()
	require.Equal(t, "u", got.Username)
	require.NotNil(t, optsTLS)
	require.True(t, optsTLS.Enable)
	require.Equal(t, "--- CERT ---", optsTLS.CertPEM.Reveal())
	require.Equal(t, "--- KEY ---", optsTLS.KeyPEM.Reveal())
	require.Equal(t, "--- CA ---", optsTLS.CACertPEM.Reveal())
}

// TestApplyAMQP10TLSMaterial_ChangeDetection covers the pure helper.
func TestApplyAMQP10TLSMaterial_ChangeDetection(t *testing.T) {
	t.Run("nil material no-op", func(t *testing.T) {
		var tls *TLSConfig
		require.False(t, applyAMQP10TLSMaterial(&tls, nil))
		require.Nil(t, tls)
	})
	t.Run("first-time enable", func(t *testing.T) {
		var tls *TLSConfig
		require.True(t, applyAMQP10TLSMaterial(&tls, tlsMat("c", "k", nil, false)))
		require.NotNil(t, tls)
		require.True(t, tls.Enable)
	})
	t.Run("dedup no-op", func(t *testing.T) {
		tls := &TLSConfig{
			Enable: true, CertPEM: shared.NewSecret("c"), KeyPEM: shared.NewSecret("k"), CACertPEM: shared.NewSecret("ca"),
		}
		require.False(t, applyAMQP10TLSMaterial(&tls, tlsMat("c", "k", []string{"ca"}, false)))
	})
}

// TestApplyCredentials_ClosedSession_Rejected pins the closed-session
// path.
func TestApplyCredentials_ClosedSession_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqp://broker.example:5672/"},
		connectivity.SessionEphemeral, nil)
	require.NoError(t, s.Close(t.Context()))

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("u", "p"), nil))
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code)
}

func pwCred(u, p string) *connectivity.PasswordCredential {
	c := connectivity.NewPasswordCredential(u, p)
	return &c
}

func tlsMat(cert, key string, ca []string, insecure bool) *connectivity.TLSMaterial {
	m := connectivity.NewTLSMaterial(cert, key, ca, insecure)
	return &m
}

// TestSetAuthFailureCallback_ReconnectAuthFailure_ForcesReactiveReResolve
// verifies the HIGH-3 reactive-recovery wiring: when a live (re)connect dial is
// rejected with an authorization failure, the URI-bound callback injected by
// the CredentialRefresher is invoked exactly once with shared.ErrNotAuthorized,
// forcing an immediate re-resolve instead of stalling on revoked credentials.
func TestSetAuthFailureCallback_ReconnectAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqps://broker.example:5671/"},
		connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(err error) { reported <- err })

	// A hard rotation revoked the old credentials: the redial fails SASL and
	// MapError classifies amqp:unauthorized-access as ErrNotAuthorized.
	s.dial = mockDialFunc(nil, &amqp.Error{
		Condition:   "amqp:unauthorized-access",
		Description: "credentials revoked",
	})

	err := s.connect(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, shared.ErrNotAuthorized)

	select {
	case got := <-reported:
		require.ErrorIs(t, got, shared.ErrNotAuthorized,
			"reconnect auth failure must invoke the reactive-recovery callback")
	default:
		t.Fatal("expected reactive-recovery callback to be invoked on reconnect auth failure")
	}
}

// TestSetAuthFailureCallback_ReconnectNonAuthError_DoesNotReport verifies the
// callback is NOT invoked for a non-authorization reconnect failure (e.g. a
// plain connection-refused): only NOT_AUTHORIZED forces a reactive re-resolve.
func TestSetAuthFailureCallback_ReconnectNonAuthError_DoesNotReport(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqps://broker.example:5671/"},
		connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(err error) { reported <- err })

	s.dial = mockDialFunc(nil, errors.New("connection refused"))

	err := s.connect(t.Context())
	require.Error(t, err)
	require.NotErrorIs(t, err, shared.ErrNotAuthorized)

	select {
	case got := <-reported:
		t.Fatalf("non-auth reconnect error must not report: got %v", got)
	default:
	}
}
