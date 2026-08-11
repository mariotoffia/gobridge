package routing

import (
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// DeliveryMode determines how a route handles message ownership transfer.
type DeliveryMode string

const (
	DeliveryDirectHold   DeliveryMode = "direct_hold"
	DeliverySharedOutbox DeliveryMode = "shared_outbox"
)

// IsValid reports whether m is one of the declared DeliveryMode values.
func (m DeliveryMode) IsValid() bool {
	switch m {
	case DeliveryDirectHold, DeliverySharedOutbox:
		return true
	}
	return false
}

// DispatchMode determines whether a route sends to one or many destinations.
type DispatchMode string

const (
	DispatchSingle DispatchMode = "single"
	DispatchFanOut DispatchMode = "fan_out"
)

// IsValid reports whether m is one of the declared DispatchMode values.
func (m DispatchMode) IsValid() bool {
	switch m {
	case DispatchSingle, DispatchFanOut:
		return true
	}
	return false
}

// AckBoundary determines when the source delivery is acknowledged.
type AckBoundary string

const (
	AckAfterTargetAccept  AckBoundary = "target_accept"
	AckAfterOutboxPersist AckBoundary = "outbox_persist"
)

// IsValid reports whether a is one of the declared AckBoundary values.
func (a AckBoundary) IsValid() bool {
	switch a {
	case AckAfterTargetAccept, AckAfterOutboxPersist:
		return true
	}
	return false
}

// BackoffPolicy configures retry backoff behavior.
type BackoffPolicy struct {
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
	// JitterFactor is the fraction in [0,1] of the computed backoff delay
	// that is randomized to de-correlate retries across competing senders
	// (thundering-herd avoidance). 0 disables jitter and reproduces the
	// deterministic exponential delay; 1 randomizes the full interval.
	// Equal-jitter is applied by route.RetryDelay:
	// delay = d*(1-JitterFactor) + rand[0, d*JitterFactor), clamped to
	// [0, MaxInterval].
	JitterFactor float64
}

// ExpiredAction determines what happens to expired messages.
type ExpiredAction string

const (
	ExpiredDrop ExpiredAction = "drop"
	ExpiredDLQ  ExpiredAction = "dlq"
)

// IsValid reports whether a is one of the declared ExpiredAction values.
func (a ExpiredAction) IsValid() bool {
	switch a {
	case ExpiredDrop, ExpiredDLQ:
		return true
	}
	return false
}

// FailureAction determines what happens on permanent failures.
type FailureAction string

const (
	FailureDLQ  FailureAction = "dlq"
	FailureDrop FailureAction = "drop"
)

// IsValid reports whether a is one of the declared FailureAction values.
func (a FailureAction) IsValid() bool {
	switch a {
	case FailureDLQ, FailureDrop:
		return true
	}
	return false
}

// FilteredAction determines what happens to a message a processor
// intentionally drops via shared.ErrMessageFiltered. It is deliberately
// distinct from OnPermanentFailure: an allow-list filter drop is a routine,
// high-volume outcome (every non-matching message), NOT a fault, so it must
// not inherit the permanent-failure DLQ default and flood the DLQ.
type FilteredAction string

const (
	FilteredDrop FilteredAction = "drop" // drop-with-metric (default)
	FilteredDLQ  FilteredAction = "dlq"  // opt-in: write to the DLQ
)

// IsValid reports whether a is one of the declared FilteredAction values.
func (a FilteredAction) IsValid() bool {
	switch a {
	case FilteredDrop, FilteredDLQ:
		return true
	}
	return false
}

// Route policy defaults.
const (
	DefaultMaxInFlight       = 100
	DefaultMaxReplayAttempts = 5
	DefaultMaxOutboxDepth    = 10000
	DefaultSendTimeout       = 30 * time.Second
	DefaultDepthCacheTTL     = 1 * time.Second
	DefaultStepDownGrace     = 15 * time.Second
	DefaultProcessorTimeout  = 30 * time.Second
	// DefaultReplayBudget is the wall-clock budget, measured from a record's
	// FirstAttemptedAt, before the outbox drainer poisons it to the DLQ. It is
	// the age half of the poison AND-gate; the count half is MaxReplayAttempts.
	DefaultReplayBudget = 15 * time.Minute
)

// NewDefaultBackoffPolicy returns a fresh BackoffPolicy with the
// recommended defaults. The returned value is safe to mutate without
// global side effects.
func NewDefaultBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{
		InitialInterval: 1 * time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		// 20% equal-jitter de-correlates retries across competing senders
		// without materially changing the backoff envelope.
		JitterFactor: 0.2,
	}
}

