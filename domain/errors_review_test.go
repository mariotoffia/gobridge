package domain_test

import (
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════
// Error Classification Review Tests
//
// Validates sentinel coverage gaps and error chain correctness
// identified by QA and Go expert reviews:
//   - Missing sentinel classes (ErrNoRouteOwner, ErrForwardFailed)
//   - BridgeError.Is() code-only semantics documented
//   - Error wrapping chain depth
// ═══════════════════════════════════════════════════════════════════

// TestSentinelClasses_ClusterErrors validates error class assignment for
// cluster-related sentinels that were missing from the original test.
func TestSentinelClasses_ClusterErrors(t *testing.T) {
	tests := []struct {
		sentinel *domain.BridgeError
		class    domain.ErrorClass
	}{
		{domain.ErrNoRouteOwner, domain.ErrorTransient},
		{domain.ErrForwardFailed, domain.ErrorTransient},
	}

	for _, tt := range tests {
		t.Run(string(tt.sentinel.Code), func(t *testing.T) {
			if tt.sentinel.Class != tt.class {
				t.Fatalf("expected class %s, got %s", tt.class, tt.sentinel.Class)
			}
		})
	}
}

// TestBridgeError_Is_MatchesByCodeOnly validates the documented behavior
// that BridgeError.Is() compares only by ErrorCode, not by ErrorClass.
func TestBridgeError_Is_MatchesByCodeOnly(t *testing.T) {
	err1 := &domain.BridgeError{
		Code:    domain.ErrCodeTimeout,
		Class:   domain.ErrorTransient,
		Message: "first",
	}
	err2 := &domain.BridgeError{
		Code:    domain.ErrCodeTimeout,
		Class:   domain.ErrorPermanent,
		Message: "second",
	}

	if !errors.Is(err1, err2) {
		t.Fatal("BridgeError.Is should match by Code regardless of Class")
	}
}

// TestBridgeError_DeepWrapping validates that errors.Is works through
// multiple layers of wrapping.
func TestBridgeError_DeepWrapping(t *testing.T) {
	inner := domain.ErrTimeout.Wrap(errors.New("root"))
	middle := domain.ErrConnectionLost.Wrap(inner)

	if !errors.Is(middle, domain.ErrConnectionLost) {
		t.Fatal("should match outer error")
	}

	var be *domain.BridgeError
	if !errors.As(middle, &be) {
		t.Fatal("errors.As should extract BridgeError")
	}
	if be.Code != domain.ErrCodeConnectionLost {
		t.Fatalf("expected CONNECTION_LOST, got %s", be.Code)
	}

	if !errors.Is(errors.Unwrap(middle), domain.ErrTimeout) {
		t.Fatal("unwrapped error should match ErrTimeout")
	}
}

// TestBridgeError_With_ChainingDoesNotMutatePreviousLinks validates that
// chaining multiple .With() calls creates independent copies.
func TestBridgeError_With_ChainingDoesNotMutatePreviousLinks(t *testing.T) {
	a := domain.ErrTimeout.With("k1", "v1")
	b := a.With("k2", "v2")
	c := a.With("k3", "v3")

	if _, ok := a.Context["k2"]; ok {
		t.Fatal("b's context leaked into a")
	}
	if _, ok := a.Context["k3"]; ok {
		t.Fatal("c's context leaked into a")
	}
	if _, ok := b.Context["k3"]; ok {
		t.Fatal("c's context leaked into b")
	}
	if _, ok := c.Context["k2"]; ok {
		t.Fatal("b's context leaked into c")
	}
}

// TestAllSentinels_HaveNonEmptyMessage validates that every sentinel
// error has a non-empty Message field.
func TestAllSentinels_HaveNonEmptyMessage(t *testing.T) {
	sentinels := []*domain.BridgeError{
		domain.ErrTimeout,
		domain.ErrConnectionLost,
		domain.ErrUnavailable,
		domain.ErrThrottled,
		domain.ErrBrokerBusy,
		domain.ErrTemporaryAuthFailure,
		domain.ErrNotAuthorized,
		domain.ErrForbidden,
		domain.ErrNotFound,
		domain.ErrInvalidPayload,
		domain.ErrPayloadTooLarge,
		domain.ErrInvalidTopic,
		domain.ErrProtocolError,
		domain.ErrSchemaViolation,
		domain.ErrMessageExpired,
		domain.ErrQoSNotSupported,
		domain.ErrMessageFiltered,
		domain.ErrNotSupported,
		domain.ErrVersionMismatch,
		domain.ErrAlreadyExists,
		domain.ErrStaleFencingToken,
		domain.ErrDuplicateRecord,
		domain.ErrNoRouteOwner,
		domain.ErrForwardFailed,
	}

	for _, s := range sentinels {
		t.Run(string(s.Code), func(t *testing.T) {
			if s.Message == "" {
				t.Fatalf("sentinel %s has empty Message", s.Code)
			}
		})
	}
}

// TestAllSentinels_HaveNonEmptyCode validates that every sentinel
// error has a non-empty Code field.
func TestAllSentinels_HaveNonEmptyCode(t *testing.T) {
	sentinels := []*domain.BridgeError{
		domain.ErrTimeout,
		domain.ErrConnectionLost,
		domain.ErrUnavailable,
		domain.ErrThrottled,
		domain.ErrBrokerBusy,
		domain.ErrTemporaryAuthFailure,
		domain.ErrNotAuthorized,
		domain.ErrForbidden,
		domain.ErrNotFound,
		domain.ErrInvalidPayload,
		domain.ErrPayloadTooLarge,
		domain.ErrInvalidTopic,
		domain.ErrProtocolError,
		domain.ErrSchemaViolation,
		domain.ErrMessageExpired,
		domain.ErrQoSNotSupported,
		domain.ErrMessageFiltered,
		domain.ErrNotSupported,
		domain.ErrVersionMismatch,
		domain.ErrAlreadyExists,
		domain.ErrStaleFencingToken,
		domain.ErrDuplicateRecord,
		domain.ErrNoRouteOwner,
		domain.ErrForwardFailed,
	}

	for _, s := range sentinels {
		if s.Code == "" {
			t.Fatalf("sentinel with message %q has empty Code", s.Message)
		}
	}
}
