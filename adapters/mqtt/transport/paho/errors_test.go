package paho

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

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

// verifies a refused dial — which reaches MapError as a *net.OpError
// wrapping syscall.ECONNREFUSED, never as a bare string — classifies as
// ErrConnectionLost through the net.Error branch.
func TestMapError_ConnectionRefused(t *testing.T) {
	be := MapError(&net.OpError{
		Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1883},
		Err:  syscall.ECONNREFUSED,
	})
	if !errors.Is(be, shared.ErrConnectionLost) {
		t.Fatalf("expected ErrConnectionLost, got code %s", be.Code)
	}
}

// verifies a server CONNECT denial classifies from its typed MQTT v5
// reason code rather than from the SDK's error text. 0x88 is "server
// unavailable" — the only classification the deleted substring table
// ever added over the typed paths.
func TestMapError_ConnackDenial_ClassifiesByReasonCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		code byte
		want *shared.BridgeError
	}{
		{"server unavailable", 0x88, shared.ErrUnavailable},
		{"bad username or password", 0x86, shared.ErrNotAuthorized},
		{"not authorized", 0x87, shared.ErrNotAuthorized},
		{"server busy", 0x89, shared.ErrBrokerBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := MapError(&autopaho.ConnackError{
				ReasonCode: tc.code,
				Err:        errors.New("server denied connect"),
			})
			if !errors.Is(be, tc.want) {
				t.Fatalf("reason 0x%02X → %s, want %s", tc.code, be.Code, tc.want.Code)
			}
		})
	}
}

// verifies the SDK's typed link-down sentinels classify as
// ErrConnectionLost. These replace the "connection refused" /
// "no route to host" / "network unreachable" substring matches, none of
// which paho.golang ever emitted.
func TestMapError_TypedLinkDownSentinels(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"autopaho connection down", autopaho.ConnectionDownError},
		{"paho connection lost", pahov5.ErrConnectionLost},
		{"wrapped connection down", fmt.Errorf("publish: %w", autopaho.ConnectionDownError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := MapError(tc.err)
			if !errors.Is(be, shared.ErrConnectionLost) {
				t.Fatalf("%v → %s, want ErrConnectionLost", tc.err, be.Code)
			}
		})
	}
}

// verifies paho.ErrInvalidArguments — which autopaho itself refuses to
// retry — classifies permanently instead of falling through to the
// transient ErrUnavailable default.
func TestMapError_InvalidArgumentsIsPermanent(t *testing.T) {
	err := fmt.Errorf("%w: server does not support shared subscriptions", pahov5.ErrInvalidArguments)
	be := MapError(err)
	if !errors.Is(be, shared.ErrProtocolError) {
		t.Fatalf("ErrInvalidArguments → %s, want ErrProtocolError", be.Code)
	}
	if be.Class != shared.ErrorPermanent {
		t.Fatalf("ErrInvalidArguments class = %s, want %s", be.Class, shared.ErrorPermanent)
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
