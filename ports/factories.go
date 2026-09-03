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

	// ManagedSubscriptionStore carries exact durable topic-filter history for
	// persistent/exclusive sessions. Identity is an opaque secret-safe durable
	// fingerprint. Required is true only when the session must fail closed if
	// history cannot be loaded before broker activation.
	ManagedSubscriptionStore     ManagedSubscriptionStore
	ManagedSubscriptionIdentity  string
	ManagedSubscriptionsRequired bool
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
// validator and an AddressValidator factory used by the runtime to
// validate rendered transport addresses.
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

	// AddressValidator returns a validator the runtime invokes against
	// each fully-rendered transport-level address (e.g. an MQTT publish
	// topic, an AMQP routing key, an SQS queue URL) before handing it
	// to the Sender. Transports without address-level validation
	// requirements MUST return nil; the runtime then skips validation
	// for that binding. Returning a non-nil validator that yields an
	// error from ValidateAddress causes the runtime to surface
	// shared.ErrInvalidTopic at the call site.
	AddressValidator() AddressValidator
}

// AddressValidator validates a fully-rendered transport-level address
// (e.g. an MQTT publish topic, an AMQP routing key, an SQS queue URL)
// before the runtime hands it to a Sender. Returning a non-nil error
// is treated by the runtime as an invalid-topic / invalid-address
// condition (mapped to shared.ErrInvalidTopic at the call site).
//
// Implementations must be safe for concurrent use; the runtime caches
// one validator per transport factory and may invoke it from many
// route-runner goroutines.
type AddressValidator interface {
	ValidateAddress(address string) error
}

// VisibilityTimeoutProvider is an optional interface that
// TransportFactory implementations may satisfy to declare the source
// visibility timeout. The runtime validator uses this value to check
// that SendTimeout does not exceed half the visibility window, which
// would cause duplicate processing.
//
// This reports a transport-WIDE default constant. When a receiver's
// typed PluginConfig also satisfies VisibilityTimeoutConfig, that
// per-route value takes precedence — see VisibilityTimeoutConfig.
type VisibilityTimeoutProvider interface {
	VisibilityTimeout() time.Duration
}

// VisibilityTimeoutConfig is an optional interface a receiver's typed
// PluginConfig may satisfy to declare the route's effective visibility /
// lock window (e.g. SQS visibility_timeout, ASB lock_duration) and
// whether that window is auto-extended while a message is in flight.
// When a receiver config satisfies it, the builder uses this per-route
// window instead of the transport Factory's VisibilityTimeoutProvider
// constant, so the runtime validator checks SendTimeout against the
// window the route will actually run with (Finding 2 /).
//
// AutoExtendEnabled reports whether the receiver renews the window in the
// background (SQS/ASB auto_extend). When true the finite-window
// SendTimeout check does not apply: the source will not redeliver while
// processing continues (barring repeated renewal failure), so a
// deliberately short window paired with auto-extend is a valid config the
// validator must not reject.
type VisibilityTimeoutConfig interface {
	EffectiveVisibilityTimeout() time.Duration
	AutoExtendEnabled() bool
}

// CapabilityConfig is an optional interface a receiver's typed
// PluginConfig may satisfy to declare the SOURCE capabilities the route
// actually runs with, overriding the transport Factory's transport-wide
// Capabilities() constant. Some transports advertise a capability at the
// Factory level that a particular receiver MODE does not implement — e.g.
// Azure Service Bus declares CapVisibilityExtension for PeekLock, but in
// ReceiveAndDelete mode Extend is a no-op and the message is already
// removed on receive, so nothing can redeliver. When a receiver config
// satisfies this interface the builder threads these per-route
// capabilities onto the route, so the runtime validator's silent-drop
// check (a source that cannot retry/redeliver AND no DLQ) sees the honest
// capability set for the mode the route will actually run with instead of
// being blinded by the transport-wide constant.
type CapabilityConfig interface {
	Capabilities() []Capability
}

// SourceRedeliveryConfig is an optional interface a receiver's typed PluginConfig
// may satisfy when whether the SOURCE redelivers an unsettled message is a
// property of the ROUTE rather than of the transport.
//
// CapabilityConfig cannot answer it, because the receiver's own options block
// does not carry the facts: for MQTT the answer depends on the SESSION the
// receiver binds to (a broker session that survives the process is what holds an
// unacknowledged delivery) and on the QoS of the subscriptions the route runs
// with (at-most-once delivery is never repeated). Both are supplied here.
//
// It is what admits an MQTT route to direct_hold. That mode settles the source
// only after the destination has accepted, so its precondition is "the source can
// be left unsettled and will redeliver" — which a QoS 1 subscription on a session
// the broker keeps does provide, and which nothing about visibility windows can
// express.
type SourceRedeliveryConfig interface {
	// SourceRedeliversUnsettled reports whether a delivery this receiver hands the
	// bridge is redelivered when the process dies before settling it. The string is
	// the operator-facing reason it is not, and is meaningful only when the answer
	// is false: it has to name WHICH precondition failed, because the two have
	// different fixes.
	SourceRedeliversUnsettled(session SessionSpec, subscriptions []connectivity.SubscriptionPlan) (bool, string)
}

// IngressMemoryConfig is an optional typed PluginConfig capability for a
// stateful ingress transport whose byte bound depends on route concurrency.
// The bridge invokes it once per ingress session during pure preflight, after
// topology cardinality validation and before opening stores or transports.
type IngressMemoryConfig interface {
	ValidateIngressMemory(routeMaxInFlight uint64) error
}

// IngressMemoryProfileConfig extends IngressMemoryConfig for deployment
// profiles that assign a per-session ingress budget and derive transport
// concurrency from it. Implementations must reject unsafe explicit values
// rather than silently reducing them.
type IngressMemoryProfileConfig interface {
	IngressMemoryConfig
	ConfigureIngressMemory(budgetBytes, routeMaxInFlight uint64) error
}
