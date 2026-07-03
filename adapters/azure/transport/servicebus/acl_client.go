package servicebus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// asbAPI is the unexported mock-seam interface for the Azure Service
// Bus Receiver SDK. Test doubles implement this; production code uses
// the *azservicebus.Receiver / sessionReceiverAdapter implementations
// constructed via asbClientHandle.
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

// asbSenderAPI is the unexported mock-seam interface for the Azure
// Service Bus Sender SDK.
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

// sessionReceiverAdapter wraps *azservicebus.SessionReceiver to satisfy
// asbAPI. Session receivers use session-level locking
// (RenewSessionLock) rather than per-message locking.
//
// Rule 2 (decorator pass-through): these methods forward to the inner
// SDK verbatim. Classification happens at the asbAPI call sites in
// acl_inbound.go via MapError; wrapping here would double-classify
// and break errors.Is sentinel matching.
type sessionReceiverAdapter struct {
	inner *azservicebus.SessionReceiver
}

var _ asbAPI = (*sessionReceiverAdapter)(nil)

func (a *sessionReceiverAdapter) ReceiveMessages(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
	return a.inner.ReceiveMessages(ctx, count, options) //nolint:wrapcheck // Rule 2 decorator pass-through; classified at caller via MapError
}

func (a *sessionReceiverAdapter) CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.CompleteMessageOptions) error {
	return a.inner.CompleteMessage(ctx, message, options) //nolint:wrapcheck // Rule 2 decorator pass-through
}

func (a *sessionReceiverAdapter) AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.AbandonMessageOptions) error {
	return a.inner.AbandonMessage(ctx, message, options) //nolint:wrapcheck // Rule 2 decorator pass-through
}

func (a *sessionReceiverAdapter) RenewMessageLock(ctx context.Context, _ *azservicebus.ReceivedMessage, _ *azservicebus.RenewMessageLockOptions) error {
	return a.inner.RenewSessionLock(ctx, nil) //nolint:wrapcheck // Rule 2 decorator pass-through
}

func (a *sessionReceiverAdapter) Close(ctx context.Context) error {
	return a.inner.Close(ctx) //nolint:wrapcheck // Rule 2 decorator pass-through
}

// ConnectionConfig holds the credentials and TLS settings shared by
// both Receiver and Sender.
//
// Field tags follow the repo plugin-config convention (snake_case,
// see config_plugin.go): the strict registry decoder (TagName "json",
// ErrorUnused) resolves YAML keys through the json tag, so every
// user-settable field MUST carry one or the documented key is
// rejected as unknown. TLSConfig is programmatic-only (a pre-built
// *tls.Config cannot come from YAML) and is excluded from decoding.
//
// TLS material precedence:
//   - TLSConfig (pre-built *tls.Config) wins when non-nil.
//   - CaPEM / ClientCertPEM / ClientKeyPEM are the PEM-driven source
//     used by ApplyCredentials rotation. When TLSConfig is nil and
//     any PEM field is set, buildClientOptions constructs a fresh
//     *tls.Config from them.
type ConnectionConfig struct {
	ConnectionString   shared.Secret `mapstructure:"connection_string" yaml:"connection_string,omitempty" json:"connection_string,omitempty"`
	Namespace          string        `mapstructure:"namespace" yaml:"namespace,omitempty" json:"namespace,omitempty"`
	UseManagedIdentity bool          `mapstructure:"use_managed_identity" yaml:"use_managed_identity,omitempty" json:"use_managed_identity,omitempty"`
	TenantID           string        `mapstructure:"tenant_id" yaml:"tenant_id,omitempty" json:"tenant_id,omitempty"`
	ClientID           string        `mapstructure:"client_id" yaml:"client_id,omitempty" json:"client_id,omitempty"`
	ClientSecret       shared.Secret `mapstructure:"client_secret" yaml:"client_secret,omitempty" json:"client_secret,omitempty"`
	TLSConfig          *tls.Config   `mapstructure:"-" yaml:"-" json:"-"`
	CaPEM              shared.Secret `mapstructure:"ca_pem" yaml:"ca_pem,omitempty" json:"ca_pem,omitempty"`
	// ClientCertPEM and ClientKeyPEM carry an in-memory client
	// certificate/key pair for mutual TLS. Both must be set; one
	// without the other is a configuration error. They are
	// shared.Secret so the (sensitive) private key — and, for
	// uniformity, the cert/CA bundles — redact on JSON/YAML/log
	// marshal; the config-save path reveals explicitly.
	ClientCertPEM      shared.Secret `mapstructure:"client_cert_pem" yaml:"client_cert_pem,omitempty" json:"client_cert_pem,omitempty"`
	ClientKeyPEM       shared.Secret `mapstructure:"client_key_pem" yaml:"client_key_pem,omitempty" json:"client_key_pem,omitempty"`
	InsecureSkipVerify bool          `mapstructure:"insecure_skip_verify" yaml:"insecure_skip_verify,omitempty" json:"insecure_skip_verify,omitempty"`
}

