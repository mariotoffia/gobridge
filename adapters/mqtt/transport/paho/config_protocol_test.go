package paho

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
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

func TestConfigApplyCredentials_RunsEveryProtocolRule(t *testing.T) {
	for name, mutate := range map[string]func(*SessionOptions){
		"unknown version":         func(o *SessionOptions) { o.ProtocolVersion = "5" },
		"no_local on v3.1.1":      func(o *SessionOptions) { o.ProtocolVersion, o.NoLocal = ProtocolVersion311, true },
		"expiry on v3.1.1":        func(o *SessionOptions) { o.ProtocolVersion, o.SessionExpiryInterval = ProtocolVersion311, 60 },
		"unknown message_id":      func(o *SessionOptions) { o.MessageID = "sha256" },
		"content_hash on default": func(o *SessionOptions) { o.MessageID = MessageIDContentHash },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Session.BrokerURLs = []string{"ssl://broker:8883"}
			mutate(&cfg.Session)
			set := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
			require.ErrorIs(t, cfg.ApplyCredentials(set), shared.ErrInvalidConfig)
		})
	}
}

func TestSessionOptionsFromMap_ReadsProtocolVersion(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{"protocol_version": ProtocolVersion311})
	require.NoError(t, err)
	assert.Equal(t, ProtocolVersion311, opts.ProtocolVersion)

	_, err = SessionOptionsFromMap(map[string]any{"protocol_version": 5})
	require.ErrorIs(t, err, shared.ErrInvalidConfig, "a non-string protocol_version must not fall back to v5 silently")
}

func TestValidateProtocol_RejectsUnknownMessageID(t *testing.T) {
	for _, version := range []string{"", ProtocolVersion5, ProtocolVersion311} {
		t.Run("version "+version, func(t *testing.T) {
			err := SessionOptions{ProtocolVersion: version, MessageID: "sha256"}.validateProtocol("")
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
			assert.Contains(t, err.Error(), "session.message_id")
		})
	}
}

func TestValidateProtocol_RejectsContentHashMessageIDOffMQTT311(t *testing.T) {
	for _, version := range []string{"", ProtocolVersion5} {
		t.Run("version "+version, func(t *testing.T) {
			err := SessionOptions{ProtocolVersion: version, MessageID: MessageIDContentHash}.validateProtocol("")
			require.ErrorIs(t, err, shared.ErrInvalidConfig, "an MQTT 5 producer carries its own message id")
			assert.Contains(t, err.Error(), "session.message_id")
		})
	}
}

func TestValidateProtocol_AcceptsContentHashMessageIDOnMQTT311(t *testing.T) {
	modes := []connectivity.SessionMode{
		"", connectivity.SessionEphemeral, connectivity.SessionPersistent, connectivity.SessionExclusive,
	}
	for _, mode := range modes {
		t.Run("mode "+string(mode), func(t *testing.T) {
			opts := SessionOptions{ProtocolVersion: ProtocolVersion311, MessageID: MessageIDContentHash}
			assert.NoError(t, opts.validateProtocol(mode))
		})
	}
}

func TestValidateProtocol_AcceptsRandomMessageIDOnEveryVersion(t *testing.T) {
	for _, version := range []string{"", ProtocolVersion5, ProtocolVersion311} {
		for _, messageID := range []string{"", MessageIDRandom} {
			t.Run(version+"/"+messageID, func(t *testing.T) {
				opts := SessionOptions{ProtocolVersion: version, MessageID: messageID}
				assert.NoError(t, opts.validateProtocol(connectivity.SessionPersistent))
			})
		}
	}
}

func TestConfigValidate_RejectsContentHashMessageIDOnV5(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.MessageID = MessageIDContentHash
	require.ErrorIs(t, cfg.Validate(), shared.ErrInvalidConfig)

	cfg.Session.ProtocolVersion = ProtocolVersion311
	require.NoError(t, cfg.Validate())
}

func TestValidateEffectiveSession_RejectsContentHashMessageIDOnV5(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.ClientID = "c"
	cfg.Session.BrokerURLs = []string{"tcp://localhost:1883"}
	cfg.Session.MessageID = MessageIDContentHash
	require.ErrorIs(t, cfg.ValidateEffectiveSession(connectivity.SessionPersistent), shared.ErrInvalidConfig)

	cfg.Session.ProtocolVersion = ProtocolVersion311
	require.NoError(t, cfg.ValidateEffectiveSession(connectivity.SessionPersistent))
}

func TestSessionOptionsFromMap_ReadsMessageID(t *testing.T) {
	opts, err := SessionOptionsFromMap(map[string]any{"message_id": MessageIDContentHash})
	require.NoError(t, err)
	assert.Equal(t, MessageIDContentHash, opts.MessageID)

	opts, err = SessionOptionsFromMap(map[string]any{})
	require.NoError(t, err)
	assert.Empty(t, opts.MessageID, "an omitted message_id keeps the random default")

	_, err = SessionOptionsFromMap(map[string]any{"message_id": true})
	require.ErrorIs(t, err, shared.ErrInvalidConfig, "a non-string message_id must not fall back to random silently")
}

func TestPluginOptionsDecode_ReadsMessageID(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))
	decode := func(session map[string]any) (ports.PluginConfig, error) {
		return reg.Decode("mqtt", parser.NewRawConfig(map[string]any{"session": session}))
	}

	pc, err := decode(map[string]any{"protocol_version": ProtocolVersion311, "message_id": MessageIDContentHash})
	require.NoError(t, err)
	cfg, ok := pc.(*Config)
	require.True(t, ok)
	assert.Equal(t, MessageIDContentHash, cfg.Session.MessageID)

	_, err = decode(map[string]any{"message_id": MessageIDContentHash})
	require.ErrorIs(t, err, shared.ErrInvalidConfig, "content_hash on MQTT 5 is refused at load time")
}

// Content identity hashes the decoded config, so writing a documented default
// must decode to the same value as omitting it; otherwise the edit alone would
// rebuild the session (ADR 0016).
func TestPluginOptionsDecode_ExplicitDefaultsMatchOmitted(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))
	decode := func(session map[string]any) *Config {
		t.Helper()
		pc, err := reg.Decode("mqtt", parser.NewRawConfig(map[string]any{"session": session}))
		require.NoError(t, err)
		cfg, ok := pc.(*Config)
		require.True(t, ok)
		return cfg
	}

	omitted := decode(map[string]any{})
	assert.Equal(t, omitted, decode(map[string]any{"protocol_version": ProtocolVersion5}))
	assert.Equal(t, omitted, decode(map[string]any{"message_id": MessageIDRandom}))
	assert.Equal(t,
		decode(map[string]any{"protocol_version": ProtocolVersion311}),
		decode(map[string]any{"protocol_version": ProtocolVersion311, "message_id": MessageIDRandom}))
}
