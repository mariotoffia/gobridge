package shared

import (
	"errors"
	"fmt"
	"maps"
	"time"
)

// ErrorClass classifies errors for routing decisions in the runtime.
type ErrorClass string

const (
	ErrorTransient ErrorClass = "transient"
	ErrorPermanent ErrorClass = "permanent"
	ErrorExpired   ErrorClass = "expired"
	ErrorRejected  ErrorClass = "rejected"
)

// ErrorCode uniquely identifies an error type for sentinel comparison.
type ErrorCode string

// Recoverable error codes -- may succeed on retry.
const (
	ErrCodeTimeout              ErrorCode = "TIMEOUT"
	ErrCodeConnectionLost       ErrorCode = "CONNECTION_LOST"
	ErrCodeUnavailable          ErrorCode = "UNAVAILABLE"
	ErrCodeThrottled            ErrorCode = "THROTTLED"
	ErrCodeBrokerBusy           ErrorCode = "BROKER_BUSY"
	ErrCodeTemporaryAuthFailure ErrorCode = "TEMPORARY_AUTH_FAILURE"
	ErrCodeTenantQuotaExceeded  ErrorCode = "TENANT_QUOTA_EXCEEDED"
)

// Permanent error codes -- retry will not help.
const (
	ErrCodeNotAuthorized ErrorCode = "NOT_AUTHORIZED"
	ErrCodeForbidden     ErrorCode = "FORBIDDEN"
	ErrCodeNotFound      ErrorCode = "NOT_FOUND"
	// ErrCodeInvalidPayload flags a rejected MESSAGE payload — malformed
	// or non-conforming wire data on an in-flight envelope. It is uniquely
	// classified ErrorRejected (drop, no DLQ). Config-validation failures
	// (invalid enum / negative duration in a blueprint) use the distinct
	// ErrCodeInvalidConfig so a single code never carries two classes;
	// BridgeError.Is matches on Code alone, so overloading one code with
	// two classes would make classification order-dependent.
	ErrCodeInvalidPayload ErrorCode = "INVALID_PAYLOAD"
	// ErrCodeInvalidConfig flags a CONFIG-validation failure: an invalid
	// enum value or a negative duration carried by a blueprint/policy. It
	// is always Permanent (a human must fix the configuration; retry never
	// helps) and is deliberately distinct from ErrCodeInvalidPayload, which
	// is a rejected message payload. Keeping them separate preserves the
	// code→class function (every code maps to exactly one ErrorClass).
	ErrCodeInvalidConfig   ErrorCode = "INVALID_CONFIG"
	ErrCodePayloadTooLarge ErrorCode = "PAYLOAD_TOO_LARGE"
	ErrCodeInvalidTopic    ErrorCode = "INVALID_TOPIC"
	ErrCodeProtocolError   ErrorCode = "PROTOCOL_ERROR"
	ErrCodeSchemaViolation ErrorCode = "SCHEMA_VIOLATION"
	ErrCodeMessageExpired  ErrorCode = "MESSAGE_EXPIRED"
	ErrCodeQoSNotSupported ErrorCode = "QOS_NOT_SUPPORTED"
	ErrCodeMessageFiltered ErrorCode = "MESSAGE_FILTERED"
)

// Infrastructure and fencing error codes.
const (
	ErrCodeNotSupported      ErrorCode = "NOT_SUPPORTED"
	ErrCodeVersionMismatch   ErrorCode = "VERSION_MISMATCH"
	ErrCodeAlreadyExists     ErrorCode = "ALREADY_EXISTS"
	ErrCodeStaleFencingToken ErrorCode = "STALE_FENCING_TOKEN"
	ErrCodeDuplicateRecord   ErrorCode = "DUPLICATE_RECORD"
	ErrCodeTransportClosed   ErrorCode = "TRANSPORT_CLOSED"
)

// Cluster routing error codes.
const (
	ErrCodeNoRouteOwner  ErrorCode = "NO_ROUTE_OWNER"
	ErrCodeForwardFailed ErrorCode = "FORWARD_FAILED"
)

