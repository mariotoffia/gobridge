package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// SessionSpec holds transport connection identity and remote session
// behavior configuration. Transport-specific settings live on Config,
// the typed PluginConfig produced by the two-stage parser.
type SessionSpec struct {
	ID          string
	Transport   string
	SessionMode connectivity.SessionMode
	Config      PluginConfig
}

// ReceiverSpec holds ingress behavior configuration.
// Transport-specific settings live on Config.
type ReceiverSpec struct {
	ID            string
	SessionID     string
	Subscriptions []connectivity.SubscriptionPlan
	Config        PluginConfig
}

// SenderSpec holds egress behavior configuration.
// Transport-specific settings live on Config.
type SenderSpec struct {
	ID        string
	SessionID string
	Config    PluginConfig
}

// CredentialedConfig is implemented by adapter PluginConfig values
// that participate in the bridge's credential-resolution flow. The
// bridge reads CredentialsURI() before constructing the adapter, calls
// the configured CredentialStore, and then mutates the config in
// place via ApplyCredentials before passing it to the factory.
//
// Implementations must use pointer receivers for ApplyCredentials so
// the mutation is observable on the originally-stored value.
type CredentialedConfig interface {
	// CredentialsURI returns the URI to resolve credentials from, or
	// "" when this attachment point should not consult a credential
	// store.
	CredentialsURI() string
	// ApplyCredentials merges the resolved credential set into the
	// concrete config. Pre-existing inline values must take
	// precedence over resolved material.
	ApplyCredentials(creds *connectivity.CredentialSet) error
}

// SessionFactory creates Session instances for stateful transports.
type SessionFactory interface {
	NewSession(ctx context.Context, spec SessionSpec) (Session, error)
}

// ReceiverFactory creates Receiver instances.
// The session parameter may be nil for stateless transports.
type ReceiverFactory interface {
	NewReceiver(ctx context.Context, spec ReceiverSpec, session Session) (Receiver, error)
}

// SenderFactory creates Sender instances.
// The session parameter may be nil for stateless transports.
type SenderFactory interface {
	NewSender(ctx context.Context, spec SenderSpec, session Session) (Sender, error)
}

// TransportFactory is the port-level contract a transport adapter
// implements so it can be registered with bridge.Builder. It composes
// the per-role factories with a Capabilities query used by the runtime
// validator.
//
// Stateless transports (e.g. SQS, HTTP) return (nil, nil) from
// NewSession. The session parameter passed to NewReceiver/NewSender
// may be nil for stateless transports.
type TransportFactory interface {
	SessionFactory
	ReceiverFactory
	SenderFactory

	// Capabilities returns the transport capabilities relevant for
	// startup validation (e.g. visibility extension, stateful session).
	Capabilities() []Capability
}

// VisibilityTimeoutProvider is an optional interface that
// TransportFactory implementations may satisfy to declare the source
// visibility timeout. The runtime validator uses this value to check
// that SendTimeout does not exceed half the visibility window, which
// would cause duplicate processing.
type VisibilityTimeoutProvider interface {
	VisibilityTimeout() time.Duration
}
