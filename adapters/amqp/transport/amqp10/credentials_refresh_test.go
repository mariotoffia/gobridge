package amqp10

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestApplyCredentials_BeforeStart_UpdatesLiveCreds verifies that
// rotating credentials on a not-yet-connected session stores them so
// the first dial picks them up via connect().
func TestApplyCredentials_BeforeStart_UpdatesLiveCreds(t *testing.T) {
	s := NewSession(SessionOptions{
		Address:  "amqp://broker.example:5672/",
		Username: "u-old",
		Password: "p-old",
	}, domain.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(), &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u-new", Password: "p-new"},
	})
	require.NoError(t, err)

	s.mu.Lock()
	got := s.liveCreds
	s.mu.Unlock()
	require.Equal(t, "u-new", got.Username)
	require.Equal(t, "p-new", got.Password)
	require.Equal(t, "u-new", s.opts.Username)
	require.Equal(t, "p-new", s.opts.Password)
}

// TestApplyCredentials_NilSet_Rejected pins the boundary check.
func TestApplyCredentials_NilSet_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqp://broker.example:5672/"},
		domain.SessionEphemeral, nil)
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
		Password: "p",
	}, domain.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	require.NoError(t, s.ApplyCredentials(t.Context(), &domain.CredentialSet{
		TLS: &domain.TLSMaterial{
			CertPEM: "--- CERT ---",
			KeyPEM:  "--- KEY ---",
			CAPEMs:  []string{"--- CA ---"},
		},
	}))
	s.mu.Lock()
	got := s.liveCreds
	optsTLS := s.opts.TLS
	s.mu.Unlock()
	require.Equal(t, "u", got.Username)
	require.NotNil(t, optsTLS)
	require.True(t, optsTLS.Enable)
	require.Equal(t, "--- CERT ---", optsTLS.CertPEM)
	require.Equal(t, "--- KEY ---", optsTLS.KeyPEM)
	require.Equal(t, "--- CA ---", optsTLS.CACertPEM)
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
		require.True(t, applyAMQP10TLSMaterial(&tls, &domain.TLSMaterial{
			CertPEM: "c", KeyPEM: "k",
		}))
		require.NotNil(t, tls)
		require.True(t, tls.Enable)
	})
	t.Run("dedup no-op", func(t *testing.T) {
		tls := &TLSConfig{
			Enable: true, CertPEM: "c", KeyPEM: "k", CACertPEM: "ca",
		}
		require.False(t, applyAMQP10TLSMaterial(&tls, &domain.TLSMaterial{
			CertPEM: "c", KeyPEM: "k", CAPEMs: []string{"ca"},
		}))
	})
}

// TestApplyCredentials_ClosedSession_Rejected pins the closed-session
// path.
func TestApplyCredentials_ClosedSession_Rejected(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqp://broker.example:5672/"},
		domain.SessionEphemeral, nil)
	require.NoError(t, s.Close(t.Context()))

	err := s.ApplyCredentials(t.Context(), &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	})
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeUnavailable, be.Code)
}
