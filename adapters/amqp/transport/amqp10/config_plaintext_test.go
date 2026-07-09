// Validates c7-plain-plaintext: SASL PLAIN (username/password) is
// REJECTED at config validation over a non-TLS scheme unless the operator
// explicitly opts in via allow_insecure_plain. SASL PLAIN sends the
// credentials in cleartext, so on a plaintext amqp:// address they travel
// on the wire in the clear.
package amqp10

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSessionOptions_PlainOverPlaintext_Gate proves the fail-closed gate
// and its escape hatches, across both the explicit sasl_mechanism=plain
// and the inferred (username-present) PLAIN cases.
//
// Mutation killed: drop the `validatePlainOverPlaintext` call from
// SessionOptions.validate (the gate). Then PLAIN over amqp:// without the
// opt-in validates, and every wantErr=true subtest FAILs on the
// require.Error below.
func TestSessionOptions_PlainOverPlaintext_Gate(t *testing.T) {
	tests := []struct {
		name    string
		opts    SessionOptions
		wantErr bool
	}{
		{
			name:    "explicit plain over plaintext amqp:// without opt-in -> rejected",
			opts:    SessionOptions{Address: "amqp://broker:5672", SASLMechanism: "plain", Username: "u", Password: shared.NewSecret("p")},
			wantErr: true,
		},
		{
			name:    "inferred plain (username set) over plaintext amqp:// without opt-in -> rejected",
			opts:    SessionOptions{Address: "amqp://broker:5672", Username: "u", Password: shared.NewSecret("p")},
			wantErr: true,
		},
		{
			name:    "schemeless address is treated as plaintext -> rejected",
			opts:    SessionOptions{Address: "broker:5672", SASLMechanism: "plain", Username: "u"},
			wantErr: true,
		},
		{
			name:    "explicit plain over plaintext WITH allow_insecure_plain -> allowed",
			opts:    SessionOptions{Address: "amqp://broker:5672", SASLMechanism: "plain", Username: "u", Password: shared.NewSecret("p"), AllowInsecurePlain: true},
			wantErr: false,
		},
		{
			name:    "inferred plain over plaintext WITH allow_insecure_plain -> allowed",
			opts:    SessionOptions{Address: "amqp://broker:5672", Username: "u", AllowInsecurePlain: true},
			wantErr: false,
		},
		{
			name:    "explicit plain over amqps:// -> allowed (TLS protects credentials)",
			opts:    SessionOptions{Address: "amqps://broker:5671", SASLMechanism: "plain", Username: "u", Password: shared.NewSecret("p")},
			wantErr: false,
		},
		{
			name:    "inferred plain over amqp+ssl:// -> allowed",
			opts:    SessionOptions{Address: "amqp+ssl://broker:5671", Username: "u"},
			wantErr: false,
		},
		{
			name:    "no credentials, no mechanism over plaintext -> allowed (not PLAIN)",
			opts:    SessionOptions{Address: "amqp://broker:5672"},
			wantErr: false,
		},
		{
			name:    "anonymous over plaintext -> allowed (not PLAIN)",
			opts:    SessionOptions{Address: "amqp://broker:5672", SASLMechanism: "anonymous"},
			wantErr: false,
		},
		// URL-embedded credentials: go-amqp's dialConn selects SASL PLAIN
		// from Address userinfo REGARDLESS of o.Username / sasl_mechanism
		// (conn.go:224 overrides the assembled SASLType). These must be
		// caught even though o.Username is empty.
		{
			name:    "URL userinfo (user:pass) over plaintext amqp:// -> rejected (cleartext PLAIN via URL)",
			opts:    SessionOptions{Address: "amqp://user:pass@broker:5672"},
			wantErr: true,
		},
		{
			name:    "URL userinfo (username only) over plaintext amqp:// -> rejected (go-amqp still PLAINs)",
			opts:    SessionOptions{Address: "amqp://admin@broker:5672/vhost"},
			wantErr: true,
		},
		{
			name:    "URL userinfo over plaintext with sasl_mechanism=external -> rejected (URL override wins)",
			opts:    SessionOptions{Address: "amqp://user:pass@broker:5672", SASLMechanism: "external"},
			wantErr: true,
		},
		{
			name:    "URL userinfo over plaintext WITH allow_insecure_plain -> allowed",
			opts:    SessionOptions{Address: "amqp://user:pass@broker:5672", AllowInsecurePlain: true},
			wantErr: false,
		},
		{
			name:    "URL userinfo over amqps:// -> allowed (TLS protects URL credentials)",
			opts:    SessionOptions{Address: "amqps://user:pass@broker:5671"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate(false)
			if tt.wantErr {
				require.Error(t, err, "PLAIN cleartext over a non-TLS scheme must be rejected")
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

// TestSessionOptions_URLEmbeddedCredentials_Rejected pins the go-amqp
// URL-userinfo bypass in isolation: an Address like amqp://user:pass@host
// carries NO o.Username and NO sasl_mechanism, yet go-amqp (conn.go:224)
// selects SASL PLAIN from the URL userinfo and ships the credentials in
// cleartext over plaintext amqp://. The gate must reject it; TLS allows it.
//
// Mutation killed: drop the `u.User != nil` userinfo branch from
// usesSASLPlain (config.go). Then usesSASLPlain sees empty o.Username +
// empty mechanism, returns false, validate() passes and the credentials
// leak — the require.Error below FAILs.
func TestSessionOptions_URLEmbeddedCredentials_Rejected(t *testing.T) {
	// user:pass in the URL over plaintext -> cleartext PLAIN via go-amqp.
	err := (&SessionOptions{Address: "amqp://user:pass@broker:5672"}).validate(false)
	require.Error(t, err, "URL-embedded credentials over plaintext amqp:// must be rejected (go-amqp PLAINs from userinfo)")
	require.Contains(t, err.Error(), "cleartext")

	// username-only userinfo still triggers go-amqp SASL PLAIN.
	err = (&SessionOptions{Address: "amqp://admin@broker:5672/vhost"}).validate(false)
	require.Error(t, err, "URL username-only userinfo over plaintext must be rejected")

	// TLS scheme protects the URL credentials.
	err = (&SessionOptions{Address: "amqps://user:pass@broker:5671"}).validate(false)
	require.NoError(t, err, "URL-embedded credentials over amqps:// are protected by TLS")

	// Explicit opt-in still allows the insecure path.
	err = (&SessionOptions{Address: "amqp://user:pass@broker:5672", AllowInsecurePlain: true}).validate(false)
	require.NoError(t, err, "allow_insecure_plain must permit URL-embedded credentials over plaintext")
}

func TestSessionOptionsFromMap_PlainOverPlaintext_Gate(t *testing.T) {
	base := map[string]any{
		"address":        "amqp://localhost:5672",
		"sasl_mechanism": "plain",
		"username":       "u",
		"password":       "p",
	}
	_, err := SessionOptionsFromMap(base)
	require.Error(t, err, "SessionOptionsFromMap must reject PLAIN over plaintext without opt-in")
	require.Contains(t, err.Error(), "cleartext")

	base["allow_insecure_plain"] = true
	_, err = SessionOptionsFromMap(base)
	require.NoError(t, err, "allow_insecure_plain must let PLAIN over plaintext through")
}

// TestConfig_PlainOverPlaintext_DeferredCredentials proves the deferred
// path: with a pending credentials_uri the inferred-PLAIN case cannot be
// judged at parse time (the username arrives later), so Validate passes;
// once ApplyCredentials resolves a username, the gate re-runs and fails
// closed over plaintext. An explicit sasl_mechanism=plain is rejected up
// front because the scheme is already fixed.
func TestConfig_PlainOverPlaintext_DeferredCredentials(t *testing.T) {
	t.Run("inferred plain: parse-time defers, ApplyCredentials fails closed", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{Address: "amqp://broker:5672"},
			CredentialsURIRef: "vault://amqp/creds",
		}
		// No username yet + pending creds -> not (yet) PLAIN -> passes.
		require.NoError(t, c.Validate())

		// Resolution supplies a username -> inferred PLAIN over plaintext.
		set := connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil)
		err := c.ApplyCredentials(set)
		require.Error(t, err, "resolved PLAIN over plaintext must fail closed post-resolution")
		require.Contains(t, err.Error(), "cleartext")
	})

	t.Run("inferred plain over amqps: ApplyCredentials passes", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{Address: "amqps://broker:5671"},
			CredentialsURIRef: "vault://amqp/creds",
		}
		require.NoError(t, c.Validate())
		set := connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil)
		require.NoError(t, c.ApplyCredentials(set), "TLS scheme protects resolved PLAIN credentials")
	})

	t.Run("explicit plain over plaintext: rejected at parse time even with pending creds", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{Address: "amqp://broker:5672", SASLMechanism: "plain"},
			CredentialsURIRef: "vault://amqp/creds",
		}
		err := c.Validate()
		require.Error(t, err, "explicit plain over plaintext is insecure regardless of where credentials resolve from")
		require.Contains(t, err.Error(), "cleartext")
	})

	t.Run("opt-in lets deferred plain through", func(t *testing.T) {
		c := &Config{
			Session:           SessionOptions{Address: "amqp://broker:5672", AllowInsecurePlain: true},
			CredentialsURIRef: "vault://amqp/creds",
		}
		require.NoError(t, c.Validate())
		set := connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil)
		require.NoError(t, c.ApplyCredentials(set))
	})
}

