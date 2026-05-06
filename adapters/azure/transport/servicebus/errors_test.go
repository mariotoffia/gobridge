package servicebus

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// verifies MapError returns nil for a nil input error.
func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// verifies MapError maps context deadline exceeded to ErrTimeout.
func TestMapError_ContextDeadline(t *testing.T) {
	err := MapError(context.DeadlineExceeded)
	if !errors.Is(err, shared.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

// verifies MapError maps context canceled to ErrUnavailable.
func TestMapError_ContextCanceled(t *testing.T) {
	err := MapError(context.Canceled)
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// verifies MapError maps ErrMessageTooLarge to ErrPayloadTooLarge with the expected message.
func TestMapError_MessageTooLarge(t *testing.T) {
	err := MapError(azservicebus.ErrMessageTooLarge)
	if !errors.Is(err, shared.ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
	if err.Message != "message too large" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

// verifies MapError maps Service Bus CodeTimeout to ErrTimeout.
func TestMapError_ServiceBusCodeTimeout(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeTimeout})
	if !errors.Is(err, shared.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if err.Message != "operation timed out" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

// verifies MapError maps CodeConnectionLost to ErrConnectionLost.
func TestMapError_ServiceBusCodeConnectionLost(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeConnectionLost})
	if !errors.Is(err, shared.ErrConnectionLost) {
		t.Fatalf("expected ErrConnectionLost, got %v", err)
	}
}

// verifies MapError maps CodeLockLost to ErrUnavailable with a lock-lost message.
func TestMapError_ServiceBusCodeLockLost(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeLockLost})
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if err.Message != "message lock lost - message may be redelivered" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

// verifies MapError maps CodeUnauthorizedAccess to ErrNotAuthorized.
func TestMapError_ServiceBusCodeUnauthorizedAccess(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeUnauthorizedAccess})
	if !errors.Is(err, shared.ErrNotAuthorized) {
		t.Fatalf("expected ErrNotAuthorized, got %v", err)
	}
}

// verifies MapError classifies generic errors by substring patterns.
func TestMapError_StringPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   *shared.BridgeError
	}{
		{"connection_refused", "connection refused", shared.ErrConnectionLost},
		{"throttled", "throttled", shared.ErrThrottled},
		{"server_busy", "server busy", shared.ErrThrottled},
		{"unauthorized", "unauthorized", shared.ErrNotAuthorized},
		{"not_found", "not found", shared.ErrNotFound},
		{"invalid_message", "invalid message", shared.ErrInvalidPayload},
		{"unknown", "something weird", shared.ErrUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MapError(fmt.Errorf("%s", tt.errMsg))
			if !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want.Code, err.Code)
			}
		})
	}
}

// verifies shared.IsRecoverableError for errors produced by MapError.
func TestMapError_IsRecoverable(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		recoverable bool
	}{
		{"timeout", MapError(context.DeadlineExceeded), true},
		{"connection_lost", MapError(&azservicebus.Error{Code: azservicebus.CodeConnectionLost}), true},
		{"throttled", MapError(fmt.Errorf("throttled")), true},
		{"not_authorized", MapError(&azservicebus.Error{Code: azservicebus.CodeUnauthorizedAccess}), false},
		{"not_found", MapError(fmt.Errorf("not found")), false},
		{"payload_too_large", MapError(azservicebus.ErrMessageTooLarge), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shared.IsRecoverableError(tt.err); got != tt.recoverable {
				t.Fatalf("IsRecoverableError = %v, want %v", got, tt.recoverable)
			}
		})
	}
}
