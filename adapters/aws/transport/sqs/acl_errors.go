package sqs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// MapError converts an SQS / AWS SDK error into a *shared.BridgeError
// with the appropriate ErrorClass for the runtime to decide retry vs DLQ.
func MapError(err error) *shared.BridgeError {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return shared.ErrTimeout.Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		return shared.ErrUnavailable.Wrap(err)
	}

	// Typed SQS API errors.
	var queueNotExist *sqstypes.QueueDoesNotExist
	if errors.As(err, &queueNotExist) {
		return shared.ErrNotFound.Wrap(err).WithMessage("queue does not exist")
	}

	var notInflight *sqstypes.MessageNotInflight
	if errors.As(err, &notInflight) {
		// Receipt-handle expiry: the visibility window lapsed, so SQS
		// already owns redelivery of this message. Classify transient —
		// the failed settlement is a timing conflict, not a poison
		// message; a permanent/rejected class would pollute DLQ
		// categorization and metrics for a condition the broker
		// self-heals.
		return shared.ErrUnavailable.Wrap(err).
			WithMessage("message not in flight (visibility expired; source will redeliver)")
	}

	var badHandle *sqstypes.ReceiptHandleIsInvalid
	if errors.As(err, &badHandle) {
		// Same class as MessageNotInflight: a stale/expired receipt
		// handle means SQS will redeliver on its own — transient, not
		// a permanent payload rejection.
		return shared.ErrUnavailable.Wrap(err).
			WithMessage("receipt handle invalid or expired (source will redeliver)")
	}

	var idsNotDistinct *sqstypes.BatchEntryIdsNotDistinct
	if errors.As(err, &idsNotDistinct) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("batch entry IDs not distinct")
	}

	var emptyBatch *sqstypes.EmptyBatchRequest
	if errors.As(err, &emptyBatch) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("empty batch request")
	}

	var invalidContents *sqstypes.InvalidMessageContents
	if errors.As(err, &invalidContents) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("invalid message contents")
	}

	var tooMany *sqstypes.TooManyEntriesInBatchRequest
	if errors.As(err, &tooMany) {
		return shared.ErrInvalidPayload.Wrap(err).WithMessage("too many entries in batch request")
	}

	var batchTooLong *sqstypes.BatchRequestTooLong
	if errors.As(err, &batchTooLong) {
		return shared.ErrPayloadTooLarge.Wrap(err).WithMessage("batch request too long")
	}

	var overLimit *sqstypes.OverLimit
	if errors.As(err, &overLimit) {
		return shared.ErrThrottled.Wrap(err).WithMessage("over limit")
	}

	var unsupported *sqstypes.UnsupportedOperation
	if errors.As(err, &unsupported) {
		return shared.ErrProtocolError.Wrap(err).WithMessage("unsupported operation")
	}

	// KMS (server-side encryption) errors need code-specific classification
	// (Finding 3). The string fallback below lower-cases the message and
	// matches "KmsAccessDenied" as "accessdenied" → permanent ErrNotAuthorized,
	// which false-DLQs every send during the 10-120s a freshly-granted KMS
	// key policy or IAM role takes to propagate. These typed checks MUST
	// precede the string fallback so the transient KMS codes are not
	// misclassified. Classification follows AWS KMS error semantics.
	var kmsThrottled *sqstypes.KmsThrottled
	if errors.As(err, &kmsThrottled) {
		// KMS-side request throttling — transient, back off and retry.
		return shared.ErrThrottled.Wrap(err).WithMessage("kms throttled")
	}
	var kmsAccessDenied *sqstypes.KmsAccessDenied
	if errors.As(err, &kmsAccessDenied) {
		// Caller lacks KMS access. Classify TEMPORARY: a freshly-granted key
		// policy / IAM role commonly takes 10-120s to propagate and a
		// permanent class would DLQ every message in that window. If the
		// grant never lands, the runtime's retry budget still bounds it.
		return shared.ErrTemporaryAuthFailure.Wrap(err).
			WithMessage("kms access denied (grant may still be propagating)")
	}
	// The remaining KMS codes are genuine, non-self-healing misconfigurations
	// (disabled/pending-deletion key, wrong key spec, missing key, region
	// opt-in): retrying cannot succeed within a redelivery window, so classify
	// permanent and let the runtime DLQ.
	var kmsDisabled *sqstypes.KmsDisabled
	if errors.As(err, &kmsDisabled) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms key disabled")
	}
	var kmsInvalidState *sqstypes.KmsInvalidState
	if errors.As(err, &kmsInvalidState) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms key invalid state")
	}
	var kmsNotFound *sqstypes.KmsNotFound
	if errors.As(err, &kmsNotFound) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms key not found")
	}
	var kmsOptIn *sqstypes.KmsOptInRequired
	if errors.As(err, &kmsOptIn) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms opt-in required")
	}
	var kmsInvalidKeyUsage *sqstypes.KmsInvalidKeyUsage
	if errors.As(err, &kmsInvalidKeyUsage) {
		return shared.ErrNotAuthorized.Wrap(err).WithMessage("kms invalid key usage")
	}

	// Fall back to string-based classification.
	msg := err.Error()

	if containsAny(msg, "throttl", "rate exceeded", "RequestLimitExceeded") {
		return shared.ErrThrottled.Wrap(err)
	}
	if containsAny(msg, "ServiceUnavailable", "InternalError", "service unavailable") {
		return shared.ErrUnavailable.Wrap(err)
	}
	if containsAny(msg, "connection refused", "connection reset", "network") {
		return shared.ErrConnectionLost.Wrap(err)
	}
	// Plain (non-KMS) API auth failures. MapError itself is a stateless
	// pure function, so it classifies plain auth PERMANENT here — a
	// genuinely-misconfigured policy must DLQ rather than retry forever.
	// The propagation window for a freshly-rotated static key / IAM role
	// (10-120s of AccessDenied for a condition that WILL self-heal) is
	// handled OUTSIDE this pure function by authGrace.classify (Finding:
	// c8-auth-permanent): the send path (sendOne / sendBatchChunk) and the
	// receive poll loop route every error through a per-adapter authGrace
	// that treats a plain auth failure as transient ErrTemporaryAuthFailure
	// while inside a bounded, clock-driven grace window and only escalates
	// to this permanent classification once the window lapses — mirroring
	// the KmsAccessDenied temporary treatment above. Keeping the grace in a
	// stateful wrapper preserves MapError's purity (and its use by callers
	// that legitimately want the immediate permanent verdict).
	if containsAny(msg, "AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId") {
		return shared.ErrNotAuthorized.Wrap(err)
	}
	// MissingParameter covers deterministic request faults such as a
	// FIFO send without a MessageGroupId: retrying cannot succeed, so
	// it must classify rejected — the same class the batch API's
	// SenderFault yields — instead of falling through to the transient
	// default and being retried forever.
	if containsAny(msg, "ValidationError", "InvalidParameter", "MissingParameter", "MalformedQueryString") {
		return shared.ErrInvalidPayload.Wrap(err)
	}

	// Safe default: treat unknown errors as recoverable.
	return shared.ErrUnavailable.Wrap(err)
}

