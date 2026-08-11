// ═══════════════════════════════════════════════
// Adversarial-review remediation test: a mid-batch publish wedge must not mark
// the already-published (already-confirmed) prefix as failed.
//
// In a pipelined batch, messages 0..k-1 publish, message k wedges, and sendCtx
// expires. The pre-fix confirm loop then awaited the prefix confirms with the
// already-expired ctx, so even a confirm that had ALREADY arrived lost the
// ctx.Done() select race inside WaitContext and was reported transient — the
// caller retried and DUPLICATED an already-delivered message. The fix drains
// already-settled confirms via Settled() BEFORE honouring the expired context,
// so a delivered prefix keeps its real (success) outcome. Genuinely-unsettled
// confirms remain ambiguous (at-least-once) — see the ponytail ceiling in
// sendBatchPipelined.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// scriptedConfirm is a pendingPublish double with an injectable settled/wait
// outcome, so the batch confirm-drain logic can be exercised without a live
// broker. Its Wait deliberately honours ctx FIRST (like the raw SDK
// WaitContext) — an already-settled confirm is lost to an expired ctx unless the
// caller drains it via Settled() first, which is exactly the review-#6 fix.
type scriptedConfirm struct {
	tag        uint64
	settled    bool
	settledErr error
	waitErr    error
}

func (c *scriptedConfirm) DeliveryTag() uint64             { return c.tag }
func (c *scriptedConfirm) Settled() (done bool, err error) { return c.settled, c.settledErr }

func (c *scriptedConfirm) Wait(ctx context.Context) error {
	// The raw confirm wait races the confirm against ctx.Done(); an expired ctx
	// wins even when the confirm is already ready. This is the race the loop's
	// Settled()-first drain exists to avoid.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait for publish confirmation: %w", err)
	}
	return c.waitErr
}

var _ pendingPublish = (*scriptedConfirm)(nil)

// TestSender_SendBatch_PrefixConfirmDrainedBeforeExpiredCtx pins the review-#6
// fix: msg0 publishes and its confirm is already settled (positive); msg1's
// publish wedges and expires the batch deadline. The confirm loop then runs
// under the expired ctx, but msg0 MUST keep its success outcome (drained via
// Settled() before the ctx is honoured), while msg1 is transient so only the
// genuinely-unconfirmed tail is retried.
//
// Mutation (drop the Settled()-first block in the confirm loop, awaiting msg0 via
// Wait(sendCtx) with the ctx already expired): msg0's ready confirm loses the
// ctx.Done() race, results[0].Err is set transient, and the require.NoError
// below fails — modelling the duplicate-on-retry regression.
func TestSender_SendBatch_PrefixConfirmDrainedBeforeExpiredCtx(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	// msg0's publisher confirm has ALREADY arrived (positive) by the time the
	// batch deadline fires.
	confirmed := &scriptedConfirm{tag: 1, settled: true}

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var pdCalls atomic.Int64
	ch := newWedgeableChannel(nil) // batch path never calls PublishConfirmed
	ch.publishDeferred = func(context.Context) (pendingPublish, error) {
		switch pdCalls.Add(1) {
		case 1:
			return confirmed, nil // msg0: published, confirm already settled
		default:
			close(started)
			<-release // msg1: publish wedges (ignores ctx), expiring sendCtx
			return nil, context.Canceled
		}
	}

	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk", Timeout: time.Minute, Clock: clocktest.New()})
	s.openChannel = func(amqpConnection, bool) (publisherChannel, error) { return ch, nil }

	env0 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e0", Payload: []byte("a")})
	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("b")})
	msgs := []ports.OutboundMessage{
		{Envelope: env0, Address: "rk"},
		{Envelope: env1, Address: "rk"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel() // the deadline fires once msg1's publish is genuinely wedged
	}()

	results, err := s.SendBatch(ctx, msgs)
	require.NoError(t, err, "SendBatch never returns a top-level error (per-message attribution)")
	require.Len(t, results, 2)

	// msg0's confirm had already arrived: it MUST keep its real (success)
	// outcome, drained via Settled() BEFORE the expired ctx is honoured, so the
	// caller does not retry (and duplicate) an already-delivered message.
	require.NoError(t, results[0].Err,
		"an already-settled prefix confirm must be honoured before the expired deadline, not misreported transient")

	// msg1 genuinely wedged → transient so the caller retries only the tail.
	require.Error(t, results[1].Err, "the wedged tail message must fail")
	var be *shared.BridgeError
	require.True(t, errors.As(results[1].Err, &be))
	require.Equal(t, shared.ErrorTransient, be.Class, "a wedged tail publish must be transient")
}
