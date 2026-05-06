package amqp091

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestApplyCredentials_BeforeStart_UpdatesLiveCreds verifies that
// rotating credentials on a not-yet-connected session stores them so
// the first dial picks them up via brokerURL().
func TestApplyCredentials_BeforeStart_UpdatesLiveCreds(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://broker.example:5672/",
		Username:  "u-old",
		Password:  "p-old",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(), &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u-new", Password: "p-new"},
	})
	require.NoError(t, err)

	s.mu.Lock()
	got := s.liveCreds
	s.mu.Unlock()
	require.Equal(t, "u-new", got.Username)
	require.Equal(t, "p-new", got.Password)
	require.Equal(t, "u-new", s.opts.Username, "opts.Username must also be updated")
	require.Equal(t, "p-new", s.opts.Password)
}

// TestApplyCredentials_NilSet_Rejected pins the boundary check.
func TestApplyCredentials_NilSet_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{BrokerURL: "amqp://broker.example:5672/"},
		connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(), nil)
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
}

// TestApplyCredentials_TLSMaterial_StashesOnOpts verifies TLS
// rotation updates s.opts.TLS in-place so the next dial picks up the
// rotated material. liveCreds for password remain unchanged when the
// set carries only TLS.
func TestApplyCredentials_TLSMaterial_StashesOnOpts(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://broker.example:5672/",
		Username:  "u",
		Password:  "p",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	require.NoError(t, s.ApplyCredentials(t.Context(), &connectivity.CredentialSet{
		TLS: &connectivity.TLSMaterial{
			CertPEM: "--- CERT ---",
			KeyPEM:  "--- KEY ---",
			CAPEMs:  []string{"--- CA ---"},
		},
	}))
	s.mu.Lock()
	got := s.liveCreds
	optsTLS := s.opts.TLS
	s.mu.Unlock()
	require.Equal(t, "u", got.Username, "password creds unchanged by TLS-only set")
	require.NotNil(t, optsTLS)
	require.True(t, optsTLS.Enable)
	require.Equal(t, "--- CERT ---", optsTLS.CertPEM)
	require.Equal(t, "--- KEY ---", optsTLS.KeyPEM)
	require.Equal(t, "--- CA ---", optsTLS.CACertPEM)
}

// TestApplyCredentials_TLSMaterial_RebuildsDialWhenEnabling pins the
// edge case where rotation turns on TLS for a session that didn't
// have it. The dial closure must be rebuilt; otherwise the next
// reconnect would still use the plain-text dialer.
func TestApplyCredentials_TLSMaterial_RebuildsDialWhenEnabling(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://broker.example:5672/",
		Username:  "u",
		Password:  "p",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	s.mu.Lock()
	dialBefore := s.dial
	s.mu.Unlock()

	require.NoError(t, s.ApplyCredentials(t.Context(), &connectivity.CredentialSet{
		TLS: &connectivity.TLSMaterial{
			CertPEM: "--- CERT ---",
			KeyPEM:  "--- KEY ---",
		},
	}))

	s.mu.Lock()
	dialAfter := s.dial
	s.mu.Unlock()
	// Function values can't be compared directly for equality in Go,
	// but reflect.ValueOf().Pointer() suffices to detect that a new
	// closure was installed.
	require.NotEqual(t, funcPtr(dialBefore), funcPtr(dialAfter),
		"dial closure must be rebuilt when TLS is newly enabled")
}

// TestApplyAMQPTLSMaterial_ChangeDetection covers the pure helper.
func TestApplyAMQPTLSMaterial_ChangeDetection(t *testing.T) {
	t.Run("nil material no-op", func(t *testing.T) {
		var tls *TLSConfig
		require.False(t, applyAMQPTLSMaterial(&tls, nil))
		require.Nil(t, tls)
	})
	t.Run("first-time enable", func(t *testing.T) {
		var tls *TLSConfig
		require.True(t, applyAMQPTLSMaterial(&tls, &connectivity.TLSMaterial{
			CertPEM: "c", KeyPEM: "k",
		}))
		require.NotNil(t, tls)
		require.True(t, tls.Enable)
	})
	t.Run("dedup no-op", func(t *testing.T) {
		tls := &TLSConfig{
			Enable: true, CertPEM: "c", KeyPEM: "k", CACertPEM: "ca",
		}
		require.False(t, applyAMQPTLSMaterial(&tls, &connectivity.TLSMaterial{
			CertPEM: "c", KeyPEM: "k", CAPEMs: []string{"ca"},
		}))
	})
	t.Run("cert rotation", func(t *testing.T) {
		tls := &TLSConfig{Enable: true, CertPEM: "old", KeyPEM: "old"}
		require.True(t, applyAMQPTLSMaterial(&tls, &connectivity.TLSMaterial{
			CertPEM: "new", KeyPEM: "new",
		}))
		require.Equal(t, "new", tls.CertPEM)
	})
}

// funcPtr exposes the underlying pointer of a function value for the
// dial-closure-swap assertion above. Using a named helper keeps the
// test assertion readable.
func funcPtr(fn dialFunc) uintptr {
	// reflect is imported lazily — prefer unsafe-style pointer compare
	// via fmt.Sprintf to avoid pulling reflect into the test file.
	return *(*uintptr)(unsafe.Pointer(&fn))
}

// TestApplyCredentials_ClosedSession_Rejected pins the closed-session
// path.
func TestApplyCredentials_ClosedSession_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{BrokerURL: "amqp://broker.example:5672/"},
		connectivity.SessionEphemeral, nil)
	require.NoError(t, s.Close(t.Context()))

	err := s.ApplyCredentials(t.Context(), &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "u", Password: "p"},
	})
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code)
}
