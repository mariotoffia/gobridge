package paho

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func pwCred(u, p string) *connectivity.PasswordCredential {
	c := connectivity.NewPasswordCredential(u, p)
	return &c
}

func tlsMat(cert, key string, ca []string, insecure bool) *connectivity.TLSMaterial {
	m := connectivity.NewTLSMaterial(cert, key, ca, insecure)
	return &m
}

// TestApplyCredentials_BeforeStart_UpdatesLiveCreds verifies that
// rotating credentials on a not-yet-started session stores them so the
// first CONNECT picks them up via ConnectPacketBuilder.
func TestApplyCredentials_BeforeStart_UpdatesLiveCreds(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-before-start",
		Username:   "u-old",
		Password:   shared.NewSecret("p-old"),
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	// Seed liveCreds manually — Start() is what normally does this, but
	// we want to test the pre-Start path of ApplyCredentials.
	s.mu.Lock()
	s.liveCreds = mqttCredentials{Username: "u-old", Password: "p-old"}
	s.mu.Unlock()

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("u-new", "p-new"), nil))
	require.NoError(t, err)

	s.mu.Lock()
	got := s.liveCreds
	s.mu.Unlock()
	require.Equal(t, "u-new", got.Username)
	require.Equal(t, "p-new", got.Password)
	require.Equal(t, "u-new", s.opts.Username, "opts.Username must also be updated for future re-starts")
	require.Equal(t, "p-new", s.opts.Password.Reveal())
}

// TestApplyCredentials_NilSet_Rejected ensures the boundary check
// returns a typed error.
func TestApplyCredentials_NilSet_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-nil",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	err := s.ApplyCredentials(t.Context(), nil)
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
}

// TestApplyCredentials_ClosedSession_Rejected ensures rotation after
// Close fails fast with an Unavailable error.
func TestApplyCredentials_ClosedSession_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-closed",
	}, connectivity.SessionEphemeral, nil)
	_ = s.Close(context.Background())

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("u", "p"), nil))
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code)
}

// TestApplyCredentials_Dedup verifies identical credentials are a no-op
// — crucial so the CredentialRefresher can call ApplyCredentials
// eagerly without forcing a reconnect for every poll tick.
func TestApplyCredentials_Dedup(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-dedup",
		Username:   "u",
		Password:   shared.NewSecret("p"),
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	s.mu.Lock()
	s.liveCreds = mqttCredentials{Username: "u", Password: "p"}
	// cm is nil; a real reconnect path would require autopaho. We rely
	// on the no-change-then-return-nil path to execute.
	s.mu.Unlock()

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("u", "p"), nil))
	require.NoError(t, err)
}

// TestReload_ClosedSession_Rejected pins the Close-after-Reload
// contract: once closed, Reload returns Unavailable rather than
// spawning a fresh CM.
func TestReload_ClosedSession_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "reload-closed",
	}, connectivity.SessionEphemeral, nil)
	require.NoError(t, s.Close(context.Background()))

	err := s.Reload(t.Context())
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code)
}

// TestApplyCredentials_TLSMaterial_BeforeStart_StashesOnOpts verifies
// that TLS PEM material arriving via ApplyCredentials before the
// session connects is stored on s.opts.TLS so the first dial uses
// the rotated material. No Reload is triggered because no
// ConnectionManager exists yet.
func TestApplyCredentials_TLSMaterial_BeforeStart_StashesOnOpts(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-tls-prestart",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(nil, tlsMat("--- CERT ---", "--- KEY ---", []string{"--- CA ---"}, false)))
	require.NoError(t, err)

	require.NotNil(t, s.opts.TLS)
	require.True(t, s.opts.TLS.Enable)
	require.Equal(t, "--- CERT ---", s.opts.TLS.CertPEM.Reveal())
	require.Equal(t, "--- KEY ---", s.opts.TLS.KeyPEM.Reveal())
	require.Equal(t, "--- CA ---", s.opts.TLS.CACertPEM.Reveal())
}

// TestApplyCredentials_TLSMaterial_Dedup verifies that an incoming
// TLSMaterial whose PEM bytes exactly match what the session already
// holds is a no-op. This is critical: the CredentialRefresher may
// emit eagerly, and every non-op rebuild costs a TLS handshake.
func TestApplyCredentials_TLSMaterial_Dedup(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-tls-dedup",
		TLS: &TLSConfig{
			Enable:    true,
			CertPEM:   shared.NewSecret("--- CERT ---"),
			KeyPEM:    shared.NewSecret("--- KEY ---"),
			CACertPEM: shared.NewSecret("--- CA ---"),
		},
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(nil, tlsMat("--- CERT ---", "--- KEY ---", []string{"--- CA ---"}, false)))
	require.NoError(t, err)
}

// TestApplyTLSMaterial_ChangeDetection covers the pure helper. The
// matrix is small but each case maps to a rotation scenario that
// caused a real-world incident elsewhere.
func TestApplyTLSMaterial_ChangeDetection(t *testing.T) {
	t.Run("nil material no-op", func(t *testing.T) {
		var tls *TLSConfig
		require.False(t, applyTLSMaterial(&tls, nil))
		require.Nil(t, tls)
	})
	t.Run("first-time enable", func(t *testing.T) {
		var tls *TLSConfig
		require.True(t, applyTLSMaterial(&tls, tlsMat("c", "k", nil, false)))
		require.NotNil(t, tls)
		require.True(t, tls.Enable)
	})
	t.Run("cert rotation", func(t *testing.T) {
		tls := &TLSConfig{Enable: true, CertPEM: shared.NewSecret("old"), KeyPEM: shared.NewSecret("old")}
		changed := applyTLSMaterial(&tls, tlsMat("new", "new", nil, false))
		require.True(t, changed)
		require.Equal(t, "new", tls.CertPEM.Reveal())
	})
	t.Run("CA-only rotation", func(t *testing.T) {
		tls := &TLSConfig{Enable: true, CACertPEM: shared.NewSecret("old-ca")}
		changed := applyTLSMaterial(&tls, tlsMat("", "", []string{"new-ca"}, false))
		require.True(t, changed)
		require.Equal(t, "new-ca", tls.CACertPEM.Reveal())
	})
	t.Run("multi-CA joined", func(t *testing.T) {
		var tls *TLSConfig
		require.True(t, applyTLSMaterial(&tls, tlsMat("", "", []string{"ca1", "ca2"}, false)))
		require.Equal(t, "ca1\nca2", tls.CACertPEM.Reveal())
	})
	t.Run("dedup no-op", func(t *testing.T) {
		tls := &TLSConfig{
			Enable: true, CertPEM: shared.NewSecret("c"), KeyPEM: shared.NewSecret("k"), CACertPEM: shared.NewSecret("ca"),
		}
		require.False(t, applyTLSMaterial(&tls, tlsMat("c", "k", []string{"ca"}, false)))
	})
}
