// Validates classification of AMQP 1.0 errors into domain BridgeError types.
package amqp10

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("MapError(nil) = %v, want nil", got)
	}
}

func TestMapError_AMQPConditions(t *testing.T) {
	tests := []struct {
		condition string
		wantCode  shared.ErrorCode
	}{
		{"amqp:not-found", shared.ErrCodeNotFound},
		{"amqp:unauthorized-access", shared.ErrCodeNotAuthorized},
		{"amqp:not-allowed", shared.ErrCodeForbidden},
		{"amqp:resource-limit-exceeded", shared.ErrCodeThrottled},
		{"amqp:connection:forced", shared.ErrCodeConnectionLost},
		{"amqp:connection:framing-error", shared.ErrCodeProtocolError},
		{"amqp:session:errant-link", shared.ErrCodeUnavailable},
		{"amqp:link:detach-forced", shared.ErrCodeConnectionLost},
		{"amqp:link:transfer-limit-exceeded", shared.ErrCodeThrottled},
		{"amqp:link:message-size-exceeded", shared.ErrCodePayloadTooLarge},
		{"amqp:internal-error", shared.ErrCodeUnavailable},
		{"amqp:not-implemented", shared.ErrCodeNotSupported},
		{"amqp:invalid-field", shared.ErrCodeInvalidPayload},
		{"amqp:decode-error", shared.ErrCodeProtocolError},
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
	if got.Code != shared.ErrCodeUnavailable {
		t.Fatalf("Code = %q, want %q for unknown condition", got.Code, shared.ErrCodeUnavailable)
	}
}

func TestMapError_ContextErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode shared.ErrorCode
	}{
		{
			name:     "verifies DeadlineExceeded maps to timeout",
			err:      context.DeadlineExceeded,
			wantCode: shared.ErrCodeTimeout,
		},
		{
			name:     "verifies wrapped DeadlineExceeded maps to timeout",
			err:      fmt.Errorf("op failed: %w", context.DeadlineExceeded),
			wantCode: shared.ErrCodeTimeout,
		},
		{
			name:     "verifies Canceled maps to unavailable",
			err:      context.Canceled,
			wantCode: shared.ErrCodeUnavailable,
		},
		{
			name:     "verifies wrapped Canceled maps to unavailable",
			err:      fmt.Errorf("canceled: %w", context.Canceled),
			wantCode: shared.ErrCodeUnavailable,
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
		wantCode shared.ErrorCode
	}{
		{"connection refused", shared.ErrCodeConnectionLost},
		{"network unreachable", shared.ErrCodeConnectionLost},
		{"reset by peer", shared.ErrCodeConnectionLost},
		{"broken pipe", shared.ErrCodeConnectionLost},
		{"unexpected eof", shared.ErrCodeConnectionLost},
		{"request throttled", shared.ErrCodeThrottled},
		{"server busy", shared.ErrCodeThrottled},
		{"system overload", shared.ErrCodeThrottled},
		{"too many requests", shared.ErrCodeThrottled},
		{"unauthorized access", shared.ErrCodeNotAuthorized},
		{"forbidden resource", shared.ErrCodeNotAuthorized},
		{"access denied", shared.ErrCodeNotAuthorized},
		{"entity not found", shared.ErrCodeNotFound},
		{"queue does not exist", shared.ErrCodeNotFound},
		{"HTTP 404", shared.ErrCodeNotFound},
		{"invalid message format", shared.ErrCodeInvalidPayload},
		{"malformed body", shared.ErrCodeInvalidPayload},
		{"bad request", shared.ErrCodeInvalidPayload},
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
	if got.Code != shared.ErrCodeUnavailable {
		t.Fatalf("Code = %q, want %q for unknown error", got.Code, shared.ErrCodeUnavailable)
	}
}
