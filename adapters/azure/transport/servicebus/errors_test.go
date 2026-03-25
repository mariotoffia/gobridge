package servicebus

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
)

func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestMapError_ContextDeadline(t *testing.T) {
	err := MapError(context.DeadlineExceeded)
	if !errors.Is(err, domain.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestMapError_ContextCanceled(t *testing.T) {
	err := MapError(context.Canceled)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestMapError_MessageTooLarge(t *testing.T) {
	err := MapError(azservicebus.ErrMessageTooLarge)
	if !errors.Is(err, domain.ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
	if err.Message != "message too large" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

func TestMapError_ServiceBusCodeTimeout(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeTimeout})
	if !errors.Is(err, domain.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if err.Message != "operation timed out" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

func TestMapError_ServiceBusCodeConnectionLost(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeConnectionLost})
	if !errors.Is(err, domain.ErrConnectionLost) {
		t.Fatalf("expected ErrConnectionLost, got %v", err)
	}
}

func TestMapError_ServiceBusCodeLockLost(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeLockLost})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if err.Message != "message lock lost - message may be redelivered" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

func TestMapError_ServiceBusCodeUnauthorizedAccess(t *testing.T) {
	err := MapError(&azservicebus.Error{Code: azservicebus.CodeUnauthorizedAccess})
	if !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("expected ErrNotAuthorized, got %v", err)
	}
}

func TestMapError_StringPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   *domain.BridgeError
	}{
		{"connection_refused", "connection refused", domain.ErrConnectionLost},
		{"throttled", "throttled", domain.ErrThrottled},
		{"server_busy", "server busy", domain.ErrThrottled},
		{"unauthorized", "unauthorized", domain.ErrNotAuthorized},
		{"not_found", "not found", domain.ErrNotFound},
		{"invalid_message", "invalid message", domain.ErrInvalidPayload},
		{"unknown", "something weird", domain.ErrUnavailable},
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
			if got := domain.IsRecoverableError(tt.err); got != tt.recoverable {
				t.Fatalf("IsRecoverableError = %v, want %v", got, tt.recoverable)
			}
		})
	}
}
