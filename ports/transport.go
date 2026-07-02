package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// Delivery is a source-owned unit of work. Transport adapters create
// concrete implementations that map Ack/Retry/Extend to transport-native
// operations (e.g., SQS DeleteMessage, ChangeMessageVisibility).
type Delivery interface {
	Envelope() *messaging.Envelope
	Ack(ctx context.Context) error
	Retry(ctx context.Context, after time.Duration, reason error) error
	Extend(ctx context.Context, until time.Time) error
}

// Receiver reads deliveries from a transport and hands each one to the
// emit callback. Run blocks until ctx is cancelled (returning ctx.Err()
// or nil) or an unrecoverable error occurs.
//
// Emit-callback contract. Every Receiver in this repository honours the
// following contract; an implementation MUST document any deviation:
//
//   - Invocation concurrency: emit is invoked SEQUENTIALLY from Run's
//     own goroutine, never concurrently. A delivery is fully handed off
//     (emit returns) before the next is pulled from the transport. A
//     Receiver that needs to fan emit out across goroutines MUST
//     document that choice, because the default pipeline assumes serial
//     invocation.
//
//   - emit returns a non-nil error: the runtime could not accept the
//     delivery for processing. The Receiver MUST NOT treat the delivery
//     as settled — it MUST NOT Ack/delete it — and leaves it to the
//     transport's redelivery/visibility policy (AMQP requeue on channel
//     close, SQS visibility-timeout expiry, Service Bus lock expiry).
//     The prevailing adapters surface the failure by returning it from
//     Run so the supervisor can re-establish the receive loop.
//
//   - emit returns nil: ownership of the delivery transfers to the
//     processing pipeline. Settlement (Ack/Retry/Extend) is from then on
//     performed exclusively through the Delivery handle by that pipeline;
//     the Receiver MUST NOT settle the delivery itself. (AMQP 0-9-1
//     AutoAck mode, where the broker settles at delivery time, is the
//     documented exception.) The receive loop simply advances to the
//     next delivery.
//
//   - Backpressure: because emit is synchronous and serial, the Receiver
//     throttles itself — it does not read ahead past an in-flight
//     delivery. Transport prefetch / visibility windows (AMQP
//     PrefetchCount, SQS in-flight limit) bound the working set; a
//     Receiver MUST NOT buffer deliveries unboundedly ahead of emit.
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

// OutboundMessage carries an envelope together with the concrete
// transport destination chosen by the runtime for this dispatch.
//
// Address is the resolved transport-level destination (e.g. an MQTT
// publish topic, an AMQP 0-9-1 routing key, an AMQP 1.0 link target
// address, an SQS queue URL/name, or an Azure Service Bus entity name).
// Envelope.Subject remains the logical event subject and MUST NOT be
// mutated to express the transport address.
type OutboundMessage struct {
	// Envelope is the logical message being dispatched.
	Envelope *messaging.Envelope
	// Address is the concrete transport destination for this dispatch.
	// An empty Address means "use the adapter's default destination".
	Address string
}

// Sender submits a single OutboundMessage to a transport.
//
// OutboundMessage.Address is the transport destination for this dispatch;
// OutboundMessage.Envelope.Subject is the logical event subject and is
// distinct from the transport address.
type Sender interface {
	Send(ctx context.Context, msg OutboundMessage) error
}

// BatchResult reports the outcome of one message in a SendBatch call,
// keyed by its index in the input slice. Err is nil on success.
type BatchResult struct {
	Index int
	Err   error
}

// BatchSender extends Sender with batch send capability for transports
// that support it (e.g., SQS SendMessageBatch).
//
// Each OutboundMessage in the batch carries its own Address (transport
// destination) alongside the logical Envelope.Subject.
//
// SendBatch result contract. The returned error is non-nil ONLY for a
// whole-batch failure where per-message attribution is impossible: a
// client/connection setup failure, or a fail-fast pre-validation that
// rejects the entire batch before any message is dispatched (e.g. a nil
// envelope, or an Address that does not match the sender's destination).
// In that case SendBatch returns (nil, err) and nothing was sent.
//
// Once the batch is dispatched, SendBatch returns (results, nil) with
// len(results) == len(msgs); results is index-aligned with msgs and each
// BatchResult carries either a nil Err (that message succeeded) or the
// per-message failure. Callers count successes as the number of nil-Err
// results. An empty input returns (nil-or-empty, nil).
type BatchSender interface {
	Sender
	SendBatch(ctx context.Context, msgs []OutboundMessage) ([]BatchResult, error)
}

// AddressValidatingSender is an optional interface a Sender may implement to
// validate a STATIC binding address when the bridge is built, so a
// misconfigured address fails fast at build time rather than at first send.
//
// This is distinct from the factory-level AddressValidator: that validates
// resolved addresses per-dispatch, whereas this checks a binding's literal
// configured address against the sender's own bound destination.
//
// The builder calls ValidateAddress only for non-empty, non-templated binding
// addresses — an address containing a "{key}" placeholder is rendered per
// message (see runtime/route.RenderAddress) and cannot be checked statically.
// A non-nil error fails the build. A Sender that cannot decide statically
// (e.g. the destination is only known at connect time) must return nil.
type AddressValidatingSender interface {
	Sender
	ValidateAddress(address string) error
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
	Reconcile(ctx context.Context, plan connectivity.SessionPlan) error
	Health(ctx context.Context) SessionHealth
	Events() <-chan SessionEvent
	Close(ctx context.Context) error
}

// Lease manages cluster ownership for single-active scenarios.
type Lease interface {
	Acquire(ctx context.Context) (persistence.LeaseToken, error)
	Renew(ctx context.Context, token persistence.LeaseToken) (persistence.LeaseToken, error)
	Release(ctx context.Context, token persistence.LeaseToken) error
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
