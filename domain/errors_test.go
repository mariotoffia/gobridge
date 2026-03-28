package domain_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

// TestBridgeError_Error verifies Error returns the message and Wrap formats a chained error string.
func TestBridgeError_Error(t *testing.T) {
	e := &domain.BridgeError{
		Code: domain.ErrCodeTimeout, Class: domain.ErrorTransient,
		Message: "timed out",
	}
	if got := e.Error(); got != "timed out" {
		t.Fatalf("expected %q, got %q", "timed out", got)
	}

	wrapped := e.Wrap(fmt.Errorf("underlying"))
	if got := wrapped.Error(); got != "timed out: underlying" {
		t.Fatalf("expected wrapped message, got %q", got)
	}
}

// TestBridgeError_Is verifies errors.Is matches wrapped BridgeError sentinels and does not match unrelated codes.
func TestBridgeError_Is(t *testing.T) {
	err := domain.ErrTimeout.Wrap(fmt.Errorf("deadline"))
	if !errors.Is(err, domain.ErrTimeout) {
		t.Fatal("expected errors.Is to match ErrTimeout")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatal("should not match ErrNotFound")
	}
}

// TestBridgeError_As verifies AsBridgeError unwraps to the correct code and RetryAfter from a wrapped error chain.
func TestBridgeError_As(t *testing.T) {
	err := domain.ErrThrottled.WithRetryAfter(5 * time.Second).Wrap(fmt.Errorf("429"))
	be, ok := domain.AsBridgeError(err)
	if !ok {
		t.Fatal("expected AsBridgeError to succeed")
	}
	if be.Code != domain.ErrCodeThrottled {
		t.Fatalf("expected code %s, got %s", domain.ErrCodeThrottled, be.Code)
	}
	if be.RetryAfter != 5*time.Second {
		t.Fatalf("expected RetryAfter 5s, got %v", be.RetryAfter)
	}
}

// TestBridgeError_With verifies With attaches context on a copy without mutating the original sentinel.
func TestBridgeError_With(t *testing.T) {
	e := domain.ErrConnectionLost.With("topic", "sensors/temp")
	if v, ok := e.Context["topic"]; !ok || v != "sensors/temp" {
		t.Fatalf("expected context key, got %v", e.Context)
	}
	// Original sentinel must not be mutated.
	if domain.ErrConnectionLost.Context != nil {
		t.Fatal("sentinel was mutated")
	}
}

// TestBridgeError_WithMessage verifies WithMessage overrides the message on a copy without mutating the sentinel.
func TestBridgeError_WithMessage(t *testing.T) {
	e := domain.ErrUnavailable.WithMessage("broker down")
	if e.Message != "broker down" {
		t.Fatalf("expected custom message, got %q", e.Message)
	}
	if domain.ErrUnavailable.Message == "broker down" {
		t.Fatal("sentinel was mutated")
	}
}

// TestBridgeError_Unwrap verifies Unwrap returns the underlying cause from a wrapped BridgeError.
func TestBridgeError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("root cause")
	e := domain.ErrNotFound.Wrap(cause)
	if errors.Unwrap(e) != cause {
		t.Fatal("Unwrap did not return cause")
	}
}

