package sqs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
)

// Verifies MapError returns nil for a nil input error.
func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// Verifies context deadline exceeded maps to ErrTimeout.
func TestMapError_ContextDeadline(t *testing.T) {
	err := MapError(context.DeadlineExceeded)
	if !errors.Is(err, domain.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

// Verifies context canceled maps to ErrUnavailable.
func TestMapError_ContextCanceled(t *testing.T) {
	err := MapError(context.Canceled)
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// Verifies QueueDoesNotExist maps to ErrNotFound with a stable message.
func TestMapError_QueueDoesNotExist(t *testing.T) {
	err := MapError(&sqstypes.QueueDoesNotExist{Message: strPtr("gone")})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err.Message != "queue does not exist" {
		t.Fatalf("unexpected message: %s", err.Message)
	}
}

// Verifies MessageNotInflight maps to ErrInvalidPayload.
func TestMapError_MessageNotInflight(t *testing.T) {
	err := MapError(&sqstypes.MessageNotInflight{Message: strPtr("nope")})
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// Verifies ReceiptHandleIsInvalid maps to ErrInvalidPayload.
func TestMapError_ReceiptHandleIsInvalid(t *testing.T) {
	err := MapError(&sqstypes.ReceiptHandleIsInvalid{Message: strPtr("bad")})
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got %v", err)
	}
}

// Verifies OverLimit maps to ErrThrottled.
func TestMapError_OverLimit(t *testing.T) {
	err := MapError(&sqstypes.OverLimit{Message: strPtr("too much")})
	if !errors.Is(err, domain.ErrThrottled) {
		t.Fatalf("expected ErrThrottled, got %v", err)
	}
}

// Verifies BatchRequestTooLong maps to ErrPayloadTooLarge.
func TestMapError_BatchRequestTooLong(t *testing.T) {
	err := MapError(&sqstypes.BatchRequestTooLong{Message: strPtr("big")})
	if !errors.Is(err, domain.ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

// Verifies UnsupportedOperation maps to ErrProtocolError.
func TestMapError_UnsupportedOperation(t *testing.T) {
	err := MapError(&sqstypes.UnsupportedOperation{Message: strPtr("nah")})
	if !errors.Is(err, domain.ErrProtocolError) {
		t.Fatalf("expected ErrProtocolError, got %v", err)
	}
}

// Verifies string-based error messages map to the expected domain error codes.
func TestMapError_StringPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   *domain.BridgeError
	}{
		{"throttle", "Throttling: Rate exceeded", domain.ErrThrottled},
		{"unavailable", "ServiceUnavailable", domain.ErrUnavailable},
		{"network", "connection refused", domain.ErrConnectionLost},
		{"auth", "AccessDenied for user", domain.ErrNotAuthorized},
		{"validation", "ValidationError: bad param", domain.ErrInvalidPayload},
		{"unknown", "something weird happened", domain.ErrUnavailable},
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

// Verifies IsRecoverableError classifies mapped SQS-related errors as expected.
func TestMapError_IsRecoverable(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		recoverable bool
	}{
		{"timeout", MapError(context.DeadlineExceeded), true},
		{"throttle", MapError(fmt.Errorf("Throttling")), true},
		{"not_found", MapError(&sqstypes.QueueDoesNotExist{Message: strPtr("x")}), false},
		{"bad_payload", MapError(&sqstypes.InvalidMessageContents{Message: strPtr("x")}), false},
		{"auth", MapError(fmt.Errorf("AccessDenied")), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.IsRecoverableError(tt.err); got != tt.recoverable {
				t.Fatalf("IsRecoverableError = %v, want %v", got, tt.recoverable)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
