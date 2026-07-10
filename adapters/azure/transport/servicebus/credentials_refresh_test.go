package servicebus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestCredentialsToConnection_ConnectionString(t *testing.T) {
	existing := ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://old/;...")}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(pwCred("", "Endpoint=sb://new/;..."), nil))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "Endpoint=sb://new/;...", out.ConnectionString.Reveal())
	require.Empty(t, out.ClientID)
}

func TestCredentialsToConnection_AADClientSecret(t *testing.T) {
	existing := ConnectionConfig{
		Namespace: "contoso.servicebus.windows.net",
		TenantID:  "tenant-1",
		ClientID:  "old-client",
	}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(pwCred("new-client", "new-secret"), nil))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "new-client", out.ClientID)
	require.Equal(t, "new-secret", out.ClientSecret.Reveal())
	require.Equal(t, "tenant-1", out.TenantID, "tenant preserved")
	require.Empty(t, out.ConnectionString)
}

func TestCredentialsToConnection_NilSet_NoChange(t *testing.T) {
	existing := ConnectionConfig{ConnectionString: shared.NewSecret("endpoint")}
	_, changed, err := credentialsToConnection(existing, nil)
	require.NoError(t, err)
	require.False(t, changed)
}

// TestCredentialsToConnection_ClientSecretClearsManagedIdentity pins
// HIGH-2: rotating from managed identity to an AAD client secret MUST
// clear UseManagedIdentity. The credential builder (rawNewAzClient)
// evaluates managed identity BEFORE client-secret auth, so a lingering
// flag would ignore the rotated secret and keep authenticating as the
// wrong identity.
//
// Mutation: drop `out.UseManagedIdentity = false` from the client-secret
// branch. Then UseManagedIdentity stays true and the assertion FAILS.
func TestCredentialsToConnection_ClientSecretClearsManagedIdentity(t *testing.T) {
	existing := ConnectionConfig{
		Namespace:          "contoso.servicebus.windows.net",
		TenantID:           "tenant-1",
		UseManagedIdentity: true,
	}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(pwCred("app-client", "app-secret"), nil))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "app-client", out.ClientID)
	require.Equal(t, "app-secret", out.ClientSecret.Reveal())
	require.False(t, out.UseManagedIdentity,
		"managed identity must be cleared so the rotated client secret takes effect (HIGH-2)")
	require.Equal(t, "tenant-1", out.TenantID, "tenant preserved")
}

// TestCredentialsToConnection_ManagedIdentityFlagStuck_ClearedWhenSecretMatches
// covers the degenerate state where the ClientID/secret already equal the
// rotated material but UseManagedIdentity is still (wrongly) set. Including
// the flag in the change guard forces a rebuild so the contradictory flag
// cannot persist and silently override the client secret.
func TestCredentialsToConnection_ManagedIdentityFlagStuck_ClearedWhenSecretMatches(t *testing.T) {
	existing := ConnectionConfig{
		Namespace:          "contoso.servicebus.windows.net",
		TenantID:           "tenant-1",
		ClientID:           "app-client",
		ClientSecret:       shared.NewSecret("app-secret"),
		UseManagedIdentity: true, // contradictory leftover
	}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(pwCred("app-client", "app-secret"), nil))
	require.NoError(t, err)
	require.True(t, changed, "a stuck UseManagedIdentity flag alone must force the client-secret switch")
	require.False(t, out.UseManagedIdentity)
	require.Equal(t, "app-client", out.ClientID)
	require.Equal(t, "app-secret", out.ClientSecret.Reveal())
}

// TestCredentialsToConnection_UsernameWithoutSecret_Rejected pins the
// HIGH-2 follow-up: a rotation credential that supplies a username but NO
// secret is malformed (client-secret auth needs both; the connection-
// string path needs an empty username). It must be rejected with
// shared.ErrInvalidPayload and MUST NOT clear UseManagedIdentity — the
// old behaviour stored a zero ClientSecret, cleared the flag, and let
// rawNewAzClient silently fall through to DefaultAzureCredential.
//
// Mutation: fold the `case Password().IsZero()` guard back into the
// client-secret `default` branch. Then err is nil, UseManagedIdentity is
// cleared, and every assertion here FAILS.
func TestCredentialsToConnection_UsernameWithoutSecret_Rejected(t *testing.T) {
	existing := ConnectionConfig{
		Namespace:          "contoso.servicebus.windows.net",
		TenantID:           "tenant-1",
		UseManagedIdentity: true,
	}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(pwCred("app-client", ""), nil))
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
	require.False(t, changed, "a malformed credential rotates nothing")
	require.True(t, out.UseManagedIdentity, "managed identity must NOT be cleared by a secret-less username")
	require.Empty(t, out.ClientID, "no client-secret material is stored")
	require.True(t, out.ClientSecret.IsZero())
}

// TestCredentialsToConnection_UsernameWithSecret_PrefersClientSecret is
// test (c): a username WITH a non-empty secret takes the client-secret
// branch (store ClientID/secret, clear MI, no error) — it must NOT be
// swept into the secret-less rejection.
//
// Mutation: drop the `.IsZero()` qualifier so ANY username hits the
// reject branch. Then this returns an error and the assertions FAIL.
func TestCredentialsToConnection_UsernameWithSecret_PrefersClientSecret(t *testing.T) {
	existing := ConnectionConfig{
		Namespace:          "contoso.servicebus.windows.net",
		TenantID:           "tenant-1",
		UseManagedIdentity: true,
	}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(pwCred("app-client", "app-secret"), nil))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "app-client", out.ClientID)
	require.Equal(t, "app-secret", out.ClientSecret.Reveal())
	require.False(t, out.UseManagedIdentity, "a valid client secret clears managed identity (HIGH-2)")
}

