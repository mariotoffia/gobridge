// ═══════════════════════════════════════════════
// Production-readiness remediation tests: sender reconnect-window
// classification.
//
// During the reconnect window the session installs the connection (so a
// concurrent Close can tear it down) but keeps connected=false until
// reconcile restores the topology. A publish that races that window used to
// open a channel on the incomplete topology and see a 404 NOT_FOUND (or an
// unroutable mandatory return) — classified Permanent — which DLQs/drops a
// message that is fine to retry once reconcile completes. The receiver
// already tolerates this window; the sender did not.
//
// The fix gates ensureChannelLocked on Session.connectionIfReady, which
// returns nil while connected=false, so the sender returns a transient
// ErrUnavailable without opening a channel or publishing.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSender_Send_ReconnectWindow_ClassifiesTransient pins: a publish
// racing the reconnect window must see a TRANSIENT (retryable) classification
// and must NOT open a channel / publish into the incomplete topology.
//
// Counterfactual (revert ensureChannelLocked to Session.Connection()): the
// sender opens a channel (channelCalls==1) and — because the mock models the
// broker rejecting the pre-topology operation with 404 — classifies the error
// Permanent (ErrNotFound), the DLQ/drop path.
func TestSender_Send_ReconnectWindow_ClassifiesTransient(t *testing.T) {
	mc := newMockConnection()
	// Should the sender ever reach channel-open during the window, model the
	// broker rejecting the pre-topology operation: 404 -> Permanent under the
	// old Connection() gate.
	mc.ChannelFn = func() (*amqpChannel, error) {
		return nil, &amqp.Error{Code: 404, Reason: "NOT_FOUND - no exchange 'ex'"}
	}
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	// Enter the reconnect window: connection installed, reconcile not yet
	// complete — exactly the state session.go holds between `s.conn = conn`
	// and `s.connected = true` on Start and on doReconnect.
	sess.mu.Lock()
	sess.connected = false
	sess.mu.Unlock()

	snd := NewSender(SenderConfig{Exchange: "ex", RoutingKey: "rk", Session: sess, Timeout: time.Second})

	err := snd.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "w1", Payload: []byte("x")}),
	})

	var be *shared.BridgeError
	if !errors.As(err, &be) {
		t.Fatalf("Send returned %v, want a classified BridgeError", err)
	}
	if be.Class == shared.ErrorPermanent {
		t.Fatalf("Send in reconnect window classified Permanent (%v): would DLQ/drop a retryable publish", err)
	}
	if be.Code != shared.ErrCodeUnavailable {
		t.Fatalf("Send in reconnect window = %v, want ErrUnavailable (transient)", err)
	}
	// The sender must NOT have opened a channel / published into the window.
	if calls := mc.channelCalls(); calls != 0 {
		t.Fatalf("sender opened a channel during the reconnect window: channelCalls = %d, want 0", calls)
	}
}

// TestSender_Send_ReadyAfterReconcile_Publishes is the positive control: once
// the session reports connected (reconcile done), the sender opens a channel
// on the live connection. It proves the connectionIfReady gate does not wedge
// the steady state — only the pre-reconcile window is refused.
func TestSender_Send_ReadyAfterReconcile_Publishes(t *testing.T) {
	mc := newMockConnection()
	channelAttempted := make(chan struct{}, 1)
	mc.ChannelFn = func() (*amqpChannel, error) {
		select {
		case channelAttempted <- struct{}{}:
		default:
		}
		// A nil *amqpChannel with a sentinel error keeps this SDK-free while
		// still proving the sender proceeded PAST the readiness gate to
		// channel-open (unlike the window test, where it must not).
		return nil, errors.New("channel open attempted")
	}
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()
	// Start leaves connected=true (no plan to reconcile), i.e. steady state.

	snd := NewSender(SenderConfig{Exchange: "ex", RoutingKey: "rk", Session: sess, Timeout: time.Second})
	_ = snd.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "r1", Payload: []byte("x")}),
	})

	if calls := mc.channelCalls(); calls != 1 {
		t.Fatalf("ready sender did not open a channel: channelCalls = %d, want 1", calls)
	}
}