// TestIsRecoverableError validates IsRecoverableError for nil, transient, permanent, rejected, expired, and arbitrary errors.
func TestIsRecoverableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transient", domain.ErrTimeout, true},
		{"permanent", domain.ErrNotFound, false},
		{"expired", domain.ErrMessageExpired, false},
		{"rejected", domain.ErrInvalidPayload, false},
		{"unknown error", fmt.Errorf("random"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.IsRecoverableError(tt.err); got != tt.want {
				t.Fatalf("IsRecoverableError = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetRetryAfter verifies GetRetryAfter returns zero for nil and non-throttled errors and honors RetryAfter on throttled errors.
func TestGetRetryAfter(t *testing.T) {
	if d := domain.GetRetryAfter(nil); d != 0 {
		t.Fatalf("expected 0 for nil, got %v", d)
	}
	if d := domain.GetRetryAfter(fmt.Errorf("plain")); d != 0 {
		t.Fatalf("expected 0 for plain error, got %v", d)
	}
	e := domain.ErrThrottled.WithRetryAfter(3 * time.Second)
	if d := domain.GetRetryAfter(e); d != 3*time.Second {
		t.Fatalf("expected 3s, got %v", d)
	}
}

// TestNewBridgeError verifies NewBridgeError sets the code and error class on the constructed BridgeError.
func TestNewBridgeError(t *testing.T) {
	e := domain.NewBridgeError("CUSTOM", domain.ErrorTransient, "custom error")
	if e.Code != "CUSTOM" || e.Class != domain.ErrorTransient {
		t.Fatalf("unexpected error: %+v", e)
	}
}

// TestSentinelClasses validates each package sentinel BridgeError carries the expected ErrorClass.
func TestSentinelClasses(t *testing.T) {
	tests := []struct {
		err   *domain.BridgeError
		class domain.ErrorClass
	}{
		{domain.ErrTimeout, domain.ErrorTransient},
		{domain.ErrConnectionLost, domain.ErrorTransient},
		{domain.ErrUnavailable, domain.ErrorTransient},
		{domain.ErrThrottled, domain.ErrorTransient},
		{domain.ErrBrokerBusy, domain.ErrorTransient},
		{domain.ErrTemporaryAuthFailure, domain.ErrorTransient},
		{domain.ErrNotAuthorized, domain.ErrorPermanent},
		{domain.ErrForbidden, domain.ErrorPermanent},
		{domain.ErrNotFound, domain.ErrorPermanent},
		{domain.ErrProtocolError, domain.ErrorPermanent},
		{domain.ErrQoSNotSupported, domain.ErrorPermanent},
		{domain.ErrNotSupported, domain.ErrorPermanent},
		{domain.ErrVersionMismatch, domain.ErrorPermanent},
		{domain.ErrAlreadyExists, domain.ErrorPermanent},
		{domain.ErrStaleFencingToken, domain.ErrorPermanent},
		{domain.ErrDuplicateRecord, domain.ErrorPermanent},
		{domain.ErrInvalidPayload, domain.ErrorRejected},
		{domain.ErrPayloadTooLarge, domain.ErrorRejected},
		{domain.ErrInvalidTopic, domain.ErrorRejected},
		{domain.ErrSchemaViolation, domain.ErrorRejected},
		{domain.ErrMessageExpired, domain.ErrorExpired},
		{domain.ErrMessageFiltered, domain.ErrorRejected},
	}
	for _, tt := range tests {
		t.Run(string(tt.err.Code), func(t *testing.T) {
			if tt.err.Class != tt.class {
				t.Fatalf("expected class %s, got %s", tt.class, tt.err.Class)
			}
		})
	}
}

// TestErrMessageFiltered_Is verifies errors.Is for ErrMessageFiltered matches itself and not other sentinels.
func TestErrMessageFiltered_Is(t *testing.T) {
	if !errors.Is(domain.ErrMessageFiltered, domain.ErrMessageFiltered) {
		t.Fatal("ErrMessageFiltered should match itself via errors.Is")
	}
	if errors.Is(domain.ErrMessageFiltered, domain.ErrNotFound) {
		t.Fatal("ErrMessageFiltered should not match ErrNotFound")
	}
}

// TestBridgeError_Is_NonBridgeErrorTarget_ReturnsFalse validates that BridgeError.Is returns false
// for targets that are not *BridgeError, exercising the type-assertion guard.
func TestBridgeError_Is_NonBridgeErrorTarget_ReturnsFalse(t *testing.T) {
	be := domain.ErrTimeout.Wrap(fmt.Errorf("inner"))

	tests := []struct {
		name   string
		target error
	}{
		{"io.EOF", io.EOF},
		{"context.Canceled", context.Canceled},
		{"plain fmt error", fmt.Errorf("x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if errors.Is(be, tt.target) {
				t.Fatalf("BridgeError should not match %T via errors.Is", tt.target)
			}
		})
	}
}

// TestSentinelErrorCodes_MatchDeclaredConstants validates every sentinel's .Code field
// matches its corresponding ErrCode* constant, covering all 24 declared sentinels.
func TestSentinelErrorCodes_MatchDeclaredConstants(t *testing.T) {
	tests := []struct {
		sentinel *domain.BridgeError
		code     domain.ErrorCode
	}{
		{domain.ErrTimeout, domain.ErrCodeTimeout},
		{domain.ErrConnectionLost, domain.ErrCodeConnectionLost},
		{domain.ErrUnavailable, domain.ErrCodeUnavailable},
		{domain.ErrThrottled, domain.ErrCodeThrottled},
		{domain.ErrBrokerBusy, domain.ErrCodeBrokerBusy},
		{domain.ErrTemporaryAuthFailure, domain.ErrCodeTemporaryAuthFailure},
		{domain.ErrNotAuthorized, domain.ErrCodeNotAuthorized},
		{domain.ErrForbidden, domain.ErrCodeForbidden},
		{domain.ErrNotFound, domain.ErrCodeNotFound},
		{domain.ErrInvalidPayload, domain.ErrCodeInvalidPayload},
		{domain.ErrPayloadTooLarge, domain.ErrCodePayloadTooLarge},
		{domain.ErrInvalidTopic, domain.ErrCodeInvalidTopic},
		{domain.ErrProtocolError, domain.ErrCodeProtocolError},
		{domain.ErrSchemaViolation, domain.ErrCodeSchemaViolation},
		{domain.ErrMessageExpired, domain.ErrCodeMessageExpired},
		{domain.ErrQoSNotSupported, domain.ErrCodeQoSNotSupported},
		{domain.ErrMessageFiltered, domain.ErrCodeMessageFiltered},
		{domain.ErrNotSupported, domain.ErrCodeNotSupported},
		{domain.ErrVersionMismatch, domain.ErrCodeVersionMismatch},
		{domain.ErrAlreadyExists, domain.ErrCodeAlreadyExists},
		{domain.ErrStaleFencingToken, domain.ErrCodeStaleFencingToken},
		{domain.ErrDuplicateRecord, domain.ErrCodeDuplicateRecord},
		{domain.ErrNoRouteOwner, domain.ErrCodeNoRouteOwner},
		{domain.ErrForwardFailed, domain.ErrCodeForwardFailed},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if tt.sentinel.Code != tt.code {
				t.Fatalf("sentinel .Code = %s, want %s", tt.sentinel.Code, tt.code)
			}
		})
	}
}

// TestBridgeError_Clone_ContextIsolation verifies that .With() returns a copy whose
// context map is fully isolated from the original — mutations on the clone do not leak.
func TestBridgeError_Clone_ContextIsolation(t *testing.T) {
	original := domain.ErrTimeout.With("key1", "val1")
	clone := original.With("key2", "val2")

	if _, ok := original.Context["key2"]; ok {
		t.Fatal("mutating clone leaked key2 into original")
	}
	if v, ok := clone.Context["key1"]; !ok || v != "val1" {
		t.Fatal("clone should inherit key1 from original")
	}
	if v, ok := clone.Context["key2"]; !ok || v != "val2" {
		t.Fatal("clone should contain key2")
	}

	clone.Context["key1"] = "overwritten"
	if original.Context["key1"] != "val1" {
		t.Fatal("direct map mutation on clone leaked into original")
	}
}

// TestBridgeError_WithRetryAfter_DoesNotMutateSentinel verifies that calling
// WithRetryAfter on a sentinel returns a new value without altering the sentinel.
func TestBridgeError_WithRetryAfter_DoesNotMutateSentinel(t *testing.T) {
	before := domain.ErrThrottled.RetryAfter

	derived := domain.ErrThrottled.WithRetryAfter(10 * time.Second)

	if domain.ErrThrottled.RetryAfter != before {
		t.Fatalf("sentinel RetryAfter mutated: got %v, want %v", domain.ErrThrottled.RetryAfter, before)
	}
	if derived.RetryAfter != 10*time.Second {
		t.Fatalf("derived RetryAfter = %v, want 10s", derived.RetryAfter)
	}
}

// TestIsRecoverableError_DocumentsBehavior documents the design decision that unknown
// (non-BridgeError) errors are treated as recoverable. This test exists for awareness;
// if the policy changes, this test should be updated to reflect the new contract.
func TestIsRecoverableError_DocumentsBehavior(t *testing.T) {
	unknowns := []error{
		fmt.Errorf("some random error"),
		io.ErrUnexpectedEOF,
		context.DeadlineExceeded,
	}
	for _, err := range unknowns {
		t.Run(err.Error(), func(t *testing.T) {
			if !domain.IsRecoverableError(err) {
				t.Fatalf("design decision: unknown error %q should be treated as recoverable", err)
			}
		})
	}
}
