package paho

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// verifies MapError returns nil for a nil input error.
func TestMapError_Nil(t *testing.T) {
	if got := MapError(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// verifies MapError maps context.DeadlineExceeded to ErrTimeout.
func TestMapError_DeadlineExceeded(t *testing.T) {
	be := MapError(context.DeadlineExceeded)
	if be == nil {
		t.Fatal("expected BridgeError")
	}
	if !errors.Is(be, shared.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got code %s", be.Code)
	}
}

// verifies MapError maps context.Canceled to ErrUnavailable.
func TestMapError_Canceled(t *testing.T) {
	be := MapError(context.Canceled)
	if be == nil {
		t.Fatal("expected BridgeError")
	}
	if !errors.Is(be, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got code %s", be.Code)
	}
}

type fakeNetError struct{ timeout bool }

func (e *fakeNetError) Error() string   { return "net error" }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return false }

var _ net.Error = (*fakeNetError)(nil)

// verifies MapError maps net errors with Timeout() true to ErrTimeout.
func TestMapError_NetTimeout(t *testing.T) {
	be := MapError(&fakeNetError{timeout: true})
	if !errors.Is(be, shared.ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got code %s", be.Code)
	}
}

// verifies MapError maps non-timeout net errors to ErrConnectionLost.
func TestMapError_NetNonTimeout(t *testing.T) {
	be := MapError(&fakeNetError{timeout: false})
	if !errors.Is(be, shared.ErrConnectionLost) {
		t.Fatalf("expected ErrConnectionLost, got code %s", be.Code)
	}
}

// verifies MapError maps connection refused strings to ErrConnectionLost.
func TestMapError_ConnectionRefused(t *testing.T) {
	be := MapError(errors.New("connection refused"))
	if !errors.Is(be, shared.ErrConnectionLost) {
		t.Fatalf("expected ErrConnectionLost, got code %s", be.Code)
	}
}

// verifies MapError maps unrecognized errors to ErrUnavailable.
func TestMapError_UnknownFallsToUnavailable(t *testing.T) {
	be := MapError(errors.New("something weird"))
	if !errors.Is(be, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got code %s", be.Code)
	}
}

// verifies MapDisconnectReasonCode for MQTT disconnect reason bytes.
func TestMapDisconnectReasonCode(t *testing.T) {
	tests := []struct {
		code     byte
		wantNil  bool
		wantCode shared.ErrorCode
	}{
		{0x00, true, ""},
		{0x89, false, shared.ErrCodeBrokerBusy},
		{0x8E, false, shared.ErrCodeConnectionLost}, // session taken over (MQTT v5 §3.14.2.1)
		{0x8F, false, shared.ErrCodeInvalidTopic},   // topic filter invalid
		{0x93, false, shared.ErrCodeThrottled},
		{0x87, false, shared.ErrCodeNotAuthorized},
		{0x90, false, shared.ErrCodeInvalidTopic},
		{0x95, false, shared.ErrCodePayloadTooLarge},
		{0x81, false, shared.ErrCodeProtocolError},
		{0x9B, false, shared.ErrCodeQoSNotSupported},
		{0xFF, false, shared.ErrCodeUnavailable},
	}

	for _, tt := range tests {
		be := MapDisconnectReasonCode(tt.code)
		if tt.wantNil {
			if be != nil {
				t.Errorf("code 0x%02X: expected nil, got %v", tt.code, be)
			}
			continue
		}
		if be == nil {
			t.Errorf("code 0x%02X: expected error, got nil", tt.code)
			continue
		}
		if be.Code != tt.wantCode {
			t.Errorf("code 0x%02X: expected code %s, got %s", tt.code, tt.wantCode, be.Code)
		}
	}
}

// verifies MapPublishReasonCode for MQTT publish acknowledgment reason bytes.
func TestMapPublishReasonCode(t *testing.T) {
	tests := []struct {
		code     byte
		wantNil  bool
		wantCode shared.ErrorCode
	}{
		{0x00, true, ""},
		{0x10, true, ""},
		{0x87, false, shared.ErrCodeForbidden},
		{0x90, false, shared.ErrCodeInvalidTopic},
		{0x97, false, shared.ErrCodeThrottled},
		{0x99, false, shared.ErrCodeInvalidPayload},
		{0x80, false, shared.ErrCodeUnavailable},
		{0xFE, false, shared.ErrCodeUnavailable},
	}

	for _, tt := range tests {
		be := MapPublishReasonCode(tt.code)
		if tt.wantNil {
			if be != nil {
				t.Errorf("code 0x%02X: expected nil, got %v", tt.code, be)
			}
			continue
		}
		if be == nil {
			t.Errorf("code 0x%02X: expected error, got nil", tt.code)
			continue
		}
		if be.Code != tt.wantCode {
			t.Errorf("code 0x%02X: expected code %s, got %s", tt.code, tt.wantCode, be.Code)
		}
	}
}

// verifies MapSubscribeReasonCode for MQTT subscribe acknowledgment reason bytes.
func TestMapSubscribeReasonCode(t *testing.T) {
	tests := []struct {
		code     byte
		wantNil  bool
		wantCode shared.ErrorCode
	}{
		{0x00, true, ""},
		{0x01, true, ""},
		{0x02, true, ""},
		{0x87, false, shared.ErrCodeForbidden},
		{0x80, false, shared.ErrCodeUnavailable},
	}

	for _, tt := range tests {
		be := MapSubscribeReasonCode(tt.code)
		if tt.wantNil {
			if be != nil {
				t.Errorf("code 0x%02X: expected nil, got %v", tt.code, be)
			}
			continue
		}
		if be == nil {
			t.Errorf("code 0x%02X: expected error, got nil", tt.code)
			continue
		}
		if be.Code != tt.wantCode {
			t.Errorf("code 0x%02X: expected code %s, got %s", tt.code, tt.wantCode, be.Code)
		}
	}
}
