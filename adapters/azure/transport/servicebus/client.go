package servicebus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

// asbAPI is the subset of the Azure Service Bus Receiver SDK used by
// the bridge Receiver. It enables test-double injection.
type asbAPI interface {
	ReceiveMessages(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error)
	CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.CompleteMessageOptions) error
	AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.AbandonMessageOptions) error
	RenewMessageLock(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.RenewMessageLockOptions) error
}

// asbSenderAPI is the subset of the Azure Service Bus Sender SDK used
// by the bridge Sender.
type asbSenderAPI interface {
	SendMessage(ctx context.Context, message *azservicebus.Message, options *azservicebus.SendMessageOptions) error
	NewMessageBatch(ctx context.Context, options *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error)
	SendMessageBatch(ctx context.Context, batch *azservicebus.MessageBatch, options *azservicebus.SendMessageBatchOptions) error
	Close(ctx context.Context) error
}

var (
	_ asbAPI       = (*azservicebus.Receiver)(nil)
	_ asbSenderAPI = (*azservicebus.Sender)(nil)
)

// ConnectionConfig holds the credentials and TLS settings shared by
// both Receiver and Sender.
type ConnectionConfig struct {
	ConnectionString   string
	Namespace          string
	UseManagedIdentity bool
	TenantID           string
	ClientID           string
	ClientSecret       string
	TLSConfig          *tls.Config
	CaPEM              string
	InsecureSkipVerify bool
}

// buildClient creates an azservicebus.Client from the given connection
// configuration. It supports connection-string auth, managed identity,
// client-secret credentials, and the default Azure credential chain.
func buildClient(cfg ConnectionConfig) (*azservicebus.Client, error) {
	opts := buildClientOptions(cfg)

	if cfg.ConnectionString != "" {
		return azservicebus.NewClientFromConnectionString(cfg.ConnectionString, opts)
	}

	var cred azcore.TokenCredential
	var err error

	switch {
	case cfg.UseManagedIdentity:
		cred, err = azidentity.NewManagedIdentityCredential(nil)
	case cfg.ClientID != "" && cfg.ClientSecret != "":
		cred, err = azidentity.NewClientSecretCredential(
			cfg.TenantID, cfg.ClientID, cfg.ClientSecret, nil,
		)
	default:
		cred, err = azidentity.NewDefaultAzureCredential(nil)
	}

	if err != nil {
		return nil, fmt.Errorf("servicebus: credential: %w", err)
	}

	return azservicebus.NewClient(cfg.Namespace, cred, opts)
}

// buildClientOptions returns ClientOptions with a custom TLS
// configuration when the ConnectionConfig requests one, or nil when
// the defaults are sufficient.
func buildClientOptions(cfg ConnectionConfig) *azservicebus.ClientOptions {
	tc := cfg.TLSConfig
	if tc == nil {
		tc = buildTLSConfig(cfg.CaPEM, cfg.InsecureSkipVerify)
	}
	if tc == nil {
		return nil
	}

	return &azservicebus.ClientOptions{
		TLSConfig: tc,
	}
}

// buildTLSConfig constructs a *tls.Config from optional CA PEM data
// and the InsecureSkipVerify flag. Returns nil when neither is set.
func buildTLSConfig(caPEM string, insecureSkipVerify bool) *tls.Config {
	if caPEM == "" && !insecureSkipVerify {
		return nil
	}

	tc := &tls.Config{
		InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // caller-controlled
	}

	if caPEM != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(caPEM))
		tc.RootCAs = pool
	}

	return tc
}
