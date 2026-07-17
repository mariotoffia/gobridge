package connectivity

// SessionMode determines session lifecycle and ownership semantics.
type SessionMode string

const (
	SessionEphemeral  SessionMode = "ephemeral"
	SessionPersistent SessionMode = "persistent"
	SessionExclusive  SessionMode = "exclusive"
)

// SubscriptionPlan describes a desired subscription in a session.
type SubscriptionPlan struct {
	Topic string
	QoS   int
	// Config is the typed plugin config attached to the subscription.
	// Adapters type-assert to their own concrete config (e.g.
	// amqp091.Config).
	Config any
}

// PublisherPlan describes a desired publisher in a session.
type PublisherPlan struct {
	Topic string
	QoS   int
	// Config is the typed plugin config attached to the publisher.
	Config any
}

// SessionPlan describes the desired state of a session for reconciliation.
// ExpectedReceiverIDs names the receiver handlers that must be registered before
// a receiver session can report full service. Empty preserves legacy plans.
type SessionPlan struct {
	Subscriptions       []SubscriptionPlan
	Publishers          []PublisherPlan
	ExpectedReceiverIDs []string
}
