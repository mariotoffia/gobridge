// ═══════════════════════════════════════════════
// Production-readiness remediation tests: publish fail-fast under broker
// flow control.
//
// Covers the HIGH finding — when the broker raises a resource alarm
// (connection.blocked), the SDK's PublishWithDeferredConfirmWithContext
// discards ctx and blocks indefinitely while holding the sender mutex,
// wedging every publisher past its deadline. Send/SendBatch must consult
// the session's blocked state and fail fast with ErrBrokerBusy instead of
// attempting the publish.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// blockedSenderSession returns a connected session pinned to the blocked
// state, plus the mock connection so callers can assert that no publish
// channel was ever opened (fail-fast happens before any SDK access).
func blockedSenderSession(t *testing.T) (*Session, *mockConnection) {
	t.Helper()
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	// Engage flow control on the current connection (generation guard in
	// setBlocked requires conn == s.conn, which holds right after Start).
	if !sess.setBlocked(mc, true, "low on memory") {
		t.Fatal("setBlocked on the current connection should be honoured")
	}
	return sess, mc
}

// TestSender_Send_BlockedBroker_FailsFast proves Send refuses to publish
// while the broker holds flow control, returning ErrBrokerBusy without
// opening a channel (which is where the SDK would otherwise wedge).
func TestSender_Send_BlockedBroker_FailsFast(t *testing.T) {
	sess, mc := blockedSenderSession(t)
	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk"})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("hi")})
	err := s.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: "rk"})

	var be *shared.BridgeError
	if !errors.As(err, &be) || be.Code != shared.ErrCodeBrokerBusy {
		t.Fatalf("Send returned %v, want ErrBrokerBusy", err)
	}
	if calls := mc.channelCalls(); calls != 0 {
		t.Fatalf("Send must fail fast before touching the broker: channelCalls = %d, want 0", calls)
	}
}

// TestSender_SendBatch_BlockedBroker_FailsFast proves SendBatch attributes
// ErrBrokerBusy to every message and never opens a channel while blocked.
func TestSender_SendBatch_BlockedBroker_FailsFast(t *testing.T) {
	sess, mc := blockedSenderSession(t)
	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk"})

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "a", Payload: []byte("1")}), Address: "rk"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b", Payload: []byte("2")}), Address: "rk"},
	}
	results, err := s.SendBatch(context.Background(), msgs)
	if err != nil {
		t.Fatalf("SendBatch top-level error = %v, want nil (per-index attribution)", err)
	}
	if len(results) != len(msgs) {
		t.Fatalf("results len = %d, want %d", len(results), len(msgs))
	}
	for i, r := range results {
		var be *shared.BridgeError
		if !errors.As(r.Err, &be) || be.Code != shared.ErrCodeBrokerBusy {
			t.Fatalf("results[%d].Err = %v, want ErrBrokerBusy", i, r.Err)
		}
		if r.Index != i {
			t.Fatalf("results[%d].Index = %d, want %d", i, r.Index, i)
		}
	}
	if calls := mc.channelCalls(); calls != 0 {
		t.Fatalf("SendBatch must fail fast before touching the broker: channelCalls = %d, want 0", calls)
	}
}

// TestSender_SendBatch_Mandatory_BlockedBroker_FailsFast proves the
// mandatory (sequential-fallback) batch path also fails fast: the blocked
// guard sits in SendBatch ahead of the per-message Send dispatch.
func TestSender_SendBatch_Mandatory_BlockedBroker_FailsFast(t *testing.T) {
	sess, mc := blockedSenderSession(t)
	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk", Mandatory: true})

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "a", Payload: []byte("1")}), Address: "rk"},
	}
	results, err := s.SendBatch(context.Background(), msgs)
	if err != nil {
		t.Fatalf("SendBatch top-level error = %v, want nil", err)
	}
	var be *shared.BridgeError
	if !errors.As(results[0].Err, &be) || be.Code != shared.ErrCodeBrokerBusy {
		t.Fatalf("results[0].Err = %v, want ErrBrokerBusy", results[0].Err)
	}
	if calls := mc.channelCalls(); calls != 0 {
		t.Fatalf("mandatory batch must fail fast: channelCalls = %d, want 0", calls)
	}
}
