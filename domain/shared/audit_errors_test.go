package shared_test

import (
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════
// BridgeError Audit Tests
//
// Validates edge cases identified:
//   - Zero-value BridgeError.Error() returns empty string
//   - BridgeError.Is() matches by Code only (ignoring Class)
//   - NewBridgeError constructor correctness
//   - IsRecoverableError with nil and non-BridgeError inputs
// ═══════════════════════════════════════════════════════════════════

// TestBridgeError_ZeroValue validates that a zero-value BridgeError
// returns an empty string from Error().
func TestBridgeError_ZeroValue(t *testing.T) {
	var be shared.BridgeError
	got := be.Error()
	if got != "" {
		t.Fatalf("zero-value BridgeError.Error() should be empty, got %q", got)
	}
}

// TestBridgeError_Is_IgnoresClass validates that BridgeError.Is()
// compares only by Code, not by Class. Two errors with the same code
// but different classes match.
//
// ═══════════════════════════════════════════════════════════════════
// This documents a design choice: code-only comparison means a custom
// BridgeError with ErrCodeTimeout + ErrorPermanent will match
// ErrTimeout (which is ErrorTransient). Users relying on errors.Is()
// should be aware that class is not part of the identity.
// ═══════════════════════════════════════════════════════════════════
func TestBridgeError_Is_IgnoresClass_Documented(t *testing.T) {
	transient := &shared.BridgeError{
		Code:    shared.ErrCodeTimeout,
		Class:   shared.ErrorTransient,
		Message: "transient timeout",
	}
	permanent := &shared.BridgeError{
		Code:    shared.ErrCodeTimeout,
		Class:   shared.ErrorPermanent,
		Message: "permanent timeout",
	}

	if !errors.Is(transient, permanent) {
		t.Fatal("BridgeError.Is should match by Code regardless of Class")
	}
}

// TestNewBridgeError_Audit validates the constructor sets all fields.
func TestNewBridgeError_Audit(t *testing.T) {
	be := shared.NewBridgeError(shared.ErrCodeTimeout, shared.ErrorTransient, "test msg")
	if be.Code != shared.ErrCodeTimeout {
		t.Fatalf("expected code %s, got %s", shared.ErrCodeTimeout, be.Code)
	}
	if be.Class != shared.ErrorTransient {
		t.Fatalf("expected class %s, got %s", shared.ErrorTransient, be.Class)
	}
	if be.Message != "test msg" {
		t.Fatalf("expected message %q, got %q", "test msg", be.Message)
	}
}

// TestIsRecoverableError_Nil validates that nil returns false.
func TestIsRecoverableError_Nil(t *testing.T) {
	if shared.IsRecoverableError(nil) {
		t.Fatal("nil should not be recoverable")
	}
}

// TestIsRecoverableError_PlainError validates that a non-BridgeError
// is treated as recoverable (safe default).
func TestIsRecoverableError_PlainError(t *testing.T) {
	err := errors.New("some error")
	if !shared.IsRecoverableError(err) {
		t.Fatal("plain error should be treated as recoverable")
	}
}

// TestIsRecoverableError_PermanentBridgeError validates that a
// permanent BridgeError is not recoverable.
func TestIsRecoverableError_PermanentBridgeError(t *testing.T) {
	if shared.IsRecoverableError(shared.ErrNotAuthorized) {
		t.Fatal("permanent error should not be recoverable")
	}
}

// TestGetRetryAfter_Nil validates nil returns zero.
func TestGetRetryAfter_Nil(t *testing.T) {
	if shared.GetRetryAfter(nil) != 0 {
		t.Fatal("nil should return zero RetryAfter")
	}
}

// TestGetRetryAfter_PlainError validates non-BridgeError returns zero.
func TestGetRetryAfter_PlainError(t *testing.T) {
	if shared.GetRetryAfter(errors.New("plain")) != 0 {
		t.Fatal("plain error should return zero RetryAfter")
	}
}

// TestBridgeError_Unwrap_NilCause validates Unwrap returns nil when
// Cause is nil.
func TestBridgeError_Unwrap_NilCause(t *testing.T) {
	be := &shared.BridgeError{Code: shared.ErrCodeTimeout, Message: "test"}
	if be.Unwrap() != nil {
		t.Fatal("Unwrap should return nil when Cause is nil")
	}
}
