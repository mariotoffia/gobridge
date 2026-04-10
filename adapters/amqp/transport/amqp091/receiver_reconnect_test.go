// ═══════════════════════════════════════════════
// Receiver Reconnect & Error Classification Tests
//
// Validates isEmitError and waitForReconnect edge cases.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// TestReceiver_IsEmitError_TransportBridgeError validates that a transport
// BridgeError (not wrapped in emitError) is classified as transport error.
func TestReceiver_IsEmitError_TransportBridgeError(t *testing.T) {
	r := &Receiver{}
	err := domain.ErrConnectionLost.WithMessage("test")

	if r.isEmitError(err) {
		t.Fatal("unwrapped BridgeError should be classified as transport error (isEmitError=false)")
	}
}

// TestReceiver_IsEmitError_EmitWrapped validates that an emit-wrapped
// error is classified as an emit callback error (returns true).
func TestReceiver_IsEmitError_EmitWrapped(t *testing.T) {
	r := &Receiver{}
	err := &emitError{err: errors.New("emit callback failed")}

	if !r.isEmitError(err) {
		t.Fatal("emitError-wrapped error should be classified as emit error (isEmitError=true)")
	}
}

// TestReceiver_IsEmitError_EmitWrappedBridgeError validates that a
// BridgeError wrapped in emitError is correctly classified as emit error.
func TestReceiver_IsEmitError_EmitWrappedBridgeError(t *testing.T) {
	r := &Receiver{}
	inner := domain.ErrInvalidPayload.WithMessage("bad data")
	err := &emitError{err: inner}

	if !r.isEmitError(err) {
		t.Fatal("emitError-wrapped BridgeError should be classified as emit error")
	}
}

// TestReceiver_IsEmitError_PlainError validates that a plain error
// (not wrapped in emitError) is classified as transport error.
func TestReceiver_IsEmitError_PlainError(t *testing.T) {
	r := &Receiver{}
	err := errors.New("some raw error")

	if r.isEmitError(err) {
		t.Fatal("plain error should be classified as transport error (isEmitError=false)")
	}
}

// TestReceiver_WaitForReconnect_ContextCancel validates that
// waitForReconnect returns false when context is cancelled.
func TestReceiver_WaitForReconnect_ContextCancel(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		domain.SessionEphemeral,
		slog.Default(),
	)
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "test-queue"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := r.waitForReconnect(ctx)
	if result {
		t.Fatal("waitForReconnect should return false when context is cancelled")
	}

	sess.Close(context.Background())
}

// TestReceiver_WaitForReconnect_EventReceived validates that
// waitForReconnect returns true when SessionConnected event arrives.
func TestReceiver_WaitForReconnect_EventReceived(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		domain.SessionEphemeral,
		slog.Default(),
	)
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "test-queue"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		sess.pushEvent(ports.SessionConnected, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := r.waitForReconnect(ctx)
	if !result {
		t.Fatal("waitForReconnect should return true when SessionConnected event received")
	}

	sess.Close(context.Background())
}

// TestReceiver_WaitForReconnect_ChannelClosed validates that
// waitForReconnect returns false when events channel is closed.
func TestReceiver_WaitForReconnect_ChannelClosed(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		domain.SessionEphemeral,
		slog.Default(),
	)
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "test-queue"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		sess.Close(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := r.waitForReconnect(ctx)
	if result {
		t.Fatal("waitForReconnect should return false when events channel is closed")
	}
}

// TestReceiver_NilSession validates waitForReconnect with nil session.
func TestReceiver_NilSession(t *testing.T) {
	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "test-queue"},
		session: nil,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	result := r.waitForReconnect(context.Background())
	if result {
		t.Fatal("waitForReconnect should return false with nil session")
	}
}