// TestApplyCredentials_Sender_UsernameWithoutSecret_Rejected proves the
// malformed-credential rejection surfaces through the public rotation API
// (ErrInvalidPayload) rather than silently drifting the sender's auth
// identity to DefaultAzureCredential.
func TestApplyCredentials_Sender_UsernameWithoutSecret_Rejected(t *testing.T) {
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{Namespace: "contoso.servicebus.windows.net", UseManagedIdentity: true},
	})
	require.NoError(t, err)

	err = s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(pwCred("app-client", ""), nil))
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
}

// TestApplyCredentials_Sender_NilSet_Rejected pins the boundary check
// on the public API.
func TestApplyCredentials_Sender_NilSet_Rejected(t *testing.T) {
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://x/;SharedAccessKeyName=a;SharedAccessKey=b")},
	})
	require.NoError(t, err)

	err = s.ApplyCredentials(t.Context(), nil)
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
}

// TestApplyCredentials_Receiver_NilSet_Rejected mirrors the Sender
// boundary test.
func TestApplyCredentials_Receiver_NilSet_Rejected(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://x/;SharedAccessKeyName=a;SharedAccessKey=b")},
	}, nil)
	require.NoError(t, err)

	err = r.ApplyCredentials(t.Context(), nil)
	require.Error(t, err)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
}

// TestApplyCredentials_NoChange_ReturnsNil verifies that a matching
// credential set is a no-op — avoids rebuilding the client for
// rotation events that don't actually change material.
func TestApplyCredentials_NoChange_ReturnsNil(t *testing.T) {
	conn := ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://x/;SharedAccessKeyName=a;SharedAccessKey=b")}
	s, err := NewSender(SenderConfig{QueueName: "q", Connection: conn})
	require.NoError(t, err)

	require.NoError(t, s.ApplyCredentials(t.Context(),
		connectivity.NewCredentialSet(pwCred("", conn.ConnectionString.Reveal()), nil)))
}

// TestCredentialsToConnection_TLSMaterial_ChangesPEMFields verifies
// TLS rotation populates CaPEM, ClientCertPEM, ClientKeyPEM on the
// returned ConnectionConfig and clears TLSConfig so buildClientOptions
// rebuilds tls.Config from the new PEM material.
func TestCredentialsToConnection_TLSMaterial_ChangesPEMFields(t *testing.T) {
	existing := ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://x/;...")}
	out, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(nil, tlsMat("--- CERT ---", "--- KEY ---", []string{"--- CA ---"}, false)))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "--- CERT ---", out.ClientCertPEM.Reveal())
	require.Equal(t, "--- KEY ---", out.ClientKeyPEM.Reveal())
	require.Equal(t, "--- CA ---", out.CaPEM.Reveal())
	require.Nil(t, out.TLSConfig, "TLSConfig must be cleared so PEMs drive the rebuild")
	// Existing password material is preserved.
	require.Equal(t, existing.ConnectionString, out.ConnectionString)
}

// TestCredentialsToConnection_TLSMaterial_Dedup ensures that
// resubmitting the same TLS material is a no-op, so the
// CredentialRefresher doesn't force a client rebuild on every tick.
func TestCredentialsToConnection_TLSMaterial_Dedup(t *testing.T) {
	existing := ConnectionConfig{
		ConnectionString: shared.NewSecret("Endpoint=sb://x/;..."),
		CaPEM:            shared.NewSecret("--- CA ---"),
		ClientCertPEM:    shared.NewSecret("--- CERT ---"),
		ClientKeyPEM:     shared.NewSecret("--- KEY ---"),
	}
	_, changed, err := credentialsToConnection(existing, connectivity.NewCredentialSet(nil, tlsMat("--- CERT ---", "--- KEY ---", []string{"--- CA ---"}, false)))
	require.NoError(t, err)
	require.False(t, changed)
}

// TestCredentialsToConnection_MultiCA_Joined documents how multiple
// CA PEMs are folded into a single CaPEM bundle.
func TestCredentialsToConnection_MultiCA_Joined(t *testing.T) {
	out, changed, err := credentialsToConnection(ConnectionConfig{}, connectivity.NewCredentialSet(nil, tlsMat("", "", []string{"ca1", "ca2"}, false)))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "ca1\nca2", out.CaPEM.Reveal())
}

// TestBuildTLSConfig_ClientCertPEM_PartialRejected pins the both-or-
// neither contract introduced for PEM-driven client auth.
func TestBuildTLSConfig_ClientCertPEM_PartialRejected(t *testing.T) {
	_, err := buildTLSConfig("", "--- CERT ---", "", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ClientCertPEM and ClientKeyPEM")
}

func pwCred(u, p string) *connectivity.PasswordCredential {
	c := connectivity.NewPasswordCredential(u, p)
	return &c
}

func tlsMat(cert, key string, ca []string, insecure bool) *connectivity.TLSMaterial {
	m := connectivity.NewTLSMaterial(cert, key, ca, insecure)
	return &m
}
