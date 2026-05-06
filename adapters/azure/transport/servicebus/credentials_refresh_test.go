package servicebus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestCredentialsToConnection_ConnectionString(t *testing.T) {
	existing := ConnectionConfig{ConnectionString: "Endpoint=sb://old/;..."}
	out, changed := credentialsToConnection(existing, &domain.CredentialSet{
		Password: &domain.PasswordCredential{Password: "Endpoint=sb://new/;..."},
	})
	require.True(t, changed)
	require.Equal(t, "Endpoint=sb://new/;...", out.ConnectionString)
	require.Empty(t, out.ClientID)
}

func TestCredentialsToConnection_AADClientSecret(t *testing.T) {
	existing := ConnectionConfig{
		Namespace: "contoso.servicebus.windows.net",
		TenantID:  "tenant-1",
		ClientID:  "old-client",
	}
	out, changed := credentialsToConnection(existing, &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "new-client", Password: "new-secret"},
	})
	require.True(t, changed)
	require.Equal(t, "new-client", out.ClientID)
	require.Equal(t, "new-secret", out.ClientSecret)
	require.Equal(t, "tenant-1", out.TenantID, "tenant preserved")
	require.Empty(t, out.ConnectionString)
}

func TestCredentialsToConnection_NilSet_NoChange(t *testing.T) {
	existing := ConnectionConfig{ConnectionString: "endpoint"}
	_, changed := credentialsToConnection(existing, nil)
	require.False(t, changed)
}

// TestApplyCredentials_Sender_NilSet_Rejected pins the boundary check
// on the public API.
func TestApplyCredentials_Sender_NilSet_Rejected(t *testing.T) {
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: "Endpoint=sb://x/;SharedAccessKeyName=a;SharedAccessKey=b"},
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
		Connection: ConnectionConfig{ConnectionString: "Endpoint=sb://x/;SharedAccessKeyName=a;SharedAccessKey=b"},
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
	conn := ConnectionConfig{ConnectionString: "Endpoint=sb://x/;SharedAccessKeyName=a;SharedAccessKey=b"}
	s, err := NewSender(SenderConfig{QueueName: "q", Connection: conn})
	require.NoError(t, err)

	require.NoError(t, s.ApplyCredentials(t.Context(),
		&domain.CredentialSet{Password: &domain.PasswordCredential{Password: conn.ConnectionString}}))
}

// TestCredentialsToConnection_TLSMaterial_ChangesPEMFields verifies
// TLS rotation populates CaPEM, ClientCertPEM, ClientKeyPEM on the
// returned ConnectionConfig and clears TLSConfig so buildClientOptions
// rebuilds tls.Config from the new PEM material.
func TestCredentialsToConnection_TLSMaterial_ChangesPEMFields(t *testing.T) {
	existing := ConnectionConfig{ConnectionString: "Endpoint=sb://x/;..."}
	out, changed := credentialsToConnection(existing, &domain.CredentialSet{
		TLS: &domain.TLSMaterial{
			CertPEM: "--- CERT ---",
			KeyPEM:  "--- KEY ---",
			CAPEMs:  []string{"--- CA ---"},
		},
	})
	require.True(t, changed)
	require.Equal(t, "--- CERT ---", out.ClientCertPEM)
	require.Equal(t, "--- KEY ---", out.ClientKeyPEM)
	require.Equal(t, "--- CA ---", out.CaPEM)
	require.Nil(t, out.TLSConfig, "TLSConfig must be cleared so PEMs drive the rebuild")
	// Existing password material is preserved.
	require.Equal(t, existing.ConnectionString, out.ConnectionString)
}

// TestCredentialsToConnection_TLSMaterial_Dedup ensures that
// resubmitting the same TLS material is a no-op, so the
// CredentialRefresher doesn't force a client rebuild on every tick.
func TestCredentialsToConnection_TLSMaterial_Dedup(t *testing.T) {
	existing := ConnectionConfig{
		ConnectionString: "Endpoint=sb://x/;...",
		CaPEM:            "--- CA ---",
		ClientCertPEM:    "--- CERT ---",
		ClientKeyPEM:     "--- KEY ---",
	}
	_, changed := credentialsToConnection(existing, &domain.CredentialSet{
		TLS: &domain.TLSMaterial{
			CertPEM: "--- CERT ---",
			KeyPEM:  "--- KEY ---",
			CAPEMs:  []string{"--- CA ---"},
		},
	})
	require.False(t, changed)
}

// TestCredentialsToConnection_MultiCA_Joined documents how multiple
// CA PEMs are folded into a single CaPEM bundle.
func TestCredentialsToConnection_MultiCA_Joined(t *testing.T) {
	out, changed := credentialsToConnection(ConnectionConfig{}, &domain.CredentialSet{
		TLS: &domain.TLSMaterial{CAPEMs: []string{"ca1", "ca2"}},
	})
	require.True(t, changed)
	require.Equal(t, "ca1\nca2", out.CaPEM)
}

// TestBuildTLSConfig_ClientCertPEM_PartialRejected pins the both-or-
// neither contract introduced for PEM-driven client auth.
func TestBuildTLSConfig_ClientCertPEM_PartialRejected(t *testing.T) {
	_, err := buildTLSConfig("", "--- CERT ---", "", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ClientCertPEM and ClientKeyPEM")
}
