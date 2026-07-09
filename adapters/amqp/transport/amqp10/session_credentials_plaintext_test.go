// Validates c7-plain-plaintext on the RUNTIME credential-rotation path:
// Session.ApplyCredentials must fail closed when a rotation would newly
// expose SASL PLAIN over a non-TLS scheme. This closes the last c7
// injection point the build-time gate (config/factory/Config.ApplyCredentials)
// does not cover — a session that passed the build gate with NO username
// could otherwise be rotated to a username and ship cleartext creds on the
// next dial.
package amqp10

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestApplyCredentials_PlainOverPlaintext_Refused proves the fail-closed
// runtime gate. A plaintext amqp:// session built with NO username (so the
// build gate was satisfied — not PLAIN) is rotated to a username+password.
// go-amqp would infer SASL PLAIN from the new username and ship it in
// cleartext, so the rotation MUST be refused, the last-good credentials
// MUST stay intact, and NO forced re-dial (conn.Close) may be issued.
//
// Mutation killed: drop the added s.opts.validatePlainOverPlaintext() call
// (and its revert) from Session.ApplyCredentials. Then the username is
// accepted, liveCreds/opts mutate to the new creds, the connection is
// closed to force a cleartext re-dial, and the assertions below FAIL.
func TestApplyCredentials_PlainOverPlaintext_Refused(t *testing.T) {
	s := NewSession(SessionOptions{
		Address: "amqp://broker.example:5672/", // plaintext, NO username
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	// Simulate a live connection so we can prove NO forced re-dial happens
	// on refusal (a mutated session would call conn.Close()).
	conn := &mockConn{}
	s.mu.Lock()
	s.conn = conn
	s.connected = true
	s.mu.Unlock()

	err := s.ApplyCredentials(t.Context(),
		connectivity.NewCredentialSet(pwCred("rotated-user", "rotated-pass"), nil))

	// Refused with the plaintext-PLAIN error.
	require.Error(t, err, "rotation that newly exposes PLAIN over plaintext must be refused")
	require.Contains(t, err.Error(), "cleartext")
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok, "refusal must be a BridgeError")
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)

	// Last-good credentials intact — the new username/password were NOT
	// committed to liveCreds or opts.
	s.mu.Lock()
	live := s.liveCreds
	optsUser := s.opts.Username
	optsPass := s.opts.Password.Reveal()
	s.mu.Unlock()
	require.Equal(t, "", live.Username, "refused rotation must not mutate liveCreds.Username")
	require.Equal(t, "", live.Password, "refused rotation must not mutate liveCreds.Password")
	require.Equal(t, "", optsUser, "refused rotation must not mutate opts.Username")
	require.Equal(t, "", optsPass, "refused rotation must not mutate opts.Password")

	// No forced re-dial: the live connection was NOT torn down, so the
	// session keeps running on its last-good (no-username) state rather
	// than dialing cleartext credentials.
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.False(t, closed,
		"a refused plaintext-PLAIN rotation must NOT close the connection / force a cleartext re-dial")
}

// TestApplyCredentials_PlainOverTLS_Allowed is the counterfactual: the same
// username rotation over an amqps:// (TLS) scheme is allowed — TLS protects
// the credentials, so validatePlainOverPlaintext passes.
func TestApplyCredentials_PlainOverTLS_Allowed(t *testing.T) {
	s := NewSession(SessionOptions{
		Address: "amqps://broker.example:5671/", // TLS scheme, NO username
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(),
		connectivity.NewCredentialSet(pwCred("rotated-user", "rotated-pass"), nil))
	require.NoError(t, err, "PLAIN over amqps:// is protected by TLS and must be allowed")

	s.mu.Lock()
	live := s.liveCreds
	optsUser := s.opts.Username
	s.mu.Unlock()
	require.Equal(t, "rotated-user", live.Username, "allowed rotation must commit the new creds")
	require.Equal(t, "rotated-user", optsUser)
}

// TestApplyCredentials_PlainOverPlaintext_AllowInsecureOptIn proves the
// explicit opt-out still works on the rotation path: with
// allow_insecure_plain set, the plaintext rotation is allowed.
func TestApplyCredentials_PlainOverPlaintext_AllowInsecureOptIn(t *testing.T) {
	s := NewSession(SessionOptions{
		Address:            "amqp://broker.example:5672/",
		AllowInsecurePlain: true,
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(t.Context()) }()

	err := s.ApplyCredentials(t.Context(),
		connectivity.NewCredentialSet(pwCred("rotated-user", "rotated-pass"), nil))
	require.NoError(t, err, "allow_insecure_plain must permit the plaintext rotation")

	s.mu.Lock()
	live := s.liveCreds
	s.mu.Unlock()
	require.Equal(t, "rotated-user", live.Username, "opted-in rotation must commit the new creds")
	require.Equal(t, "rotated-pass", live.Password)
}
