// Validates classification of AMQP 1.0 errors into domain BridgeError types.
package amqp10

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain"
)

func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("MapError(nil) = %v, want nil", got)
	}
}

func TestMapError_AMQPConditions(t *testing.T) {
	tests := []struct {
		condition string
		wantCode  domain.ErrorCode
	}{
		{"amqp:not-found", domain.ErrCodeNotFound},
		{"amqp:unauthorized-access", domain.ErrCodeNotAuthorized},
		{"amqp:not-allowed", domain.ErrCodeForbidden},
		{"amqp:resource-limit-exceeded", domain.ErrCodeThrottled},
		{"amqp:connection:forced", domain.ErrCodeConnectionLost},
		{"amqp:connection:framing-error", domain.ErrCodeProtocolError},
		{"amqp:session:errant-link", domain.ErrCodeUnavailable},
		{"amqp:link:detach-forced", domain.ErrCodeConnectionLost},
		{"amqp:link:transfer-limit-exceeded", domain.ErrCodeThrottled},
		{"amqp:link:message-size-exceeded", domain.ErrCodePayloadTooLarge},
		{"amqp:internal-error", domain.ErrCodeUnavailable},
		{"amqp:not-implemented", domain.ErrCodeNotSupported},
		{"amqp:invalid-field", domain.ErrCodeInvalidPayload},
		{"amqp:decode-error", domain.ErrCodeProtocolError},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			amqpErr := &amqp.Error{
				Condition:   amqp.ErrCond(tt.condition),
				Description: "test description",
			}

			got := MapError(amqpErr)
			if got == nil {
				t.Fatal("MapError returned nil for AMQP error")
			}
			if got.Code != tt.wantCode {
				t.Fatalf("Code = %q, want %q", got.Code, tt.wantCode)
			}
			if got.Cause == nil {
				t.Fatal("Cause should wrap the original AMQP error")
			}
		})
	}
}

func TestMapError_AMQPCondition_Unknown(t *testing.T) {
	amqpErr := &amqp.Error{
		Condition:   amqp.ErrCond("amqp:vendor:custom-condition"),
		Description: "vendor specific",
	}

	got := MapError(amqpErr)
	if got == nil {
		t.Fatal("MapError returned nil")
	}
	if got.Code != domain.ErrCodeUnavailable {
		t.Fatalf("Code = %q, want %q for unknown condition", got.Code, domain.ErrCodeUnavailable)
	}
}

func TestMapError_ContextErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode domain.ErrorCode
	}{
		{
			name:     "verifies DeadlineExceeded maps to timeout",
			err:      context.DeadlineExceeded,
			wantCode: domain.ErrCodeTimeout,
		},
		{
			name:     "verifies wrapped DeadlineExceeded maps to timeout",
			err:      fmt.Errorf("op failed: %w", context.DeadlineExceeded),
			wantCode: domain.ErrCodeTimeout,
		},
		{
			name:     "verifies Canceled maps to unavailable",
			err:      context.Canceled,
			wantCode: domain.ErrCodeUnavailable,
		},
		{
			name:     "verifies wrapped Canceled maps to unavailable",
			err:      fmt.Errorf("canceled: %w", context.Canceled),
			wantCode: domain.ErrCodeUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapError(tt.err)
			if got == nil {
				t.Fatal("MapError returned nil")
			}
			if got.Code != tt.wantCode {
				t.Fatalf("Code = %q, want %q", got.Code, tt.wantCode)
			}
		})
	}
}

func TestMapError_StringPatterns(t *testing.T) {
	tests := []struct {
		msg      string
		wantCode domain.ErrorCode
	}{
		{"connection refused", domain.ErrCodeConnectionLost},
		{"network unreachable", domain.ErrCodeConnectionLost},
		{"reset by peer", domain.ErrCodeConnectionLost},
		{"broken pipe", domain.ErrCodeConnectionLost},
		{"unexpected eof", domain.ErrCodeConnectionLost},
		{"request throttled", domain.ErrCodeThrottled},
		{"server busy", domain.ErrCodeThrottled},
		{"system overload", domain.ErrCodeThrottled},
		{"too many requests", domain.ErrCodeThrottled},
		{"unauthorized access", domain.ErrCodeNotAuthorized},
		{"forbidden resource", domain.ErrCodeNotAuthorized},
		{"access denied", domain.ErrCodeNotAuthorized},
		{"HTTP 401", domain.ErrCodeNotAuthorized},
		{"HTTP 403", domain.ErrCodeNotAuthorized},
		{"entity not found", domain.ErrCodeNotFound},
		{"queue does not exist", domain.ErrCodeNotFound},
		{"HTTP 404", domain.ErrCodeNotFound},
		{"invalid message format", domain.ErrCodeInvalidPayload},
		{"malformed body", domain.ErrCodeInvalidPayload},
		{"bad request", domain.ErrCodeInvalidPayload},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			got := MapError(errors.New(tt.msg))
			if got == nil {
				t.Fatal("MapError returned nil")
			}
			if got.Code != tt.wantCode {
				t.Fatalf("Code = %q, want %q for message %q", got.Code, tt.wantCode, tt.msg)
			}
		})
	}
}

func TestMapError_Default(t *testing.T) {
	got := MapError(errors.New("some completely unknown error"))
	if got == nil {
		t.Fatal("MapError returned nil")
	}
	if got.Code != domain.ErrCodeUnavailable {
		t.Fatalf("Code = %q, want %q for unknown error", got.Code, domain.ErrCodeUnavailable)
	}
}