// Runtime error codes for pipeline classification.
const (
	ErrCodeNoBindingMatch   ErrorCode = "NO_BINDING_MATCH"
	ErrCodePoisonMessage    ErrorCode = "POISON_MESSAGE"
	ErrCodeProcessorPanic   ErrorCode = "PROCESSOR_PANIC"
	ErrCodeProcessorTimeout ErrorCode = "PROCESSOR_TIMEOUT"
	// ErrCodeInternal flags a programmer / invariant-violation error
	// that should never occur in a correctly-wired bridge (e.g. an
	// adapter forgot to inject its clock, an envelope constructor
	// returned a sentinel that signals a missing dependency rather
	// than bad input). Always Permanent and never recoverable.
	ErrCodeInternal ErrorCode = "INTERNAL"
)

// Outbox aggregate state-machine error codes.
const (
	ErrCodeInvalidOutboxRecord     ErrorCode = "INVALID_OUTBOX_RECORD"
	ErrCodeOutboxNotClaimable      ErrorCode = "OUTBOX_NOT_CLAIMABLE"
	ErrCodeOutboxNotInClaimedState ErrorCode = "OUTBOX_NOT_IN_CLAIMED_STATE"
	ErrCodeOutboxAlreadyTerminal   ErrorCode = "OUTBOX_ALREADY_TERMINAL"
	ErrCodeOutboxNotPending        ErrorCode = "OUTBOX_NOT_PENDING"
)

// Cluster rollout aggregate state-machine error codes.
const (
	ErrCodeInvalidRolloutProposal ErrorCode = "INVALID_ROLLOUT_PROPOSAL"
	ErrCodeRolloutNotCommittable  ErrorCode = "ROLLOUT_NOT_COMMITTABLE"
	ErrCodeRolloutTerminal        ErrorCode = "ROLLOUT_TERMINAL"
	ErrCodeRolloutAckRejected     ErrorCode = "ROLLOUT_ACK_REJECTED"
	ErrCodeRolloutDigestMismatch  ErrorCode = "ROLLOUT_DIGEST_MISMATCH"
	ErrCodeRolloutNotConfirmable  ErrorCode = "ROLLOUT_NOT_CONFIRMABLE"
)

// BridgeError is the structured error type for the bridge.
// Transport adapters and runtime components return BridgeError instances
// so the pipeline can classify failures for retry, DLQ, or drop decisions.
type BridgeError struct {
	Code       ErrorCode
	Class      ErrorClass
	Message    string
	Cause      error
	RetryAfter time.Duration
	Context    map[string]any
}

func (e *BridgeError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *BridgeError) Unwrap() error {
	return e.Cause
}

