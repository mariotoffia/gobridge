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
)

// Default backoff values.
var DefaultBackoffPolicy = BackoffPolicy{
	InitialInterval: 1 * time.Second,
	MaxInterval:     30 * time.Second,
	Multiplier:      2.0,
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
	SendTimeout          time.Duration
	DepthCacheTTL        time.Duration
}

// WithDefaults returns a copy with zero-valued fields set to defaults.
func (p RoutePolicy) WithDefaults() RoutePolicy {
	if p.MaxInFlight == 0 {
		p.MaxInFlight = DefaultMaxInFlight
	}
	if p.MaxReplayAttempts == 0 {
		p.MaxReplayAttempts = DefaultMaxReplayAttempts
	}
	if p.MaxOutboxDepth == 0 {
		p.MaxOutboxDepth = DefaultMaxOutboxDepth
	}
	if p.Backoff.InitialInterval == 0 {
		p.Backoff.InitialInterval = DefaultBackoffPolicy.InitialInterval
	}
	if p.Backoff.MaxInterval == 0 {
		p.Backoff.MaxInterval = DefaultBackoffPolicy.MaxInterval
	}
	if p.Backoff.Multiplier == 0 {
		p.Backoff.Multiplier = DefaultBackoffPolicy.Multiplier
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
	return p
}

// DestinationBinding describes a concrete target that a route may send to.
type DestinationBinding struct {
	ID        string
	Transport string
	SessionID string
	SenderID  string
	Address   string
	Options   map[string]any
}

// DispatchPlan is the result of destination resolution for one envelope dispatch.
type DispatchPlan struct {
	BindingID string
	Address   string
	Headers   map[string]any
}

// SubscriptionPlan describes a desired subscription in a session.
type SubscriptionPlan struct {
	Topic   string
	QoS     int
	Options map[string]any
}

// PublisherPlan describes a desired publisher in a session.
type PublisherPlan struct {
	Topic   string
	QoS     int
	Options map[string]any
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
