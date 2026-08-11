// Validates: the MQTT CONNECT username/password travel in cleartext,
// so configuring them over a non-TLS broker URL (tcp://, mqtt://, ws://, or
// schemeless) is REJECTED at config validation / session build unless the
// operator explicitly opts in via allow_plaintext_credentials=true. A TLS
// scheme (ssl://, mqtts://, ...) protects the credentials and is always
// allowed.
package paho

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSessionOptions_PlaintextCredentials_Gate proves the fail-closed gate and
// its escape hatches across the credential vectors (username, password) and
// the TLS/non-TLS/opt-in axes.
//
// Mutation killed: drop the validatePlaintextCredentials call from
// Factory.NewSession (or Config.Validate). Then credentials over tcp:// build,
// and every wantErr=true subtest FAILs on the require.Error below.
func TestSessionOptions_PlaintextCredentials_Gate(t *testing.T) {
	tests := []struct {
		name    string
		opts    SessionOptions
		wantErr bool
	}{
		{
			name:    "username+password over plaintext tcp:// without opt-in -> rejected",
			opts:    SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}, Username: "u", Password: shared.NewSecret("p")},
			wantErr: true,
		},
		{
			name:    "username-only over plaintext mqtt:// without opt-in -> rejected",
			opts:    SessionOptions{BrokerURLs: []string{"mqtt://broker:1883"}, Username: "u"},
			wantErr: true,
		},
		{
			name:    "password-only over plaintext tcp:// without opt-in -> rejected",
			opts:    SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}, Password: shared.NewSecret("p")},
			wantErr: true,
		},
		{
			name:    "schemeless broker URL treated as plaintext -> rejected",
			opts:    SessionOptions{BrokerURLs: []string{"broker:1883"}, Username: "u"},
			wantErr: true,
		},
		{
			name:    "one plaintext URL among TLS URLs -> rejected (weakest link)",
			opts:    SessionOptions{BrokerURLs: []string{"ssl://a:8883", "tcp://b:1883"}, Username: "u"},
			wantErr: true,
		},
		{
			name:    "credentials over plaintext WITH allow_plaintext_credentials -> allowed",
			opts:    SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}, Username: "u", Password: shared.NewSecret("p"), AllowPlaintextCredentials: true},
			wantErr: false,
		},
		{
			name:    "credentials over ssl:// -> allowed (TLS protects credentials)",
			opts:    SessionOptions{BrokerURLs: []string{"ssl://broker:8883"}, Username: "u", Password: shared.NewSecret("p")},
			wantErr: false,
		},
		{
			name:    "credentials over mqtts:// -> allowed",
			opts:    SessionOptions{BrokerURLs: []string{"mqtts://broker:8883"}, Username: "u"},
			wantErr: false,
		},
		{
			name:    "credentials over tls:// -> allowed",
			opts:    SessionOptions{BrokerURLs: []string{"tls://broker:8883"}, Username: "u"},
			wantErr: false,
		},
		{
			name:    "all TLS URLs -> allowed",
			opts:    SessionOptions{BrokerURLs: []string{"ssl://a:8883", "wss://b:443"}, Username: "u"},
			wantErr: false,
		},
		{
			name:    "no credentials over plaintext tcp:// -> allowed (nothing to leak)",
			opts:    SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}},
			wantErr: false,
		},
		{
			name:    "tls.enable set but tcp:// scheme -> STILL rejected (scheme, not tls.enable, selects TLS)",
			opts:    SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}, Username: "u", TLS: &TLSConfig{Enable: true}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validatePlaintextCredentials()
			if tt.wantErr {
				require.Error(t, err, "cleartext credentials over a non-TLS broker must be rejected")
				require.Contains(t, err.Error(), "cleartext")
				be, ok := shared.AsBridgeError(err)
				require.True(t, ok, "gate must return a BridgeError")
				require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestSessionOptionsFromMap_PlaintextCredentials_Gate proves the map decode
// path enforces the gate and honours the opt-in.
func TestSessionOptionsFromMap_PlaintextCredentials_Gate(t *testing.T) {
	base := map[string]any{
		"broker_urls": []string{"tcp://localhost:1883"},
		"client_id":   "c1",
		"username":    "u",
		"password":    "p",
	}
	_, err := SessionOptionsFromMap(base)
	require.Error(t, err, "SessionOptionsFromMap must reject cleartext credentials over tcp:// without opt-in")
	require.Contains(t, err.Error(), "cleartext")

	base["allow_plaintext_credentials"] = true
	_, err = SessionOptionsFromMap(base)
	require.NoError(t, err, "allow_plaintext_credentials must let credentials over plaintext through")
}

// TestConfig_PlaintextCredentials_DeferredCredentials proves the deferred
// path: with a pending credentials_uri and no username yet, the gate cannot
// judge at parse time (username arrives later) so Validate passes; once
// ApplyCredentials resolves a username over a plaintext broker, the gate
// re-runs and fails closed.
func TestConfig_PlaintextCredentials_DeferredCredentials(t *testing.T) {
	t.Run("pending creds over plaintext: parse defers, ApplyCredentials fails closed", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}, ClientID: "c"},
			CredentialsURIRef: "vault://mqtt/creds",
		}
		require.NoError(t, c.Validate(), "no username yet + pending creds -> nothing to leak -> passes")

		set := connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil)
		err := c.ApplyCredentials(set)
		require.Error(t, err, "resolved credentials over plaintext must fail closed post-resolution")
		require.Contains(t, err.Error(), "cleartext")
	})

	t.Run("pending creds over ssl://: ApplyCredentials passes", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{BrokerURLs: []string{"ssl://broker:8883"}, ClientID: "c"},
			CredentialsURIRef: "vault://mqtt/creds",
		}
		require.NoError(t, c.Validate())
		set := connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil)
		require.NoError(t, c.ApplyCredentials(set), "TLS scheme protects resolved credentials")
	})

	t.Run("opt-in lets deferred plaintext creds through", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{BrokerURLs: []string{"tcp://broker:1883"}, ClientID: "c", AllowPlaintextCredentials: true},
			CredentialsURIRef: "vault://mqtt/creds",
		}
		require.NoError(t, c.Validate())
		set := connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil)
		require.NoError(t, c.ApplyCredentials(set))
	})
}

// TestFactory_NewSession_PlaintextCredentials_Rejected proves the gate is
// enforced at the transport build boundary (the factory), not only in the
// standalone validators.
func TestFactory_NewSession_PlaintextCredentials_Rejected(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID:          "s-plain",
		Transport:   "mqtt.paho",
		SessionMode: connectivity.SessionEphemeral,
		Config: Config{Session: SessionOptions{
			BrokerURLs: []string{"tcp://localhost:1883"},
			ClientID:   "c1",
			Username:   "u",
			Password:   shared.NewSecret("p"),
		}},
	}
	_, err := f.NewSession(context.Background(), spec)
	require.Error(t, err, "factory must reject cleartext credentials over tcp:// at build time")
	require.Contains(t, err.Error(), "cleartext")

	// With the opt-in the same session builds.
	optIn := ports.SessionSpec{
		ID:          "s-plain-ok",
		Transport:   "mqtt.paho",
		SessionMode: connectivity.SessionEphemeral,
		Config: Config{Session: SessionOptions{
			BrokerURLs:                []string{"tcp://localhost:1883"},
			ClientID:                  "c1",
			Username:                  "u",
			Password:                  shared.NewSecret("p"),
			AllowPlaintextCredentials: true,
		}},
	}
	sess, err := f.NewSession(context.Background(), optIn)
	require.NoError(t, err)
	require.NotNil(t, sess)
}
