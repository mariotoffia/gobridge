package shared_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestErrorCodeToClass_IsAFunction is the finding-3 guard: it scans every
// constructed / sentinel *shared.BridgeError produced inside the domain and
// asserts that each ErrorCode maps to EXACTLY ONE ErrorClass — i.e. code→class
// is a mathematical function.
//
// This matters because BridgeError.Is matches on Code ALONE (see
// TestBridgeError_Is_MatchesByCodeOnly). If one code carried two classes, the
// runtime's transient/permanent/expired/rejected routing decision for that code
// would depend on which construction site produced the error — a latent, order-
// dependent classification bug. The specific regression this pins: the routing
// policy validators (invalidEnum / invalidDuration) once minted
// ErrCodeInvalidPayload with class Permanent while the ErrInvalidPayload
// sentinel is Rejected; they now use the dedicated ErrCodeInvalidConfig.
//
// When you add a new domain sentinel or a new BridgeError construction site,
// add it to domainBridgeErrors (or the routing-constructor section) so this
// invariant keeps covering the whole domain.
func TestErrorCodeToClass_IsAFunction(t *testing.T) {
	// Every sentinel *BridgeError declared in domain/shared.
	domainSentinels := []*shared.BridgeError{
		// transient
		shared.ErrTimeout,
		shared.ErrConnectionLost,
		shared.ErrUnavailable,
		shared.ErrThrottled,
		shared.ErrBrokerBusy,
		shared.ErrTemporaryAuthFailure,
		shared.ErrTenantQuotaExceeded,
		// permanent / rejected / expired
		shared.ErrNotAuthorized,
		shared.ErrForbidden,
		shared.ErrNotFound,
		shared.ErrInvalidConfig,
		shared.ErrInvalidPayload,
		shared.ErrPayloadTooLarge,
		shared.ErrInvalidTopic,
		shared.ErrProtocolError,
		shared.ErrSchemaViolation,
		shared.ErrMessageExpired,
		shared.ErrQoSNotSupported,
		shared.ErrMessageFiltered,
		// infrastructure / fencing
		shared.ErrNotSupported,
		shared.ErrVersionMismatch,
		shared.ErrAlreadyExists,
		shared.ErrStaleFencingToken,
		shared.ErrDuplicateRecord,
		shared.ErrTransportClosedPermanently,
		// cluster routing
		shared.ErrNoRouteOwner,
		shared.ErrForwardFailed,
		// processor chain
		shared.ErrProcessorPanic,
		shared.ErrProcessorTimeout,
		// outbox aggregate state-machine
		shared.ErrInvalidOutboxRecord,
		shared.ErrOutboxNotClaimable,
		shared.ErrOutboxNotInClaimedState,
		shared.ErrOutboxAlreadyTerminal,
	}

	// The routing policy validators are the ONLY non-sentinel BridgeError
	// construction sites in the domain (verified by grep for
	// &shared.BridgeError{ / NewBridgeError under domain/). Exercise each
	// branch and fold the produced errors into the same scan.
	routingErrors := []*shared.BridgeError{
		asBridgeError(t, routing.RoutePolicy{DeliveryMode: "wat"}.Validate()),            // invalidEnum
		asBridgeError(t, routing.RoutePolicy{DispatchMode: "nope"}.Validate()),           // invalidEnum
		asBridgeError(t, routing.RoutePolicy{AckAfter: "bad"}.Validate()),                // invalidEnum
		asBridgeError(t, routing.RoutePolicy{OnExpired: "x"}.Validate()),                 // invalidEnum
		asBridgeError(t, routing.RoutePolicy{OnPermanentFailure: "y"}.Validate()),        // invalidEnum
		asBridgeError(t, routing.RoutePolicy{ReplayBudget: -1 * time.Second}.Validate()), // invalidDuration
	}

	codeToClass := make(map[shared.ErrorCode]shared.ErrorClass)
	assign := func(code shared.ErrorCode, class shared.ErrorClass, origin string) {
		if code == "" {
			t.Fatalf("%s: BridgeError has empty Code", origin)
		}
		if prev, seen := codeToClass[code]; seen && prev != class {
			t.Fatalf("code %q maps to two classes: %q and %q (code→class must be a function); origin=%s",
				code, prev, class, origin)
		}
		codeToClass[code] = class
	}

	for _, be := range domainSentinels {
		assign(be.Code, be.Class, "sentinel "+string(be.Code))
	}
	for _, be := range routingErrors {
		assign(be.Code, be.Class, "routing.Validate "+be.Message)
	}

	// Crux of finding 3, asserted explicitly for readability.
	if got := codeToClass[shared.ErrCodeInvalidPayload]; got != shared.ErrorRejected {
		t.Fatalf("INVALID_PAYLOAD class = %q, want %q (must stay uniquely rejected)", got, shared.ErrorRejected)
	}
	if got := codeToClass[shared.ErrCodeInvalidConfig]; got != shared.ErrorPermanent {
		t.Fatalf("INVALID_CONFIG class = %q, want %q", got, shared.ErrorPermanent)
	}
}

// TestRoutingValidate_UsesInvalidConfigNotInvalidPayload proves the routing
// policy validators no longer alias the message-payload code. A config defect
// (bad enum / negative duration) is ErrCodeInvalidConfig (Permanent) and is NOT
// errors.Is-equal to ErrInvalidPayload.
func TestRoutingValidate_UsesInvalidConfigNotInvalidPayload(t *testing.T) {
	err := routing.RoutePolicy{DeliveryMode: "bogus"}.Validate()
	if err == nil {
		t.Fatal("expected an error for an invalid DeliveryMode")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("routing config error should match ErrInvalidConfig, got %v", err)
	}
	if errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatal("routing config error must NOT match ErrInvalidPayload (code was unified to INVALID_CONFIG)")
	}
}

func asBridgeError(t *testing.T, err error) *shared.BridgeError {
	t.Helper()
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatalf("expected a *shared.BridgeError, got %T: %v", err, err)
	}
	return be
}
