package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// Delivery is a source-owned unit of work. Transport adapters create
// concrete implementations that map Ack/Retry/Extend to transport-native
// operations (e.g., SQS DeleteMessage, ChangeMessageVisibility).
type Delivery interface {
	Envelope() *domain.Envelope
	Ack(ctx context.Context) error
	Retry(ctx context.Context, after time.Duration, reason error) error
	Extend(ctx context.Context, until time.Time) error
}

// Receiver reads deliveries from a transport. The emit callback is invoked
// for each received delivery; Run blocks until the context is cancelled or
// an unrecoverable error occurs.
type Receiver interface {
	Run(ctx context.Context, emit func(context.Context, Delivery) error) error
}

// ReceiverStartedSignaler is an optional interface. Receivers that
// expose a readiness signal close the returned channel once their
// receive loop is live and ready to process messages. Callers
// type-assert to use it; the channel is initialized at construction
// time so it is safe to read even before Run has been called (the read
// simply blocks until the receiver becomes ready).
type ReceiverStartedSignaler interface {
	Started() <-chan struct{}
}

// Sender submits envelopes to a transport.
type Sender interface {
	Send(ctx context.Context, env *domain.Envelope) error
}

// BatchSender extends Sender with batch send capability for transports
// that support it (e.g., SQS SendMessageBatch).
type BatchSender interface {
	Sender
	SendBatch(ctx context.Context, envs []*domain.Envelope) (int, error)
}

// SessionEventType classifies session lifecycle events.
type SessionEventType int

const (
	SessionConnected SessionEventType = iota
	SessionDisconnected
	SessionReconnecting
	SessionError
	SessionReconciled // all subscriptions re-established after reconnect
)

// SessionEvent is a lifecycle notification from a stateful session.
type SessionEvent struct {
	Type      SessionEventType
	Err       error
	Timestamp time.Time
}

// ServiceLevel describes the operational completeness of a session's
// subscription and handler state.
type ServiceLevel string

const (
	// ServiceLevelNone indicates no subscriptions are active and no
	// handlers are registered, or the session is not connected.
	ServiceLevelNone ServiceLevel = "none"
	// ServiceLevelDegraded indicates the session is connected but not
	// all desired subscriptions are active on the broker.
	ServiceLevelDegraded ServiceLevel = "degraded"
	// ServiceLevelFull indicates all desired subscriptions are active
	// and handlers are registered (when subscriptions are expected).
	ServiceLevelFull ServiceLevel = "full"
)

// SessionHealth describes the current health state of a session.
// Transports that manage subscriptions (e.g., MQTT) should populate
// the subscription and handler fields so callers can determine readiness.
type SessionHealth struct {
	Connected           bool
	LastError           error
	SubscriptionsWanted int          // Number of subscriptions in the reconciled plan
	SubscriptionsActive int          // Number of subscriptions confirmed by broker
	HandlersRegistered  int          // Number of receiver handlers on the message router
	ReceiveMaximum      uint16       // MQTT v5 ReceiveMaximum (0 = unknown/not applicable)
	Ready               bool         // Connected to the broker (connectivity only)
	ServiceLevel        ServiceLevel // Operational completeness (none/degraded/full)
	ActiveTopics        []string     // topics with active broker subscription
}

// HasTopic reports whether the given topic is among the active subscriptions.
func (h SessionHealth) HasTopic(topic string) bool {
	for _, t := range h.ActiveTopics {
		if t == topic {
			return true
		}
	}
	return false
}

// Session owns network identity and remote state for stateful transports.
// Stateless transports do not require a Session.
type Session interface {
	Start(ctx context.Context) error
	Reconcile(ctx context.Context, plan domain.SessionPlan) error
	Health(ctx context.Context) SessionHealth
	Events() <-chan SessionEvent
	Close(ctx context.Context) error
}

// Lease manages cluster ownership for single-active scenarios.
type Lease interface {
	Acquire(ctx context.Context) (domain.LeaseToken, error)
	Renew(ctx context.Context, token domain.LeaseToken) (domain.LeaseToken, error)
	Release(ctx context.Context, token domain.LeaseToken) error
	Owner() string
}

// Capability describes a routing-relevant transport feature.
type Capability string

const (
	CapStatefulSession     Capability = "stateful_session"
	CapSourceRedelivery    Capability = "source_redelivery"
	CapVisibilityExtension Capability = "visibility_extension"
	CapDelayedSend         Capability = "delayed_send"
	CapSharedConsumer      Capability = "shared_consumer"
	CapExclusiveIdentity   Capability = "exclusive_identity"
	CapHTTPEndpoint        Capability = "http_endpoint"
)
