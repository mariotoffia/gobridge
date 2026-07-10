package ports

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Delivery is a source-owned unit of work. Transport adapters create
// concrete implementations that map Ack/Retry/Extend to transport-native
// operations (e.g., SQS DeleteMessage, ChangeMessageVisibility).
//
// # Settlement state machine (canonical contract)
//
// A Delivery models exactly TWO terminal dispositions: Ack (positive
// settlement — the message was handled, drop it from the source) and
// Retry (negative acknowledgement — return the message for redelivery).
// There is no separate Nack method: Retry IS the nack-with-redelivery
// disposition. Extend is NOT a settlement; it prolongs ownership.
//
// The following rules are NORMATIVE for a conformant Delivery. The conformance
// kit in ports/transporttest asserts the SUCCESSFUL-settlement invariants —
// latched disposition, idempotency + mutual exclusion, and
// ErrNotSupported-never-latches — and ships a reference implementation that
// complies. Error-path behaviour is deliberately transport-specific (see
// "Settle errors" below), so the kit does not assert a single error rule;
// adapters are brought to the successful-settlement invariants via the kit.
//
//   - Unsettled until first SUCCESSFUL settle. A Delivery starts UNSETTLED and
//     stays unsettled until the FIRST successful Ack or Retry.
//
//   - Settle errors: fate unknown, rely on redelivery. A settle call that
//     fails with a non-nil, non-ErrNotSupported error (a broker/network error)
//     leaves the message's fate UNKNOWN. The caller MUST NOT read the error as
//     "the message survived" and MUST rely on the transport's at-least-once
//     redelivery. Whether the SAME handle may be re-settled is
//     transport-specific and each implementation MUST document its choice: a
//     transport whose settle op is safely repeatable (e.g. SQS DeleteMessage /
//     ChangeMessageVisibility) MAY stay UNSETTLED so the caller can re-settle
//     with the same OR a different disposition; a transport where a settle
//     error invalidates the handle (e.g. an AMQP channel death — a dead channel
//     cannot re-ack its delivery tag) MAY latch a terminal settlement-failed
//     state whose later settle calls return an error satisfying
//     errors.Is(err, shared.ErrUnavailable), with the broker redelivering the
//     message as a FRESH Delivery. Neither path loses a message; both forbid a
//     late settle from flipping a disposition the broker already acted on.
//
//   - Latched disposition. The first SUCCESSFUL Ack or Retry transitions the
//     Delivery to SETTLED and FIXES the disposition for its lifetime.
//
//   - Idempotency + mutual exclusion. Once settled, any further settle call —
//     the SAME method or the OTHER one — MUST be a no-op that returns nil and
//     MUST NOT change the broker-side disposition. Concretely:
//     Ack-after-Retry MUST NOT ack/delete the message; Retry-after-Ack MUST
//     NOT redeliver it; a double-Ack (or double-Retry) is a nil no-op. This
//     is the exact message-loss class the contract exists to forbid: a late
//     Ack that deletes a message a prior Retry already returned for
//     redelivery, or a late Retry that resurfaces a message a prior Ack
//     already removed.
//
//   - ErrNotSupported never latches. An operation a transport cannot
//     implement returns an error satisfying errors.Is(err,
//     shared.ErrNotSupported) (code shared.ErrCodeNotSupported) and leaves the
//     delivery UNSETTLED, so a fallback disposition (e.g. the runtime's
//     retry→DLQ fallback) can still settle it through another method.
//
//   - Concurrency. Settle calls may arrive from different goroutines and MAY
//     race each other and Extend; an implementation MUST make the
//     unsettled→settled transition atomic so exactly one disposition wins and
//     the losers observe the settled state as no-ops.
type Delivery interface {
	Envelope() *messaging.Envelope
	// Ack positively settles the delivery: the message was handled and the
	// source may drop it (e.g. SQS DeleteMessage, AMQP Ack). The first
	// SUCCESSFUL Ack latches settlement to the Ack disposition. If the
	// delivery is already settled (by a prior Ack OR Retry), Ack is a nil
	// no-op and MUST NOT delete/ack the message. A broker error here does NOT
	// mean the message survived: the disposition is UNKNOWN and the caller
	// relies on redelivery; whether the handle is re-settleable afterwards is
	// transport-specific (see the "Settle errors" rule above).
	Ack(ctx context.Context) error
	// Retry negatively settles the delivery, returning the message for
	// redelivery after the advisory delay `after` (transports without native
	// delayed redelivery treat it as best-effort and MUST document the loss).
	// `reason` is the classified failure that triggered the retry, for
	// logging/metrics only. The first SUCCESSFUL Retry latches settlement to
	// the Retry disposition. If the delivery is already settled (by a prior
	// Retry OR Ack), Retry is a nil no-op and MUST NOT redeliver the message.
	// A transport with no source-redelivery primitive returns an error
	// satisfying errors.Is(err, shared.ErrNotSupported) WITHOUT latching, so
	// the caller can fall back (typically to DLQ routing).
	Retry(ctx context.Context, after time.Duration, reason error) error
	// Extend prolongs the caller's ownership of an in-flight, UNSETTLED
	// delivery until `until`. `until` is the CALLER's wall-clock instant (see
	// the wall-clock semantics note below): the implementation converts it to
	// the transport's own units (e.g. an SQS visibility timeout of
	// until-now). Extend is NOT a settlement — it never transitions the
	// delivery to settled and may be called repeatedly. A transport with no
	// visibility/lock-extension primitive returns an error satisfying
	// errors.Is(err, shared.ErrNotSupported).
	//
	// Wall-clock semantics: `until` is compared against the same wall clock
	// the broker uses to expire visibility/lock windows. Callers pass an
	// absolute instant, not a duration, so a slow hand-off does not silently
	// shorten the window.
	Extend(ctx context.Context, until time.Time) error
}

