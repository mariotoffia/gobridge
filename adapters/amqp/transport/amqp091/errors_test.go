package amqp091

import (
	"context"
	"errors"
	"fmt"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
)

// verifies MapError returns nil for a nil input.
func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// verifies MapError maps AMQP error codes to the expected BridgeError sentinels.
func TestMapError_AMQPCodes(t *testing.T) {
	tests := []struct {
		name string
		code int
		want *domain.BridgeError
	}{
		{"connection-forced", 320, domain.ErrConnectionLost},
		{"access-refused", 403, domain.ErrNotAuthorized},
		{"not-found", 404, domain.ErrNotFound},
		{"not-allowed", 405, domain.ErrForbidden},
		{"not-implemented-406", 406, domain.ErrNotSupported},
		{"frame-error", 501, domain.ErrProtocolError},
		{"syntax-error", 502, domain.ErrProtocolError},
		{"command-invalid", 503, domain.ErrProtocolError},
		{"channel-error", 504, domain.ErrUnavailable},
		{"unexpected-frame", 505, domain.ErrProtocolError},
		{"not-allowed-530", 530, domain.ErrForbidden},
		{"not-implemented-540", 540, domain.ErrNotSupported},
		{"internal-error", 541, domain.ErrUnavailable},
		{"unknown-code", 999, domain.ErrUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			amqpErr := &amqp.Error{Code: tt.code, Reason: tt.name}
			got := MapError(amqpErr)
			if !errors.Is(got, tt.want) {
				t.Fatalf("MapError(code=%d) = %v (code %s), want %v (code %s)",
					tt.code, got, got.Code, tt.want, tt.want.Code)
			}
			if got.Cause == nil {
				t.Error("Cause should wrap the original error")
			}
		})
	}
}

// verifies MapError maps context.DeadlineExceeded to ErrTimeout.
func TestMapError_ContextDeadlineExceeded(t *testing.T) {
	got := MapError(context.DeadlineExceeded)
	if !errors.Is(got, domain.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", got)
	}
}

// verifies MapError maps context.Canceled to ErrUnavailable.
func TestMapError_ContextCanceled(t *testing.T) {
	got := MapError(context.Canceled)
	if !errors.Is(got, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", got)
	}
}

// verifies MapError classifies network/connection string patterns correctly.
func TestMapError_StringPatterns(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   *domain.BridgeError
	}{
		{"connection_refused", "connection refused", domain.ErrConnectionLost},
		{"no_route", "no route to host", domain.ErrConnectionLost},
		{"network_unreachable", "network unreachable", domain.ErrConnectionLost},
		{"connection_reset", "connection reset by peer", domain.ErrConnectionLost},
		{"timeout", "operation timeout", domain.ErrTimeout},
		{"timed_out", "request timed out", domain.ErrTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapError(fmt.Errorf("%s", tt.errMsg))
			if !errors.Is(got, tt.want) {
				t.Fatalf("MapError(%q) = code %s, want code %s",
					tt.errMsg, got.Code, tt.want.Code)
			}
		})
	}
}

// verifies MapError defaults unrecognized errors to ErrUnavailable.
func TestMapError_UnknownError(t *testing.T) {
	got := MapError(fmt.Errorf("something completely unexpected"))
	if !errors.Is(got, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got code %s", got.Code)
	}
}

// verifies MapError wraps context errors even when wrapped in another error.
func TestMapError_WrappedContextError(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	got := MapError(wrapped)
	if !errors.Is(got, domain.ErrTimeout) {
		t.Fatalf("expected ErrTimeout for wrapped deadline, got code %s", got.Code)
	}
}

// verifies domain.IsRecoverableError for errors produced by MapError.
func TestMapError_IsRecoverable(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		recoverable bool
	}{
		{"timeout", MapError(context.DeadlineExceeded), true},
		{"connection_lost", MapError(&amqp.Error{Code: 320}), true},
		{"unavailable", MapError(fmt.Errorf("something weird")), true},
		{"not_authorized", MapError(&amqp.Error{Code: 403}), false},
		{"not_found", MapError(&amqp.Error{Code: 404}), false},
		{"protocol_error", MapError(&amqp.Error{Code: 501}), false},
		{"forbidden", MapError(&amqp.Error{Code: 405}), false},
		{"not_supported", MapError(&amqp.Error{Code: 406}), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.IsRecoverableError(tt.err); got != tt.recoverable {
				t.Fatalf("IsRecoverableError = %v, want %v", got, tt.recoverable)
			}
		})
	}
}