// RoutePolicy defines per-route delivery, retry, and backpressure configuration.
type RoutePolicy struct {
	MaxInFlight        int
	Backoff            BackoffPolicy
	OnExpired          ExpiredAction
	OnPermanentFailure FailureAction
	// OnFiltered governs a message a processor intentionally drops via
	// shared.ErrMessageFiltered. It defaults to FilteredDrop (drop-with-metric):
	// filtered messages are NOT written to the DLQ unless this is explicitly set
	// to FilteredDLQ. Kept separate from OnPermanentFailure so an allow-list
	// filter cannot DLQ 100% of non-matching traffic just because the
	// permanent-failure sink is the DLQ default.
	OnFiltered        FilteredAction
	DeliveryMode      DeliveryMode
	DispatchMode      DispatchMode
	AckAfter          AckBoundary
	MaxReplayAttempts int
	// ReplayBudget bounds the total wall-clock time, measured from a record's
	// FirstAttemptedAt, that the drainer keeps redelivering before poisoning it
	// to the DLQ. It is the age half of the poison AND-gate (the count half is
	// MaxReplayAttempts). Zero means DefaultReplayBudget (15m): WithDefaults
	// fills it; Validate rejects a negative value. YAML: replay_budget.
	ReplayBudget   time.Duration
	MaxOutboxDepth int
	AllowUnfenced  bool
	AllowRetryDrop bool
	// TrustBridgeHeaders, when true, makes the route preserve the
	// BRIDGE-TO-BRIDGE PROPAGATED reserved headers (correlation-id,
	// causation-id, idempotency-key, dedup-id, ordering-key, tenant-id,
	// forwarded-from/hop) carried by an inbound delivery instead of stripping
	// every x-bridge.* header at ingress. It exists so a receiver fed
	// EXCLUSIVELY by a trusted upstream bridge can continue a
	// correlation/idempotency context across the hop. (W3C trace context —
	// traceparent/tracestate — is NOT x-bridge.*-prefixed, is never stripped,
	// and survives in BOTH modes.)
	//
	// Default false: the safe posture strips all reserved headers so an
	// external producer cannot spoof bridge metadata (e.g. tenant-id).
	// INTERNAL-ONLY headers (route-id, route-override, source-id,
	// content-type) are stripped in BOTH modes — enabling this never lets an
	// external message steer routing.
	TrustBridgeHeaders bool
	SendTimeout        time.Duration
	DepthCacheTTL      time.Duration
	// ProcessorTimeout bounds a single processor's OWN execution time: the
	// budget is disarmed while the processor delegates to next() and rearmed
	// when next() returns, so an outer processor is not charged for time spent
	// in the downstream chain. Zero means use DefaultProcessorTimeout (30s). A
	// panicking processor returns shared.ErrProcessorPanic (Permanent); a
	// processor that overruns its own budget returns shared.ErrProcessorTimeout
	// (Transient).
	ProcessorTimeout time.Duration
}