// Is enables errors.Is() comparison by error code.
func (e *BridgeError) Is(target error) bool {
	t, ok := target.(*BridgeError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// With returns a copy with additional context.
func (e *BridgeError) With(key string, value any) *BridgeError {
	clone := e.clone()
	if clone.Context == nil {
		clone.Context = make(map[string]any, 1)
	}
	clone.Context[key] = value
	return clone
}

// WithMessage returns a copy with a custom message.
func (e *BridgeError) WithMessage(msg string) *BridgeError {
	clone := e.clone()
	clone.Message = msg
	return clone
}

// Wrap returns a copy with the cause set.
func (e *BridgeError) Wrap(err error) *BridgeError {
	clone := e.clone()
	clone.Cause = err
	return clone
}

// WithRetryAfter returns a copy with a retry delay hint.
func (e *BridgeError) WithRetryAfter(d time.Duration) *BridgeError {
	clone := e.clone()
	clone.RetryAfter = d
	return clone
}

func (e *BridgeError) clone() *BridgeError {
	c := *e
	if e.Context != nil {
		c.Context = maps.Clone(e.Context)
	}
	return &c
}

// Sentinel errors -- recoverable (transient).
var (
	ErrTimeout = &BridgeError{
		Code: ErrCodeTimeout, Class: ErrorTransient,
		Message: "request timed out",
	}
	ErrConnectionLost = &BridgeError{
		Code: ErrCodeConnectionLost, Class: ErrorTransient,
		Message: "connection lost",
	}
	ErrUnavailable = &BridgeError{
		Code: ErrCodeUnavailable, Class: ErrorTransient,
		Message: "service unavailable",
	}
	ErrThrottled = &BridgeError{
		Code: ErrCodeThrottled, Class: ErrorTransient,
		Message: "rate limited",
	}
	ErrBrokerBusy = &BridgeError{
		Code: ErrCodeBrokerBusy, Class: ErrorTransient,
		Message: "broker overloaded",
	}
	ErrTemporaryAuthFailure = &BridgeError{
		Code: ErrCodeTemporaryAuthFailure, Class: ErrorTransient,
		Message: "authentication temporarily failed",
	}
	// ErrTenantQuotaExceeded signals that a tenant has hit a usage ceiling
	// (e.g. its max in-flight deliveries). Transient by design: the tenant
	// becomes deliverable again as its in-flight count drains, so the route
	// retry policy is the correct pressure valve rather than a DLQ.
	ErrTenantQuotaExceeded = &BridgeError{
		Code: ErrCodeTenantQuotaExceeded, Class: ErrorTransient,
		Message: "tenant quota exceeded",
	}
)

// Sentinel errors -- permanent.
var (
	ErrNotAuthorized = &BridgeError{
		Code: ErrCodeNotAuthorized, Class: ErrorPermanent,
		Message: "not authorized",
	}
	ErrForbidden = &BridgeError{
		Code: ErrCodeForbidden, Class: ErrorPermanent,
		Message: "forbidden",
	}
	ErrNotFound = &BridgeError{
		Code: ErrCodeNotFound, Class: ErrorPermanent,
		Message: "not found",
	}
	// ErrInvalidConfig is the sentinel for a CONFIG-validation failure
	// (invalid enum value, negative duration, or an otherwise malformed
	// blueprint/policy field). It is Permanent: retry cannot help, a human
	// must correct the configuration. It is deliberately distinct from
	// ErrInvalidPayload (a rejected MESSAGE payload) so the INVALID_PAYLOAD
	// code stays uniquely ErrorRejected and INVALID_CONFIG stays uniquely
	// ErrorPermanent — see the code→class function test in
	// domain/shared/errorclass_function_test.go.
	ErrInvalidConfig = &BridgeError{
		Code: ErrCodeInvalidConfig, Class: ErrorPermanent,
		Message: "invalid configuration",
	}
	ErrInvalidPayload = &BridgeError{
		Code: ErrCodeInvalidPayload, Class: ErrorRejected,
		Message: "invalid payload",
	}
	ErrPayloadTooLarge = &BridgeError{
		Code: ErrCodePayloadTooLarge, Class: ErrorRejected,
		Message: "payload too large",
	}
	ErrInvalidTopic = &BridgeError{
		Code: ErrCodeInvalidTopic, Class: ErrorRejected,
		Message: "invalid topic",
	}
	ErrProtocolError = &BridgeError{
		Code: ErrCodeProtocolError, Class: ErrorPermanent,
		Message: "protocol error",
	}
	ErrSchemaViolation = &BridgeError{
		Code: ErrCodeSchemaViolation, Class: ErrorRejected,
		Message: "schema validation failed",
	}
	ErrMessageExpired = &BridgeError{
		Code: ErrCodeMessageExpired, Class: ErrorExpired,
		Message: "message expired",
	}
	ErrQoSNotSupported = &BridgeError{
		Code: ErrCodeQoSNotSupported, Class: ErrorPermanent,
		Message: "QoS level not supported",
	}
	ErrMessageFiltered = &BridgeError{
		Code: ErrCodeMessageFiltered, Class: ErrorRejected,
		Message: "message filtered",
	}
)

// Sentinel errors -- infrastructure / fencing.
var (
	ErrNotSupported = &BridgeError{
		Code: ErrCodeNotSupported, Class: ErrorPermanent,
		Message: "operation not supported",
	}
	ErrVersionMismatch = &BridgeError{
		Code: ErrCodeVersionMismatch, Class: ErrorPermanent,
		Message: "version mismatch",
	}
	ErrAlreadyExists = &BridgeError{
		Code: ErrCodeAlreadyExists, Class: ErrorPermanent,
		Message: "resource already exists",
	}
	ErrStaleFencingToken = &BridgeError{
		Code: ErrCodeStaleFencingToken, Class: ErrorPermanent,
		Message: "stale fencing token",
	}
	ErrDuplicateRecord = &BridgeError{
		Code: ErrCodeDuplicateRecord, Class: ErrorPermanent,
		Message: "duplicate record",
	}
	// ErrTransportClosedPermanently is a definitive "this transport instance is
	// closed for good" marker. Single-use exclusive transports (paho MQTT, AMQP
	// 0-9-1, AMQP 1.0) cannot be Started again after Close: the receiver/sender
	// still reference the dead instance and no in-process rebuild path exists.
	// They surface that as the transient ErrUnavailable (so first-connect broker
	// blips stay retriable) but WRAP this permanent marker as the cause, so
	// escalation logic can tell a definitive Start-after-Close from a momentary
	// "broker unreachable". It is only ever a wrapped cause — the outermost error
	// stays ErrUnavailable, so failure CLASSIFICATION is unchanged; discover it
	// with errors.Is(err, ErrTransportClosedPermanently), never by class.
	ErrTransportClosedPermanently = &BridgeError{
		Code: ErrCodeTransportClosed, Class: ErrorPermanent,
		Message: "transport closed permanently",
	}
)

// Sentinel errors -- cluster routing.
var (
	ErrNoRouteOwner = &BridgeError{
		Code: ErrCodeNoRouteOwner, Class: ErrorTransient,
		Message: "no instance owns this route",
	}
	ErrForwardFailed = &BridgeError{
		Code: ErrCodeForwardFailed, Class: ErrorTransient,
		Message: "cluster forward failed",
	}
)

// Sentinel errors -- processor chain.
var (
	// ErrProcessorPanic indicates a processor panicked during execution.
	// Panics are treated as bugs (Permanent) and route to DLQ rather than retry.
	ErrProcessorPanic = &BridgeError{
		Code: ErrCodeProcessorPanic, Class: ErrorPermanent,
		Message: "processor panicked",
	}
	// ErrProcessorTimeout indicates a processor exceeded its configured timeout.
	// Treated as Transient -- may succeed on retry.
	ErrProcessorTimeout = &BridgeError{
		Code: ErrCodeProcessorTimeout, Class: ErrorTransient,
		Message: "processor timed out",
	}
)

// Sentinel errors -- outbox aggregate state-machine.
var (
	// ErrInvalidOutboxRecord indicates an attempt to construct an OutboxRecord
	// without the required identity fields. Permanent: callers must fix inputs.
	ErrInvalidOutboxRecord = &BridgeError{
		Code: ErrCodeInvalidOutboxRecord, Class: ErrorPermanent,
		Message: "invalid outbox record",
	}
	// ErrOutboxNotClaimable indicates a Claim call against a record whose
	// state does not permit claiming (terminal or under a newer fencing
	// token). Permanent: retry will not help without external state change.
	ErrOutboxNotClaimable = &BridgeError{
		Code: ErrCodeOutboxNotClaimable, Class: ErrorPermanent,
		Message: "outbox record is not claimable",
	}
	// ErrOutboxNotInClaimedState indicates Complete was invoked on a record
	// that is not currently in the Claimed state.
	ErrOutboxNotInClaimedState = &BridgeError{
		Code: ErrCodeOutboxNotInClaimedState, Class: ErrorPermanent,
		Message: "outbox record is not in claimed state",
	}
	// ErrOutboxAlreadyTerminal indicates Expire was invoked on a record
	// that has already reached a terminal state (Completed or Expired).
	ErrOutboxAlreadyTerminal = &BridgeError{
		Code: ErrCodeOutboxAlreadyTerminal, Class: ErrorPermanent,
		Message: "outbox record is already in a terminal state",
	}
	// ErrOutboxNotPending indicates Expire was invoked on a record that is
	// not Pending (e.g. Claimed). Expire is pending-only: a claimed record
	// is reclaimed through Claim/stale-reclaim, never expired out from under
	// a potentially still-live owner (see ports.OutboxStore.Expire).
	ErrOutboxNotPending = &BridgeError{
		Code: ErrCodeOutboxNotPending, Class: ErrorPermanent,
		Message: "outbox record is not pending",
	}
)

// Sentinel errors -- cluster rollout aggregate state-machine.
var (
	// ErrInvalidRolloutProposal indicates an attempt to open a rollout with a
	// malformed proposal (zero generation, empty proposer/digest, no members,
	// or a duplicate/empty member id). Permanent: callers must fix inputs.
	ErrInvalidRolloutProposal = &BridgeError{
		Code: ErrCodeInvalidRolloutProposal, Class: ErrorPermanent,
		Message: "invalid rollout proposal",
	}
	// ErrRolloutNotCommittable indicates Commit was invoked on a rollout that
	// is not in Staging with every membership-epoch member acked (invariant
	// the all-member barrier). Permanent: the coordinator must gather the
	// remaining acks or abort before it can commit.
	ErrRolloutNotCommittable = &BridgeError{
		Code: ErrCodeRolloutNotCommittable, Class: ErrorPermanent,
		Message: "rollout is not committable: staging with all acks required",
	}
	// ErrRolloutTerminal indicates a state transition (ack, commit, abort) was
	// invoked against a rollout that has already reached a terminal state in a
	// direction that cannot be reconciled (terminal-immutable):
	// acking a decided rollout, committing an aborted one, or aborting a
	// committed one. Permanent. A same-direction re-decision with a live
	// fencing token is instead an idempotent no-op, not this error.
	ErrRolloutTerminal = &BridgeError{
		Code: ErrCodeRolloutTerminal, Class: ErrorPermanent,
		Message: "rollout is already in a terminal state",
	}
	// ErrRolloutAckRejected indicates an Ack or Nack that violates the barrier
	// bookkeeping: a member outside the frozen membership epoch,
	// a second vote from a member that already acked or nacked, or an ack with
	// an empty build digest. Permanent: retry with the same inputs will not
	// help.
	ErrRolloutAckRejected = &BridgeError{
		Code: ErrCodeRolloutAckRejected, Class: ErrorPermanent,
		Message: "rollout ack/nack rejected",
	}
	// ErrRolloutDigestMismatch indicates that candidate config bytes fetched by
	// a member do not match the digest recorded in the rollout row, or that the
	// row carries no digest to verify against. Permanent: the member
	// Nacks rather than build unverified or substituted bytes.
	ErrRolloutDigestMismatch = &BridgeError{
		Code: ErrCodeRolloutDigestMismatch, Class: ErrorPermanent,
		Message: "rollout candidate digest mismatch",
	}
	// ErrRolloutNotConfirmable indicates Confirm was invoked on a committed
	// rollout whose confirm window (design §8.1) is not satisfiable as confirmed:
	// the window is inactive (base protocol, confirm_window == 0), or not every
	// membership-epoch member has recorded convergence yet (the
	// all-member confirm barrier). Permanent for this observation: the coordinator
	// waits for the remaining Converge records or reverts on the confirm deadline.
	ErrRolloutNotConfirmable = &BridgeError{
		Code: ErrCodeRolloutNotConfirmable, Class: ErrorPermanent,
		Message: "rollout is not confirmable: committed with all members converged required",
	}
)

// NewBridgeError creates a BridgeError with the given parameters.
func NewBridgeError(code ErrorCode, class ErrorClass, message string) *BridgeError {
	return &BridgeError{Code: code, Class: class, Message: message}
}

// AsBridgeError extracts a *BridgeError from an error chain.
func AsBridgeError(err error) (*BridgeError, bool) {
	var be *BridgeError
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}

// IsRecoverableError returns true if the error is transient.
// Unknown error types are treated as recoverable (safe default).
func IsRecoverableError(err error) bool {
	if err == nil {
		return false
	}
	be, ok := AsBridgeError(err)
	if !ok {
		return true
	}
	return be.Class == ErrorTransient
}

// GetRetryAfter extracts the RetryAfter hint from an error chain.
func GetRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	be, ok := AsBridgeError(err)
	if !ok {
		return 0
	}
	return be.RetryAfter
}
