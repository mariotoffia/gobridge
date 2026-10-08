package paho

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestValidateProtocol_AcceptsEveryOptionOnV5(t *testing.T) {
	for _, version := range []string{"", ProtocolVersion5} {
		opts := SessionOptions{
			ProtocolVersion:       version,
			NoLocal:               true,
			SessionExpiryInterval: 3600,
			Password:              shared.NewSecret("only-a-password"),
			CleanStart:            true,
		}
		assert.NoError(t, opts.validateProtocol(connectivity.SessionPersistent), "version %q", version)
	}
}

func TestValidateProtocol_RejectsUnknownVersion(t *testing.T) {
	err := SessionOptions{ProtocolVersion: "5"}.validateProtocol("")
	require.ErrorIs(t, err, shared.ErrInvalidConfig)
	assert.Contains(t, err.Error(), "protocol_version")
}

func TestValidateProtocol_ErrorsNameTheProtocolVersionKey(t *testing.T) {
	cases := map[string]SessionOptions{
		"unknown version":              {ProtocolVersion: "5"},
		"option unavailable on v3.1.1": {ProtocolVersion: ProtocolVersion311, NoLocal: true},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			err := opts.validateProtocol("")
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
			assert.Contains(t, err.Error(), "session.protocol_version")
		})
	}
}

func TestValidateProtocol_RejectsWhatMQTT311CannotExpress(t *testing.T) {
	cases := []struct {
		name string
		opts SessionOptions
		mode connectivity.SessionMode
		key  string
	}{
		{"no_local", SessionOptions{NoLocal: true}, "", "session.no_local"},
		{"session expiry", SessionOptions{SessionExpiryInterval: 60}, "", "session.session_expiry_interval"},
		{"password without username", SessionOptions{Password: shared.NewSecret("p")}, "", "session.password"},
		{"persistent clean start", SessionOptions{CleanStart: true}, connectivity.SessionPersistent, "session.clean_start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.ProtocolVersion = ProtocolVersion311
			err := tc.opts.validateProtocol(tc.mode)
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
			assert.Contains(t, err.Error(), tc.key)
		})
	}
}

func TestValidateProtocol_AllowsWhatMQTT311CanExpress(t *testing.T) {
	ok := []struct {
		name string
		opts SessionOptions
		mode connectivity.SessionMode
	}{
		{"persistent username and password", SessionOptions{Username: "u", Password: shared.NewSecret("p")}, connectivity.SessionPersistent},
		{"ephemeral clean start", SessionOptions{CleanStart: true}, connectivity.SessionEphemeral},
		{"exclusive clean start", SessionOptions{CleanStart: true}, connectivity.SessionExclusive},
		// The mode rule waits for ValidateEffectiveSession.
		{"clean start, mode unknown", SessionOptions{CleanStart: true}, ""},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.ProtocolVersion = ProtocolVersion311
			assert.NoError(t, tc.opts.validateProtocol(tc.mode))
		})
	}
}

func TestConfigValidate_RejectsNoLocalOnMQTT311(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ProtocolVersion = ProtocolVersion311
	cfg.Session.NoLocal = true
	require.ErrorIs(t, cfg.Validate(), shared.ErrInvalidConfig)
}

func TestValidateEffectiveSession_RejectsPersistentCleanStartOnMQTT311(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ProtocolVersion = ProtocolVersion311
	cfg.Session.ClientID = "c"
	cfg.Session.BrokerURLs = []string{"tcp://localhost:1883"}
	cfg.Session.CleanStart = true
	require.ErrorIs(t, cfg.ValidateEffectiveSession(connectivity.SessionPersistent), shared.ErrInvalidConfig)
	require.NoError(t, cfg.ValidateEffectiveSession(connectivity.SessionEphemeral))
}

func TestConfigApplyCredentials_RejectsAResolvedPasswordWithoutUsernameOnMQTT311(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ProtocolVersion = ProtocolVersion311
	cfg.Session.BrokerURLs = []string{"ssl://broker:8883"}
	set := connectivity.NewCredentialSet(pwCred("", "p"), nil)
	require.ErrorIs(t, cfg.ApplyCredentials(set), shared.ErrInvalidConfig)
}

func TestSessionOptionsFromMap_ReadsProtocolVersion(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{"protocol_version": ProtocolVersion311})
	require.NoError(t, err)
	assert.Equal(t, ProtocolVersion311, opts.ProtocolVersion)

	_, err = SessionOptionsFromMap(map[string]any{"protocol_version": 5})
	require.ErrorIs(t, err, shared.ErrInvalidConfig, "a non-string protocol_version must not fall back to v5 silently")
}