// WithDefaults returns a copy with zero-valued or invalid fields set to defaults.
// Enum fields that carry an unrecognised value are reset to their default; use
// Validate when callers want to reject invalid input rather than silently
// correct it.
func (p RoutePolicy) WithDefaults() RoutePolicy {
	if p.MaxInFlight <= 0 {
		p.MaxInFlight = DefaultMaxInFlight
	}
	if p.MaxReplayAttempts <= 0 {
		p.MaxReplayAttempts = DefaultMaxReplayAttempts
	}
	if p.ReplayBudget <= 0 {
		p.ReplayBudget = DefaultReplayBudget
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
	if !p.OnExpired.IsValid() {
		p.OnExpired = ExpiredDLQ
	}
	if !p.OnPermanentFailure.IsValid() {
		p.OnPermanentFailure = FailureDLQ
	}
	if !p.OnFiltered.IsValid() {
		p.OnFiltered = FilteredDrop
	}
	if !p.DispatchMode.IsValid() {
		p.DispatchMode = DispatchSingle
	}
	if !p.DeliveryMode.IsValid() {
		p.DeliveryMode = DeliveryDirectHold
	}
	if !p.AckAfter.IsValid() {
		// shared_outbox acks the source once the outbox record is durably
		// persisted, never after the downstream target accepts; default the
		// effective policy to the boundary it actually honors so consumers
		// reading the defaulted policy don't see a target_accept that is never
		// observed. An explicit target_accept on shared_outbox is rejected up
		// front by the route validator; this only governs the unset default.
		if p.DeliveryMode == DeliverySharedOutbox {
			p.AckAfter = AckAfterOutboxPersist
		} else {
			p.AckAfter = AckAfterTargetAccept
		}
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

// Validate reports the first invalid enum value or negative duration carried by
// p as a permanent BridgeError with code shared.ErrCodeInvalidConfig. A
// zero-valued enum or duration is treated as "use default" (handled by
// WithDefaults) and is NOT considered invalid here; only a NEGATIVE ReplayBudget
// is rejected. Callers that want strict rejection of typos like
// RoutePolicy{DeliveryMode: "wat"} should call Validate before (or instead of)
// WithDefaults.
//
// The returned code is shared.ErrCodeInvalidConfig (Permanent), NOT
// ErrCodeInvalidPayload: an invalid route policy is a configuration defect a
// human must fix, semantically distinct from a rejected message payload. This
// keeps ErrCodeInvalidPayload uniquely ErrorRejected (see the code→class
// function test in domain/shared).
func (p RoutePolicy) Validate() error {
	if p.DeliveryMode != "" && !p.DeliveryMode.IsValid() {
		return invalidEnum("DeliveryMode", string(p.DeliveryMode))
	}
	if p.DispatchMode != "" && !p.DispatchMode.IsValid() {
		return invalidEnum("DispatchMode", string(p.DispatchMode))
	}
	if p.AckAfter != "" && !p.AckAfter.IsValid() {
		return invalidEnum("AckAfter", string(p.AckAfter))
	}
	if p.OnExpired != "" && !p.OnExpired.IsValid() {
		return invalidEnum("OnExpired", string(p.OnExpired))
	}
	if p.OnPermanentFailure != "" && !p.OnPermanentFailure.IsValid() {
		return invalidEnum("OnPermanentFailure", string(p.OnPermanentFailure))
	}
	if p.OnFiltered != "" && !p.OnFiltered.IsValid() {
		return invalidEnum("OnFiltered", string(p.OnFiltered))
	}
	if p.ReplayBudget < 0 {
		return invalidDuration("ReplayBudget", p.ReplayBudget)
	}
	// Reject negative Backoff fields. WithDefaults fills only ZERO fields, so
	// a negative survives it. A negative MaxInterval is the dangerous one:
	// route.retryDelay only clamps exponential growth behind a `> 0` MaxInterval
	// guard, so a negative cap never fires and float64 growth reaches
	// time.Duration(+Inf) (implementation-defined, negative on amd64/arm64),
	// feeding Retry a negative/near-infinite delay. InitialInterval and Multiplier
	// are rejected for the same fail-loud-on-bad-config posture.
	if p.Backoff.InitialInterval < 0 {
		return invalidDuration("Backoff.InitialInterval", p.Backoff.InitialInterval)
	}
	if p.Backoff.MaxInterval < 0 {
		return invalidDuration("Backoff.MaxInterval", p.Backoff.MaxInterval)
	}
	if p.Backoff.Multiplier < 0 {
		return invalidFloat("Backoff.Multiplier", p.Backoff.Multiplier)
	}
	return nil
}

func invalidEnum(field, value string) *shared.BridgeError {
	return &shared.BridgeError{
		Code:    shared.ErrCodeInvalidConfig,
		Class:   shared.ErrorPermanent,
		Message: fmt.Sprintf("routing: invalid %s value %q", field, value),
	}
}

func invalidDuration(field string, value time.Duration) *shared.BridgeError {
	return &shared.BridgeError{
		Code:    shared.ErrCodeInvalidConfig,
		Class:   shared.ErrorPermanent,
		Message: fmt.Sprintf("routing: invalid %s value %s (must not be negative)", field, value),
	}
}

func invalidFloat(field string, value float64) *shared.BridgeError {
	return &shared.BridgeError{
		Code:    shared.ErrCodeInvalidConfig,
		Class:   shared.ErrorPermanent,
		Message: fmt.Sprintf("routing: invalid %s value %g (must not be negative)", field, value),
	}
}

// DestinationBinding describes a concrete target that a route may send to.
type DestinationBinding struct {
	ID        string
	Transport string
	SessionID string
	SenderID  string
	// Address is the transport-level destination for this binding
	// (queue URL, topic name, exchange/routing-key, etc.). It is the
	// single destination abstraction propagated to ports.OutboundMessage
	// at send time and is distinct from messaging.Envelope.Subject —
	// the logical message subject — which the runtime must preserve.
	Address string
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
	// Address is the resolved transport-level destination for this
	// dispatch (typically copied or rendered from the selected
	// DestinationBinding.Address). It travels alongside the envelope
	// via ports.OutboundMessage.Address; it is NOT the same as
	// messaging.Envelope.Subject and must not overwrite it.
	Address string
	Headers map[string]any
}
