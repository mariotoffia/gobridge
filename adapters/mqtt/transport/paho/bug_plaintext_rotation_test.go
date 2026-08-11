package paho

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════════════
// blocking-#3: runtime credential rotation (Session.ApplyCredentials) is the
// hole the static/deferred gates miss. A tcp:// (plaintext) session that
// started WITHOUT credentials could have username/password injected at rotation
// and sent in cleartext on the next CONNECT. The rotation MUST re-run the same
// plaintext-credentials gate BEFORE mutating liveCreds/opts, and a dial-time
// defense-in-depth guard must catch direct NewSession callers that bypass the
// factory.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_ApplyCredentials_PlaintextGate_RuntimeRotation proves the runtime
// rotation gate across the transport/opt-in axes and — critically — that a
// REJECTED rotation leaves liveCreds/opts UNCHANGED.
//
// Mutation killed: drop the plaintextCredentialViolation check from
// Session.ApplyCredentials. Then the tcp:// rotation succeeds, liveCreds/opts
// are mutated, and the require.Error + "unchanged" assertions below FAIL.
func TestBug_ApplyCredentials_PlaintextGate_RuntimeRotation(t *testing.T) {
	t.Run("tcp:// without opt-in: rotation REJECTED and state unchanged", func(t *testing.T) {
		s := NewSession(SessionOptions{
			BrokerURLs: []string{"tcp://192.0.2.1:1883"},
			ClientID:   "rot-plain",
			// No credentials at start, no opt-in: the classic hole.
		}, connectivity.SessionEphemeral, nil)
		defer func() { _ = s.Close(context.Background()) }()

		err := s.ApplyCredentials(t.Context(),
			connectivity.NewCredentialSet(pwCred("injected-user", "injected-pass"), nil))
		require.Error(t, err, "injecting credentials onto a plaintext transport must fail closed")
		require.Contains(t, err.Error(), "cleartext")
		be, ok := shared.AsBridgeError(err)
		require.True(t, ok)
		require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)

		// The rejected rotation must NOT have mutated any credential state.
		s.mu.Lock()
		defer s.mu.Unlock()
		require.Empty(t, s.liveCreds.Username, "liveCreds.Username must be unchanged after a rejected rotation")
		require.Empty(t, s.liveCreds.Password, "liveCreds.Password must be unchanged after a rejected rotation")
		require.Empty(t, s.opts.Username, "opts.Username must be unchanged after a rejected rotation")
		require.True(t, s.opts.Password.IsZero(), "opts.Password must be unchanged after a rejected rotation")
	})

	t.Run("ssl:// : rotation permitted (TLS protects credentials)", func(t *testing.T) {
		s := NewSession(SessionOptions{
			BrokerURLs: []string{"ssl://192.0.2.1:8883"},
			ClientID:   "rot-tls",
		}, connectivity.SessionEphemeral, nil)
		defer func() { _ = s.Close(context.Background()) }()

		require.NoError(t, s.ApplyCredentials(t.Context(),
			connectivity.NewCredentialSet(pwCred("u", "p"), nil)),
			"a TLS scheme protects the rotated credentials")

		s.mu.Lock()
		defer s.mu.Unlock()
		require.Equal(t, "u", s.liveCreds.Username)
		require.Equal(t, "p", s.liveCreds.Password)
	})

	t.Run("tcp:// WITH allow_plaintext_credentials: rotation permitted", func(t *testing.T) {
		s := NewSession(SessionOptions{
			BrokerURLs:                []string{"tcp://192.0.2.1:1883"},
			ClientID:                  "rot-optin",
			AllowPlaintextCredentials: true,
		}, connectivity.SessionEphemeral, nil)
		defer func() { _ = s.Close(context.Background()) }()

		require.NoError(t, s.ApplyCredentials(t.Context(),
			connectivity.NewCredentialSet(pwCred("u", "p"), nil)),
			"the explicit opt-in permits plaintext credential rotation")

		s.mu.Lock()
		defer s.mu.Unlock()
		require.Equal(t, "u", s.liveCreds.Username)
	})
}

// TestBug_Dial_PlaintextGate_DefenseInDepth proves the dial-time guard fails
// closed even for a session hand-built with credentials on a plaintext
// transport (bypassing the factory's static gate) — so cleartext credentials
// can never actually leave the process without the opt-in.
//
// Mutation killed: delete the plaintextCredentialViolation guard from dial.
// Then dial proceeds past the guard (and would try to open a cleartext
// connection carrying the credentials); require.Error + "cleartext" FAIL.
func TestBug_Dial_PlaintextGate_DefenseInDepth(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "dial-plain",
		Username:   "u",
		Password:   shared.NewSecret("p"),
		// No opt-in: even though NewSession does not run the factory gate, the
		// dial guard must refuse.
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	// A bounded context so a regression (guard removed) fails fast on the
	// unreachable TEST-NET broker instead of hanging; the guard itself returns
	// before any dial, so the guarded path is instant.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := s.dial(ctx)
	require.Error(t, err, "dial must fail closed on plaintext credentials without opt-in")
	require.Contains(t, err.Error(), "cleartext")
}