func containsAny(s string, substrs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// authGraceWindow bounds how long a plain (non-KMS) API auth failure is
// treated as transient before it escalates to permanent ErrNotAuthorized.
// A freshly-rotated static key or a newly-granted IAM role commonly takes
// 10-120s to propagate, during which the broker returns AccessDenied for a
// condition that WILL self-heal; classifying it permanent inside that window
// makes a direct-hold route DLQ/drop-then-ACK the source during a purely
// transient gap (policy loss). 120s matches the upper bound documented for
// the KmsAccessDenied temporary treatment in MapError.
const authGraceWindow = 120 * time.Second

// authGrace gives plain API auth failures a bounded, clock-driven grace
// window (Finding: c8-auth-permanent). It is held per Sender/Receiver so the
// classification can be stateful without contaminating the pure MapError.
//
// The first plain auth failure of a streak records its instant; every auth
// failure within authGraceWindow of that instant classifies transient
// (ErrTemporaryAuthFailure, retryable), and only a failure past the window
// escalates to permanent ErrNotAuthorized. Only a SUCCESSFUL call ends the
// streak (reset): a non-auth error (throttle, network, timeout) does NOT
// prove auth recovered, so it neither starts nor clears a window. This makes
// a genuine revocation GUARANTEED to escalate to permanent at the window edge
// even when transient blips interleave, while never DLQ-ing prematurely. A
// later rotation gap (after a real success) gets a fresh window.
type authGrace struct {
	clk    clock.Clock
	window time.Duration

	mu    sync.Mutex
	since time.Time // first auth failure of the current streak; zero = none pending
}

// newAuthGrace builds an authGrace using the injected clock (defaulting to
// clock.System) and grace window (defaulting to authGraceWindow when <= 0).
func newAuthGrace(clk clock.Clock, window time.Duration) *authGrace {
	if clk == nil {
		clk = clock.System
	}
	if window <= 0 {
		window = authGraceWindow
	}
	return &authGrace{clk: clk, window: window}
}

// classify maps err via MapError and, for a plain API auth failure, applies
// the bounded grace: transient (ErrTemporaryAuthFailure) while inside the
// window measured from the first failure of the current streak, permanent
// (the MapError verdict, ErrNotAuthorized) once the window lapses. A non-auth
// error defers to MapError unchanged and leaves any in-progress window intact
// — it neither starts nor clears the streak; only reset() (on a real success)
// clears it, so a genuine revocation still escalates at the window edge even
// when transient blips interleave.
func (g *authGrace) classify(err error) *shared.BridgeError {
	mapped := MapError(err)
	// A nil grace (struct-literal Sender/Receiver in tests that bypass the
	// constructors) degrades to the pure MapError verdict — production always
	// wires a grace via newAuthGrace.
	if g == nil {
		return mapped
	}
	if !isPlainAuthFailure(err) {
		// A NON-auth error (throttle, network, timeout) does not prove auth
		// recovered — only a real SUCCESS does (reset()). Leave any
		// in-progress auth window intact: do NOT clear `since` (so a genuine
		// revocation still escalates to permanent at the window edge despite
		// interleaved blips) and do NOT start one (a non-auth error must
		// never open a window).
		return mapped
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Now()
	if g.since.IsZero() {
		g.since = now
	}
	if now.Sub(g.since) <= g.window {
		return shared.ErrTemporaryAuthFailure.Wrap(err).
			WithMessage("auth failure within grace window (credentials may still be propagating)")
	}
	return mapped
}

// reset clears any pending auth-failure streak so the next plain auth
// failure starts a fresh grace window. Callers invoke it after a successful
// SQS call.
func (g *authGrace) reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.since = time.Time{}
	g.mu.Unlock()
}

// isPlainAuthFailure reports whether err is a plain (non-KMS) API auth
// failure — AccessDenied / UnauthorizedAccess / InvalidClientTokenId — that
// MapError classifies permanent ErrNotAuthorized. It deliberately excludes
// the typed KMS errors (KmsAccessDenied etc.): MapError classifies those
// BEFORE the string fallback and each already carries its own
// transient/permanent policy, so the grace must not double-handle a KMS
// condition (whose .Error() text can contain the same substrings).
func isPlainAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var (
		kmsThrottled       *sqstypes.KmsThrottled
		kmsAccessDenied    *sqstypes.KmsAccessDenied
		kmsDisabled        *sqstypes.KmsDisabled
		kmsInvalidState    *sqstypes.KmsInvalidState
		kmsNotFound        *sqstypes.KmsNotFound
		kmsOptIn           *sqstypes.KmsOptInRequired
		kmsInvalidKeyUsage *sqstypes.KmsInvalidKeyUsage
	)
	if errors.As(err, &kmsThrottled) || errors.As(err, &kmsAccessDenied) ||
		errors.As(err, &kmsDisabled) || errors.As(err, &kmsInvalidState) ||
		errors.As(err, &kmsNotFound) || errors.As(err, &kmsOptIn) ||
		errors.As(err, &kmsInvalidKeyUsage) {
		return false
	}
	return containsAny(err.Error(), "AccessDenied", "UnauthorizedAccess", "InvalidClientTokenId")
}
