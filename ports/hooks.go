package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
)

// DeliveryDirection indicates whether a message is entering or leaving
// the bridge.
type DeliveryDirection string

const (
	// DirectionIngress means the message was received from an external
	// source (e.g. SQS, MQTT, HTTP).
	DirectionIngress DeliveryDirection = "ingress"
	// DirectionEgress means the message is being sent to an external
	// destination (e.g. MQTT publish, SQS SendMessage).
	DirectionEgress DeliveryDirection = "egress"
)

// DeliveryAttempt describes a single send or receive attempt. It is
// passed to DeliveryHook.OnAttempt on every try, regardless of outcome.
type DeliveryAttempt struct {
	// Direction is ingress (received) or egress (sent).
	Direction DeliveryDirection
	// RouteID identifies the route processing the message.
	RouteID string
	// BindingID identifies the destination binding (egress only).
	BindingID string
	// Envelope is the message being processed. Hooks must not mutate it.
	Envelope *domain.Envelope
	// Attempt is the 1-based attempt number for this message.
	Attempt int
	// MaxAttempts is the configured maximum replay attempts from the
	// route policy. Zero means unknown or unlimited.
	MaxAttempts int
	// Err is nil on success, non-nil on failure.
	Err error
}

// DeliveryOutcome describes the final disposition of a message. It is
// passed to DeliveryHook.OnSettled exactly once per message when no
// further retries will occur.
type DeliveryOutcome struct {
	// Direction is ingress or egress.
	Direction DeliveryDirection
	// RouteID identifies the route that processed the message.
	RouteID string
	// BindingID identifies the destination binding (egress only).
	BindingID string
	// Envelope is the message that reached its terminal state.
	// Hooks must not mutate it.
	Envelope *domain.Envelope
	// Attempt is the total number of attempts made.
	Attempt int
	// MaxAttempts is the configured maximum from route policy.
	MaxAttempts int
	// Err is nil when the message was delivered successfully.
	// Non-nil indicates permanent failure, DLQ routing, or drop.
	Err error
	// Terminal is always true. It exists so callers can distinguish
	// an OnSettled event from an OnAttempt event in logging code
	// that handles both.
	Terminal bool
}

// DeliveryHook observes message delivery lifecycle events without
// modifying the message or control flow. Implementations must be safe
// for concurrent use; methods may be called from multiple goroutines.
//
// OnAttempt is invoked on every ingress receive or egress send attempt,
// including retries. OnSettled is invoked exactly once when the message
// reaches a terminal state (delivered, DLQ'd, dropped, or expired).
//
// Hooks are called synchronously on the delivery goroutine. A slow hook
// directly increases delivery latency.
type DeliveryHook interface {
	OnAttempt(ctx context.Context, evt DeliveryAttempt)
	OnSettled(ctx context.Context, evt DeliveryOutcome)
}

// NoopDeliveryHook is the default hook that discards all events.
type NoopDeliveryHook struct{}

var _ DeliveryHook = NoopDeliveryHook{}

func (NoopDeliveryHook) OnAttempt(context.Context, DeliveryAttempt) {}
func (NoopDeliveryHook) OnSettled(context.Context, DeliveryOutcome) {}
