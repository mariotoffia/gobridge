package domain

import "time"

// DeliveryMode determines how a route handles message ownership transfer.
type DeliveryMode string

const (
	DeliveryDirectHold   DeliveryMode = "direct_hold"
	DeliverySharedOutbox DeliveryMode = "shared_outbox"
)

// DispatchMode determines whether a route sends to one or many destinations.
type DispatchMode string

const (
	DispatchSingle DispatchMode = "single"
	DispatchFanOut DispatchMode = "fan_out"
)

// SessionMode determines session lifecycle and ownership semantics.
type SessionMode string

const (
	SessionEphemeral  SessionMode = "ephemeral"
	SessionPersistent SessionMode = "persistent"
	SessionExclusive  SessionMode = "exclusive"
)

// AckBoundary determines when the source delivery is acknowledged.
type AckBoundary string

const (
	AckAfterTargetAccept  AckBoundary = "target_accept"
	AckAfterOutboxPersist AckBoundary = "outbox_persist"
)

// BackoffPolicy configures retry backoff behavior.
type BackoffPolicy struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
}

// ExpiredAction determines what happens to expired messages.
type ExpiredAction string

const (
	ExpiredDrop ExpiredAction = "drop"
	ExpiredDLQ  ExpiredAction = "dlq"
)

// FailureAction determines what happens on permanent failures.
type FailureAction string

const (
	FailureDLQ  FailureAction = "dlq"
	FailureDrop FailureAction = "drop"
)

// Route policy defaults.
const (
	DefaultMaxInFlight       = 100
	DefaultMaxReplayAttempts = 5
	DefaultMaxOutboxDepth    = 10000
	DefaultSendTimeout       = 30 * time.Second
	DefaultDepthCacheTTL     = 1 * time.Second
	DefaultStepDownGrace     = 15 * time.Second
	DefaultProcessorTimeout  = 30 * time.Second
)

// DefaultBackoffPolicy holds the default backoff configuration.
// Deprecated: use NewDefaultBackoffPolicy() to get an immutable copy.
// This variable is kept for backward compatibility but callers should
// not mutate it.
var DefaultBackoffPolicy = BackoffPolicy{
	InitialInterval: 1 * time.Second,
	MaxInterval:     30 * time.Second,
	Multiplier:      2.0,
}

// NewDefaultBackoffPolicy returns a fresh BackoffPolicy with the
// recommended defaults. Unlike the DefaultBackoffPolicy var, the
// returned value is safe to mutate without global side effects.
func NewDefaultBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{
		InitialInterval: 1 * time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
	}
}

// RoutePolicy defines per-route delivery, retry, and backpressure configuration.
type RoutePolicy struct {
	MaxInFlight          int
	Backoff              BackoffPolicy
	RequireDurableEgress bool
	OnExpired            ExpiredAction
	OnPermanentFailure   FailureAction
	DeliveryMode         DeliveryMode
	DispatchMode         DispatchMode
	AckAfter             AckBoundary
	MaxReplayAttempts    int
	MaxOutboxDepth       int
	AllowUnfenced        bool
	AllowRetryDrop       bool `json:"allow_retry_drop,omitempty" yaml:"allow_retry_drop,omitempty"`
	SendTimeout          time.Duration
	DepthCacheTTL        time.Duration
	// ProcessorTimeout bounds the execution time of a single processor in the
	// chain. Zero means use DefaultProcessorTimeout (30s). A panicking processor
	// returns shared.ErrProcessorPanic (Permanent). A processor that exceeds
	// this deadline returns shared.ErrProcessorTimeout (Transient).
	ProcessorTimeout time.Duration
}

// WithDefaults returns a copy with zero-valued or invalid fields set to defaults.
func (p RoutePolicy) WithDefaults() RoutePolicy {
	if p.MaxInFlight <= 0 {
		p.MaxInFlight = DefaultMaxInFlight
	}
	if p.MaxReplayAttempts <= 0 {
		p.MaxReplayAttempts = DefaultMaxReplayAttempts
	}
	if p.MaxOutboxDepth <= 0 {
		p.MaxOutboxDepth = DefaultMaxOutboxDepth
	}
	defaults := NewDefaultBackoffPolicy()
	if p.Backoff.InitialInterval == 0 {
		p.Backoff.InitialInterval = defaults.InitialInterval
	}
	if p.Backoff.MaxInterval == 0 {
		p.Backoff.MaxInterval = defaults.MaxInterval
	}
	if p.Backoff.Multiplier == 0 {
		p.Backoff.Multiplier = defaults.Multiplier
	}
	if p.OnExpired == "" {
		p.OnExpired = ExpiredDLQ
	}
	if p.OnPermanentFailure == "" {
		p.OnPermanentFailure = FailureDLQ
	}
	if p.DispatchMode == "" {
		p.DispatchMode = DispatchSingle
	}
	if p.DeliveryMode == "" {
		p.DeliveryMode = DeliveryDirectHold
	}
	if p.AckAfter == "" {
		p.AckAfter = AckAfterTargetAccept
	}
	if p.SendTimeout <= 0 {
		p.SendTimeout = DefaultSendTimeout
	}
	if p.DepthCacheTTL <= 0 {
		p.DepthCacheTTL = DefaultDepthCacheTTL
	}
	if p.ProcessorTimeout <= 0 {
		p.ProcessorTimeout = DefaultProcessorTimeout
	}
	return p
}

// DestinationBinding describes a concrete target that a route may send to.
type DestinationBinding struct {
	ID        string
	Transport string
	SessionID string
	SenderID  string
	Address   string
	// Config is the typed plugin config carried from BindingDef.
	// In production this is populated with a value that satisfies
	// ports.PluginConfig; the type assertion happens at the adapter
	// boundary. domain/ does not depend on ports/, so the static
	// type stays as any.
	Config any
	// Headers carries per-binding static header overrides that the
	// runtime merges into outbound DispatchPlan.Headers. These are
	// wire-time annotations (not plugin config) and are typically
	// only populated by tests.
	Headers map[string]any
}

// DispatchPlan is the result of destination resolution for one envelope dispatch.
type DispatchPlan struct {
	BindingID string
	Address   string
	Headers   map[string]any
}

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
type SessionPlan struct {
	Subscriptions []SubscriptionPlan
	Publishers    []PublisherPlan
}

// DLQEntry represents a dead-letter queue record.
type DLQEntry struct {
	ID            string
	Envelope      Envelope
	RouteID       string
	BindingID     string
	SessionID     string
	SourceID      string
	CorrelationID string
	Reason        string
	Category      string
	ErrorCode     string
	LastError     string
	FailedAt      time.Time
	Attempts      int
}

// DLQFilter specifies criteria for querying DLQ entries.
type DLQFilter struct {
	RouteID  string
	Category string
	Since    time.Time
	Before   time.Time
	Limit    int
}