// TestFactory_NewSession_PlainOverPlaintext_Rejected proves the gate is
// enforced at the transport build boundary (the factory), not only in the
// standalone validators.
func TestFactory_NewSession_PlainOverPlaintext_Rejected(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID:          "s-plain",
		Transport:   "amqp10",
		SessionMode: connectivity.SessionEphemeral,
		Config: Config{Session: SessionOptions{
			Address:       "amqp://localhost:5672",
			SASLMechanism: "plain",
			Username:      "u",
			Password:      shared.NewSecret("p"),
		}},
	}
	_, err := f.NewSession(t.Context(), spec)
	require.Error(t, err, "factory must reject PLAIN over plaintext at build time")
	require.Contains(t, err.Error(), "cleartext")

	// With the opt-in the same session builds.
	optIn := ports.SessionSpec{
		ID:          "s-plain-ok",
		Transport:   "amqp10",
		SessionMode: connectivity.SessionEphemeral,
		Config: Config{Session: SessionOptions{
			Address:            "amqp://localhost:5672",
			SASLMechanism:      "plain",
			Username:           "u",
			Password:           shared.NewSecret("p"),
			AllowInsecurePlain: true,
		}},
	}
	sess, err := f.NewSession(t.Context(), optIn)
	require.NoError(t, err)
	require.NotNil(t, sess)
}
