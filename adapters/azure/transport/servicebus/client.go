package servicebus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

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

// retryScheduler schedules and cancels messages for future delivery.
// Used by asbDelivery.Retry when a non-zero delay is requested.
type retryScheduler interface {
	ScheduleMessages(ctx context.Context, messages []*azservicebus.Message, scheduledEnqueueTime time.Time, options *azservicebus.ScheduleMessagesOptions) ([]int64, error)
	CancelScheduledMessages(ctx context.Context, sequenceNumbers []int64, options *azservicebus.CancelScheduledMessagesOptions) error
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
	_ asbAPI         = (*azservicebus.Receiver)(nil)
	_ asbSenderAPI   = (*azservicebus.Sender)(nil)
	_ retryScheduler = (*azservicebus.Sender)(nil)
)

// sessionReceiverAdapter wraps *azservicebus.SessionReceiver to
// satisfy asbAPI. Session receivers use session-level locking
// (RenewSessionLock) rather than per-message locking.
type sessionReceiverAdapter struct {
	inner *azservicebus.SessionReceiver
}

var _ asbAPI = (*sessionReceiverAdapter)(nil)

func (a *sessionReceiverAdapter) ReceiveMessages(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
	return a.inner.ReceiveMessages(ctx, count, options)
}

func (a *sessionReceiverAdapter) CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.CompleteMessageOptions) error {
	return a.inner.CompleteMessage(ctx, message, options)
}

func (a *sessionReceiverAdapter) AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.AbandonMessageOptions) error {
	return a.inner.AbandonMessage(ctx, message, options)
}

func (a *sessionReceiverAdapter) RenewMessageLock(ctx context.Context, _ *azservicebus.ReceivedMessage, _ *azservicebus.RenewMessageLockOptions) error {
	return a.inner.RenewSessionLock(ctx, nil)
}

func (a *sessionReceiverAdapter) Close(ctx context.Context) error {
	return a.inner.Close(ctx)
}

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
	opts, err := buildClientOptions(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.ConnectionString != "" {
		return azservicebus.NewClientFromConnectionString(cfg.ConnectionString, opts)
	}

	var cred azcore.TokenCredential

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
func buildClientOptions(cfg ConnectionConfig) (*azservicebus.ClientOptions, error) {
	tc := cfg.TLSConfig
	if tc == nil {
		var err error
		tc, err = buildTLSConfig(cfg.CaPEM, cfg.InsecureSkipVerify)
		if err != nil {
			return nil, err
		}
	}
	if tc == nil {
		return nil, nil
	}

	return &azservicebus.ClientOptions{
		TLSConfig: tc,
	}, nil
}

// buildTLSConfig constructs a *tls.Config from optional CA PEM data
// and the InsecureSkipVerify flag. Returns (nil, nil) when neither is set.
// Returns an error if CaPEM is provided but contains no valid certificates.
func buildTLSConfig(caPEM string, insecureSkipVerify bool) (*tls.Config, error) {
	if caPEM == "" && !insecureSkipVerify {
		return nil, nil
	}

	tc := &tls.Config{
		InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // caller-controlled
	}

	if caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, fmt.Errorf("servicebus: CaPEM contains no valid certificates")
		}
		tc.RootCAs = pool
	}

	return tc, nil
}
