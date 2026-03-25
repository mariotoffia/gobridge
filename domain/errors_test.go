package domain_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
)

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

func TestBridgeError_Is(t *testing.T) {
	err := domain.ErrTimeout.Wrap(fmt.Errorf("deadline"))
	if !errors.Is(err, domain.ErrTimeout) {
		t.Fatal("expected errors.Is to match ErrTimeout")
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatal("should not match ErrNotFound")
	}
}

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

func TestBridgeError_WithMessage(t *testing.T) {
	e := domain.ErrUnavailable.WithMessage("broker down")
	if e.Message != "broker down" {
		t.Fatalf("expected custom message, got %q", e.Message)
	}
	if domain.ErrUnavailable.Message == "broker down" {
		t.Fatal("sentinel was mutated")
	}
}

func TestBridgeError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("root cause")
	e := domain.ErrNotFound.Wrap(cause)
	if errors.Unwrap(e) != cause {
		t.Fatal("Unwrap did not return cause")
	}
}

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

func TestNewBridgeError(t *testing.T) {
	e := domain.NewBridgeError("CUSTOM", domain.ErrorTransient, "custom error")
	if e.Code != "CUSTOM" || e.Class != domain.ErrorTransient {
		t.Fatalf("unexpected error: %+v", e)
	}
}

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

func TestErrMessageFiltered_Is(t *testing.T) {
	if !errors.Is(domain.ErrMessageFiltered, domain.ErrMessageFiltered) {
		t.Fatal("ErrMessageFiltered should match itself via errors.Is")
	}
	if errors.Is(domain.ErrMessageFiltered, domain.ErrNotFound) {
		t.Fatal("ErrMessageFiltered should not match ErrNotFound")
	}
}
