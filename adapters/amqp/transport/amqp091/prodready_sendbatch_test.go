// ═══════════════════════════════════════════════
// Production-readiness remediation tests: pipelined SendBatch (F6).
//
// SendBatch used to hold the sender mutex across each publish+confirm
// round-trip — strictly sequential, ~10 msg/s on a 100ms-RTT link. The
// non-mandatory path is now pipelined (publish all with deferred
// confirms, then await them). End-to-end throughput is proven against a
// real broker (integration tests); these unit tests pin the pieces that
// are deterministic without a broker:
//
//   - per-index error attribution in the pipelined path (a failed
//     message must not shift or consume a sibling's confirmation),
//   - the mandatory path stays sequential (basic.return carries no
//     delivery tag, so attribution requires one-in-flight ordering).
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSender_SendBatchPipelined_PerIndexAttribution mixes invalid and
// valid-but-unsendable messages and asserts every index carries exactly
// its own failure, with no whole-batch error.
func TestSender_SendBatchPipelined_PerIndexAttribution(t *testing.T) {
	// No exchange default, no session: index 0 fails validation (nil
	// envelope), index 1 fails routing-key resolution, index 2 passes
	// validation and fails at publish (no session).
	s := NewSender(SenderConfig{})
	require.False(t, s.cfg.Mandatory, "this test pins the pipelined (non-mandatory) path")

	msgs := []ports.OutboundMessage{
		{Envelope: nil, Address: "rk"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "no-rk", Payload: []byte("a")})},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok", Payload: []byte("b")}), Address: "rk"},
	}

	results, err := s.SendBatch(context.Background(), msgs)
	require.NoError(t, err, "SendBatch must not return a whole-batch error")
	require.Len(t, results, len(msgs))

	for i, r := range results {
		require.Equal(t, i, r.Index, "results must stay index-aligned")
		require.Error(t, r.Err, "message %d must carry its own error", i)
	}

	var be *shared.BridgeError
	require.True(t, errors.As(results[0].Err, &be))
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code, "nil envelope -> invalid payload")

	require.True(t, errors.As(results[1].Err, &be))
	require.Equal(t, shared.ErrCodeInvalidTopic, be.Code, "missing routing key -> invalid topic")

	require.True(t, errors.As(results[2].Err, &be))
	require.Equal(t, shared.ErrCodeUnavailable, be.Code, "no session -> unavailable")
}

// TestSender_SendBatch_Mandatory_StaysSequential pins the deliberate
// fallback: with mandatory=true each message goes through the one-in-
// flight Send path (correct basic.return attribution beats throughput).
func TestSender_SendBatch_Mandatory_StaysSequential(t *testing.T) {
	s := NewSender(SenderConfig{Mandatory: true})

	msgs := []ports.OutboundMessage{
		{Envelope: nil},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m", Payload: []byte("a")}), Address: "rk"},
	}

	results, err := s.SendBatch(context.Background(), msgs)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Error(t, results[0].Err)
	require.Error(t, results[1].Err) // no session
	require.Equal(t, 0, results[0].Index)
	require.Equal(t, 1, results[1].Index)
}

// TestSender_SendBatchPipelined_MidBatchBlock_FailsRemainderFast is the M-3
// regression: SendBatch only checks broker flow control at entry, but a
// resource alarm can engage mid-batch. The pipelined loop must re-check
// blockedState per message so the remainder fails fast with ErrBrokerBusy
// instead of wedging the next publish under the sender mutex (the SDK's
// deferred-publish path ignores ctx while the broker holds backpressure).
//
// The first channel open engages flow control (simulating a mid-batch
// alarm) and then fails, so no real SDK channel is dereferenced. With the
// per-message re-check, only message 0 reaches the channel open; messages 1
// and 2 are refused fast.
//
// Counterfactual (remove the per-message re-check): messages 1 and 2 each
// call ensureChannelLocked, so chCalls == 3 and their errors are the mapped
// channel-open failure (ErrUnavailable), not ErrBrokerBusy.
func TestSender_SendBatchPipelined_MidBatchBlock_FailsRemainderFast(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	var chCalls int
	mc.ChannelFn = func() (*amqpChannel, error) {
		chCalls++
		if !sess.setBlocked(mc, true, "low on memory") {
			t.Error("setBlocked on the current connection should be honoured")
		}
		return nil, errors.New("channel open failed")
	}

	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk"})
	require.False(t, s.cfg.Mandatory, "this test pins the pipelined (non-mandatory) path")

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "a", Payload: []byte("1")}), Address: "rk"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b", Payload: []byte("2")}), Address: "rk"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "c", Payload: []byte("3")}), Address: "rk"},
	}

	results, err := s.SendBatch(context.Background(), msgs)
	require.NoError(t, err, "SendBatch must not return a whole-batch error")
	require.Len(t, results, 3)

	require.Equal(t, 1, chCalls,
		"only message 0 should reach the channel open; the rest must fail fast on the re-check")
	require.Error(t, results[0].Err, "message 0's channel open failed")

	for i := 1; i < 3; i++ {
		var be *shared.BridgeError
		require.True(t, errors.As(results[i].Err, &be), "message %d error: %v", i, results[i].Err)
		require.Equal(t, shared.ErrCodeBrokerBusy, be.Code,
			"message %d must fail fast with ErrBrokerBusy after the mid-batch alarm", i)
	}
}