// asbReceiverOptions carries the receiver-construction knobs in a
// domain-shaped form, so callers in receiver.go never need to touch
// SDK option types.
type asbReceiverOptions struct {
	// ReceiveAndDelete switches the receive mode from PeekLock (the
	// SDK default) to ReceiveAndDelete.
	ReceiveAndDelete bool
	// SubQueue selects an entity sub-queue; "" = none, "deadletter"
	// or "transferdeadletter".
	SubQueue string
}

// asbSessionOptions is the session-receiver counterpart to asbReceiverOptions.
type asbSessionOptions struct {
	ReceiveAndDelete bool
}

// asbClientHandle is the unexported façade over *azservicebus.Client.
// All methods return either domain types or unexported seam interfaces
// (asbAPI, asbSenderAPI, retryScheduler), so consumers outside this
// ACL never reference SDK types and never need to import the Azure
// SDK.
type asbClientHandle struct {
	raw *azservicebus.Client
}

// Close terminates the underlying client connection.
func (h *asbClientHandle) Close(ctx context.Context) error {
	if h == nil || h.raw == nil {
		return nil
	}
	if err := h.raw.Close(ctx); err != nil {
		return fmt.Errorf("servicebus: close client: %w", err)
	}
	return nil
}

// NewSender returns the dual sender/scheduler handle. The underlying
// *azservicebus.Sender satisfies both asbSenderAPI and retryScheduler;
// returning a single value lets callers store it in either field.
func (h *asbClientHandle) NewSender(entity string) (*azservicebus.Sender, error) {
	s, err := h.raw.NewSender(entity, nil)
	if err != nil {
		return nil, fmt.Errorf("servicebus: new sender for %q: %w", entity, err)
	}
	return s, nil
}

// NewReceiverForQueue creates a queue receiver. opts maps onto the
// SDK's ReceiverOptions internally.
func (h *asbClientHandle) NewReceiverForQueue(queue string, opts asbReceiverOptions) (asbAPI, error) {
	r, err := h.raw.NewReceiverForQueue(queue, buildReceiverOptions(opts))
	if err != nil {
		return nil, fmt.Errorf("servicebus: new receiver for queue %q: %w", queue, err)
	}
	return r, nil
}

// NewReceiverForSubscription creates a topic-subscription receiver.
func (h *asbClientHandle) NewReceiverForSubscription(topic, subscription string, opts asbReceiverOptions) (asbAPI, error) {
	r, err := h.raw.NewReceiverForSubscription(topic, subscription, buildReceiverOptions(opts))
	if err != nil {
		return nil, fmt.Errorf("servicebus: new receiver for subscription %q/%q: %w", topic, subscription, err)
	}
	return r, nil
}

// AcceptSessionForQueue accepts a queue session and wraps the resulting
// *azservicebus.SessionReceiver as a sessionReceiverAdapter so callers
// see only the asbAPI shape.
func (h *asbClientHandle) AcceptSessionForQueue(ctx context.Context, queue, sessionID string, opts asbSessionOptions) (asbAPI, error) {
	sr, err := h.raw.AcceptSessionForQueue(ctx, queue, sessionID, buildSessionOptions(opts))
	if err != nil {
		return nil, fmt.Errorf("servicebus: accept session %q on queue %q: %w", sessionID, queue, err)
	}
	return &sessionReceiverAdapter{inner: sr}, nil
}

// AcceptSessionForSubscription accepts a topic-subscription session.
func (h *asbClientHandle) AcceptSessionForSubscription(ctx context.Context, topic, subscription, sessionID string, opts asbSessionOptions) (asbAPI, error) {
	sr, err := h.raw.AcceptSessionForSubscription(ctx, topic, subscription, sessionID, buildSessionOptions(opts))
	if err != nil {
		return nil, fmt.Errorf("servicebus: accept session %q on subscription %q/%q: %w", sessionID, topic, subscription, err)
	}
	return &sessionReceiverAdapter{inner: sr}, nil
}

func buildReceiverOptions(o asbReceiverOptions) *azservicebus.ReceiverOptions {
	out := &azservicebus.ReceiverOptions{}
	if o.ReceiveAndDelete {
		out.ReceiveMode = azservicebus.ReceiveModeReceiveAndDelete
	}
	// Config validation (validateSubQueue) guarantees SubQueue is one of
	// the known values; matching is case-insensitive so an accepted
	// casing can never silently fall through to the main queue.
	switch strings.ToLower(o.SubQueue) {
	case "deadletter":
		out.SubQueue = azservicebus.SubQueueDeadLetter
	case "transferdeadletter":
		out.SubQueue = azservicebus.SubQueueTransfer
	}
	return out
}

