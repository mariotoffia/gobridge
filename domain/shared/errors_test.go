package shared_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestBridgeError_Error verifies Error returns the message and Wrap formats a chained error string.
func TestBridgeError_Error(t *testing.T) {
	e := &shared.BridgeError{
		Code: shared.ErrCodeTimeout, Class: shared.ErrorTransient,
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
	err := shared.ErrTimeout.Wrap(fmt.Errorf("deadline"))
	if !errors.Is(err, shared.ErrTimeout) {
		t.Fatal("expected errors.Is to match ErrTimeout")
	}
	if errors.Is(err, shared.ErrNotFound) {
		t.Fatal("should not match ErrNotFound")
	}
}

// TestBridgeError_As verifies AsBridgeError unwraps to the correct code and RetryAfter from a wrapped error chain.
func TestBridgeError_As(t *testing.T) {
	err := shared.ErrThrottled.WithRetryAfter(5 * time.Second).Wrap(fmt.Errorf("429"))
	be, ok := shared.AsBridgeError(err)
	if !ok {
		t.Fatal("expected AsBridgeError to succeed")
	}
	if be.Code != shared.ErrCodeThrottled {
		t.Fatalf("expected code %s, got %s", shared.ErrCodeThrottled, be.Code)
	}
	if be.RetryAfter != 5*time.Second {
		t.Fatalf("expected RetryAfter 5s, got %v", be.RetryAfter)
	}
}

// TestBridgeError_With verifies With attaches context on a copy without mutating the original sentinel.
func TestBridgeError_With(t *testing.T) {
	e := shared.ErrConnectionLost.With("topic", "sensors/temp")
	if v, ok := e.Context["topic"]; !ok || v != "sensors/temp" {
		t.Fatalf("expected context key, got %v", e.Context)
	}
	// Original sentinel must not be mutated.
	if shared.ErrConnectionLost.Context != nil {
		t.Fatal("sentinel was mutated")
	}
}

// TestBridgeError_WithMessage verifies WithMessage overrides the message on a copy without mutating the sentinel.
func TestBridgeError_WithMessage(t *testing.T) {
	e := shared.ErrUnavailable.WithMessage("broker down")
	if e.Message != "broker down" {
		t.Fatalf("expected custom message, got %q", e.Message)
	}
	if shared.ErrUnavailable.Message == "broker down" {
		t.Fatal("sentinel was mutated")
	}
}

// TestBridgeError_Unwrap verifies Unwrap returns the underlying cause from a wrapped BridgeError.
func TestBridgeError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("root cause")
	e := shared.ErrNotFound.Wrap(cause)
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
		{"transient", shared.ErrTimeout, true},
		{"permanent", shared.ErrNotFound, false},
		{"expired", shared.ErrMessageExpired, false},
		{"rejected", shared.ErrInvalidPayload, false},
		{"unknown error", fmt.Errorf("random"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shared.IsRecoverableError(tt.err); got != tt.want {
				t.Fatalf("IsRecoverableError = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetRetryAfter verifies GetRetryAfter returns zero for nil and non-throttled errors and honors RetryAfter on throttled errors.
func TestGetRetryAfter(t *testing.T) {
	if d := shared.GetRetryAfter(nil); d != 0 {
		t.Fatalf("expected 0 for nil, got %v", d)
	}
	if d := shared.GetRetryAfter(fmt.Errorf("plain")); d != 0 {
		t.Fatalf("expected 0 for plain error, got %v", d)
	}
	e := shared.ErrThrottled.WithRetryAfter(3 * time.Second)
	if d := shared.GetRetryAfter(e); d != 3*time.Second {
		t.Fatalf("expected 3s, got %v", d)
	}
}

// TestNewBridgeError verifies NewBridgeError sets the code and error class on the constructed BridgeError.
func TestNewBridgeError(t *testing.T) {
	e := shared.NewBridgeError("CUSTOM", shared.ErrorTransient, "custom error")
	if e.Code != "CUSTOM" || e.Class != shared.ErrorTransient {
		t.Fatalf("unexpected error: %+v", e)
	}
}

// TestSentinelClasses validates each package sentinel BridgeError carries the expected ErrorClass.
func TestSentinelClasses(t *testing.T) {
	tests := []struct {
		err   *shared.BridgeError
		class shared.ErrorClass
	}{
		{shared.ErrTimeout, shared.ErrorTransient},
		{shared.ErrConnectionLost, shared.ErrorTransient},
		{shared.ErrUnavailable, shared.ErrorTransient},
		{shared.ErrThrottled, shared.ErrorTransient},
		{shared.ErrBrokerBusy, shared.ErrorTransient},
		{shared.ErrTemporaryAuthFailure, shared.ErrorTransient},
		{shared.ErrNotAuthorized, shared.ErrorPermanent},
		{shared.ErrForbidden, shared.ErrorPermanent},
		{shared.ErrNotFound, shared.ErrorPermanent},
		{shared.ErrProtocolError, shared.ErrorPermanent},
		{shared.ErrQoSNotSupported, shared.ErrorPermanent},
		{shared.ErrNotSupported, shared.ErrorPermanent},
		{shared.ErrVersionMismatch, shared.ErrorPermanent},
		{shared.ErrAlreadyExists, shared.ErrorPermanent},
		{shared.ErrStaleFencingToken, shared.ErrorPermanent},
		{shared.ErrDuplicateRecord, shared.ErrorPermanent},
		{shared.ErrInvalidPayload, shared.ErrorRejected},
		{shared.ErrPayloadTooLarge, shared.ErrorRejected},
		{shared.ErrInvalidTopic, shared.ErrorRejected},
		{shared.ErrSchemaViolation, shared.ErrorRejected},
		{shared.ErrMessageExpired, shared.ErrorExpired},
		{shared.ErrMessageFiltered, shared.ErrorRejected},
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
	if !errors.Is(shared.ErrMessageFiltered, shared.ErrMessageFiltered) {
		t.Fatal("ErrMessageFiltered should match itself via errors.Is")
	}
	if errors.Is(shared.ErrMessageFiltered, shared.ErrNotFound) {
		t.Fatal("ErrMessageFiltered should not match ErrNotFound")
	}
}

// TestBridgeError_Is_NonBridgeErrorTarget_ReturnsFalse validates that BridgeError.Is returns false
// for targets that are not *BridgeError, exercising the type-assertion guard.
func TestBridgeError_Is_NonBridgeErrorTarget_ReturnsFalse(t *testing.T) {
	be := shared.ErrTimeout.Wrap(fmt.Errorf("inner"))

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
		sentinel *shared.BridgeError
		code     shared.ErrorCode
	}{
		{shared.ErrTimeout, shared.ErrCodeTimeout},
		{shared.ErrConnectionLost, shared.ErrCodeConnectionLost},
		{shared.ErrUnavailable, shared.ErrCodeUnavailable},
		{shared.ErrThrottled, shared.ErrCodeThrottled},
		{shared.ErrBrokerBusy, shared.ErrCodeBrokerBusy},
		{shared.ErrTemporaryAuthFailure, shared.ErrCodeTemporaryAuthFailure},
		{shared.ErrNotAuthorized, shared.ErrCodeNotAuthorized},
		{shared.ErrForbidden, shared.ErrCodeForbidden},
		{shared.ErrNotFound, shared.ErrCodeNotFound},
		{shared.ErrInvalidPayload, shared.ErrCodeInvalidPayload},
		{shared.ErrPayloadTooLarge, shared.ErrCodePayloadTooLarge},
		{shared.ErrInvalidTopic, shared.ErrCodeInvalidTopic},
		{shared.ErrProtocolError, shared.ErrCodeProtocolError},
		{shared.ErrSchemaViolation, shared.ErrCodeSchemaViolation},
		{shared.ErrMessageExpired, shared.ErrCodeMessageExpired},
		{shared.ErrQoSNotSupported, shared.ErrCodeQoSNotSupported},
		{shared.ErrMessageFiltered, shared.ErrCodeMessageFiltered},
		{shared.ErrNotSupported, shared.ErrCodeNotSupported},
		{shared.ErrVersionMismatch, shared.ErrCodeVersionMismatch},
		{shared.ErrAlreadyExists, shared.ErrCodeAlreadyExists},
		{shared.ErrStaleFencingToken, shared.ErrCodeStaleFencingToken},
		{shared.ErrDuplicateRecord, shared.ErrCodeDuplicateRecord},
		{shared.ErrNoRouteOwner, shared.ErrCodeNoRouteOwner},
		{shared.ErrForwardFailed, shared.ErrCodeForwardFailed},
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
	original := shared.ErrTimeout.With("key1", "val1")
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
	before := shared.ErrThrottled.RetryAfter

	derived := shared.ErrThrottled.WithRetryAfter(10 * time.Second)

	if shared.ErrThrottled.RetryAfter != before {
		t.Fatalf("sentinel RetryAfter mutated: got %v, want %v", shared.ErrThrottled.RetryAfter, before)
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
			if !shared.IsRecoverableError(err) {
				t.Fatalf("design decision: unknown error %q should be treated as recoverable", err)
			}
		})
	}
}
