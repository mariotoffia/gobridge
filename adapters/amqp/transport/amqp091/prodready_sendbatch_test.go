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