// Receiver reads deliveries from a transport and hands each one to the
// emit callback. Run blocks until ctx is cancelled (returning ctx.Err()
// or nil) or an unrecoverable error occurs.
//
// Emit-callback contract. Every Receiver in this repository honours the
// following contract; an implementation MUST document any deviation:
//
//   - Invocation concurrency: emit is invoked SEQUENTIALLY, never
//     concurrently — a delivery is fully handed off (emit returns) before
//     the next is pulled from the transport. The prevailing adapters
//     invoke emit from Run's own goroutine. A Receiver MAY instead emit
//     from a DIFFERENT goroutine (e.g. a broker-client callback goroutine,
//     as the paho MQTT receiver does) PROVIDED it serializes all emissions
//     through a single dispatch worker so the calls remain strictly
//     sequential, and PROVIDED it documents that deviation on its Run
//     method — the default pipeline assumes serial, non-overlapping emit
//     and gives no guarantee under concurrent invocation.
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
//
//   - Settlement ordering: settlement (Ack/Retry/Extend) is invoked
//     through the Delivery handle from the processing pipeline's own
//     goroutines, NOT from the receive loop. The pipeline gives no
//     ordering guarantee relative to emit order — deliveries emitted
//     1, 2 may settle 2 then 1 — and a settlement MAY arrive after Run
//     has returned (e.g. an in-flight send completing during shutdown
//     under a detached drain context). A Receiver MUST tolerate
//     out-of-order settlement, and each Delivery handle MUST remain
//     safe to settle independent of the receive loop's state.
//
//   - Emit lifetime: the Receiver MUST NOT invoke emit after Run has
//     returned. Once Run returns, the runtime releases the resources
//     behind the emit callback; a late emit is a Receiver bug (the
//     runtime guards this defensively and rejects the delivery, which
//     then falls back to transport redelivery).
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
//
// Success boundary. Send returns nil ONLY once the transport has accepted
// the message to the point the source delivery may be settled without loss
// under this transport's durability model: a broker-acknowledged publish
// (MQTT PUBACK/PUBCOMP for QoS>0, AMQP publisher confirm, SQS/Service Bus
// send response, AMQP 1.0 accepted disposition). A nil return MUST NOT mean
// "buffered locally" or "written to the socket" for a durability-bearing
// send — the runtime acks the source on nil (direct_hold) or completes the
// outbox record, so a premature nil is silent message loss by contract.
// Fire-and-forget transports (MQTT QoS0) have no acknowledgement; their nil
// means "handed to the client" and the caller inherits that transport's
// at-most-once semantics — such senders MUST document it.
//
// Context. Send MUST honour ctx: a cancelled/expired ctx returns promptly
// with a ctx error (wrapped) rather than blocking, so shutdown and reconfig
// are not wedged by a hung broker call.
//
// Errors. On failure Send returns a *shared.BridgeError (or an error wrapping
// one) so the runtime can classify transient vs permanent vs rejected and
// apply the route's retry/DLQ policy. A bare, unclassifiable error is treated
// as transient.
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

// NonDurableEgressReporter is an optional interface a Sender may implement to
// declare that its egress path claims an at-least-once delivery guarantee whose
// in-flight packet state is NOT durable across a process crash.
//
// The canonical case is the MQTT (autopaho) Sender at QoS 1/2: the broker
// acknowledgement (PUBACK/PUBCOMP) proves broker acceptance, but the outbound
// packet queue backing that handshake lives in memory, so a publish in flight
// at process death is lost and QoS 2 is not exactly-once across a restart. A
// QoS 0 (best-effort) Sender makes no delivery claim and reports false.
//
// The bridge uses this at build time to reason about egress durability
// PER ROUTE: a non-durable-egress Sender only risks bridge-level message loss
// on a delivery mode that acknowledges the SOURCE before the egress durability
// boundary. Both current modes gate that boundary — direct_hold acks the source
// only after the broker PUBACK/PUBCOMP, and shared_outbox only after a
// version-fenced outbox persist — so a non-durable egress causes no
// bridge-level loss on either. The advisory therefore stays silent today and
// exists to flag a future non-gating delivery mode.
//
// A Sender that does not implement this interface is treated as durable-egress
// (no advisory). Report true only when the delivery-guarantee-vs-durability gap
// genuinely exists.
type NonDurableEgressReporter interface {
	Sender
	// NonDurableEgress reports whether this Sender's accepted-but-in-flight
	// egress can be lost at process crash despite claiming a delivery
	// guarantee (QoS >= 1 for MQTT).
	NonDurableEgress() bool
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

	// CapPlanDrivenSubscriptions marks a transport whose receivers establish
	// their subscriptions ONLY by the session manager reconciling the
	// connectivity.SessionPlan (MQTT/paho, amqp091). For such a transport a
	// receiver bound to a session that never gets a manager is silently inert:
	// nothing reconciles its plan, so it subscribes to nothing. The bridge
	// builder uses this capability to require a session manager for every
	// plan-driven receiver and to FAIL the build otherwise (ADV-P4-FU1).
	//
	// Self-establishing transports (amqp10, whose receivers attach links on
	// start independently of the plan) and address-direct transports
	// (SQS/Service Bus/HTTP, which poll or receive at an address and have no
	// reconcile plan) do NOT advertise it, so the builder skips the check for
	// them — a missing manager is not the same defect there.
	CapPlanDrivenSubscriptions Capability = "plan_driven_subscriptions"
)