func buildSessionOptions(o asbSessionOptions) *azservicebus.SessionReceiverOptions {
	out := &azservicebus.SessionReceiverOptions{}
	if o.ReceiveAndDelete {
		out.ReceiveMode = azservicebus.ReceiveModeReceiveAndDelete
	}
	return out
}

// rawNewAzClient constructs an *asbClientHandle from the connection
// configuration without any domain-error classification. Callers wrap
// the returned error with the sentinel that matches their lifecycle
// phase (cold-init: ErrUnavailable; credential rotation:
// ErrTemporaryAuthFailure) — keeping the chain free of double
// classification so errors.Is matches exactly one domain sentinel.
func rawNewAzClient(cfg ConnectionConfig) (*asbClientHandle, error) {
	opts, err := buildClientOptions(cfg)
	if err != nil {
		return nil, err
	}

	if !cfg.ConnectionString.IsZero() {
		c, err := azservicebus.NewClientFromConnectionString(cfg.ConnectionString.Reveal(), opts)
		if err != nil {
			return nil, fmt.Errorf("servicebus: new client from connection string: %w", err)
		}
		return &asbClientHandle{raw: c}, nil
	}

	var cred azcore.TokenCredential

	switch {
	case cfg.UseManagedIdentity:
		cred, err = azidentity.NewManagedIdentityCredential(nil)
	case cfg.ClientID != "" && !cfg.ClientSecret.IsZero():
		cred, err = azidentity.NewClientSecretCredential(
			cfg.TenantID, cfg.ClientID, cfg.ClientSecret.Reveal(), nil,
		)
	default:
		cred, err = azidentity.NewDefaultAzureCredential(nil)
	}

	if err != nil {
		return nil, fmt.Errorf("servicebus: credential: %w", err)
	}

	c, err := azservicebus.NewClient(cfg.Namespace, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("servicebus: new client for namespace %q: %w", cfg.Namespace, err)
	}
	return &asbClientHandle{raw: c}, nil
}

// buildClient constructs a Service Bus client handle for the cold-init
// code path (Receiver.ensureClient / Sender.ensureClient). It
// classifies any failure with shared.ErrUnavailable so the runtime
// treats the pipeline as transiently unavailable and retries.
//
// For the credential-rotation path (ApplyCredentials), use
// rawNewAzClient directly and classify with
// shared.ErrTemporaryAuthFailure.
func buildClient(cfg ConnectionConfig) (*asbClientHandle, error) {
	client, err := rawNewAzClient(cfg)
	if err != nil {
		return nil, shared.ErrUnavailable.Wrap(err)
	}
	return client, nil
}

// buildClientOptions returns ClientOptions with a custom TLS
// configuration when the ConnectionConfig requests one, or nil when
// the defaults are sufficient.
func buildClientOptions(cfg ConnectionConfig) (*azservicebus.ClientOptions, error) {
	tc := cfg.TLSConfig
	if tc == nil {
		var err error
		tc, err = buildTLSConfig(cfg.CaPEM.Reveal(), cfg.ClientCertPEM.Reveal(), cfg.ClientKeyPEM.Reveal(), cfg.InsecureSkipVerify)
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

// buildTLSConfig constructs a *tls.Config from optional PEM material
// and the InsecureSkipVerify flag. Returns (nil, nil) when nothing is
// set and the SDK default trust store should be used unchanged.
//
// Rules:
//   - CaPEM, when set, replaces the system root pool with a private one.
//   - ClientCertPEM + ClientKeyPEM must be set together or not at all;
//     an imbalanced pair is a config error.
func buildTLSConfig(caPEM, clientCertPEM, clientKeyPEM string, insecureSkipVerify bool) (*tls.Config, error) {
	if caPEM == "" && clientCertPEM == "" && clientKeyPEM == "" && !insecureSkipVerify {
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

	switch {
	case clientCertPEM != "" && clientKeyPEM != "":
		cert, err := tls.X509KeyPair([]byte(clientCertPEM), []byte(clientKeyPEM))
		if err != nil {
			return nil, fmt.Errorf("servicebus: parse client certificate PEM: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	case clientCertPEM != "" || clientKeyPEM != "":
		return nil, fmt.Errorf("servicebus: client certificate PEM requires both ClientCertPEM and ClientKeyPEM")
	}

	return tc, nil
}
