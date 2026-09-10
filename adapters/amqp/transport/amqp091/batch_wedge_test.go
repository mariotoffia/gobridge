// ═══════════════════════════════════════════════
// Production-readiness remediation tests: pipelined SendBatch wedge +
// abandoned-publish budget (Chunk-11).
//
// the SDK's PublishWithDeferredConfirmWithContext IGNORES ctx and
// blocks while the broker holds connection.blocked flow control. The pipelined
// SendBatch loop issued it raw under the sender mutex, so a single mid-batch
// resource alarm wedged the batch AND every subsequent send, shutdown, and
// reconfiguration on that mutex. The loop now races each deferred publish
// against the batch deadline (awaitCall); on a wedge it abandons the channel to
// a reaper WITHOUT holding the mutex and fails the wedged message plus its
// unsent tail transient.
//
// an abandoned publish's reaper stays parked until the wedged publish
// finally returns (on a black-holed connection: never). Send/SendBatch now
// charge each abandon against a per-sender budget (MaxAbandonedPublishes) and
// fast-fail transient once it is exhausted, so a stuck connection cannot stack
// reapers without bound. The budget frees as the wedged publishes unblock.
//
// Both are driven deterministically via the publisherChannel seam
// (Sender.openChannel) with a channel whose deferred publish blocks on a
// release channel — no live broker, no sleep.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSender_SendBatch_PublishDeferredWedge_AbandonsChannelAndFailsTailTransient
// is the mutation catcher for the pipelined batch loop. A batch whose
// FIRST deferred publish wedges (ignores ctx, blocks) must:
//
//	(a) abandon the wedged channel — nil s.sc so the next send reopens a fresh
//	    one, and charge it against the abandoned budget;
//	(b) fail the wedged message AND every message after it TRANSIENT so the
//	    caller retries the unsent tail (not DLQ a message that merely hit flow
//	    control);
//	(c) release the sender mutex — a SUBSEQUENT Send makes progress on a fresh
//	    channel (holding the mutex across the reaper spawn would deadlock it);
//	(d) let the background reaper close the abandoned channel and FREE the
//	    budget once the wedged publish finally unblocks.
//
// Counterfactual (revert to a raw PublishDeferred under the mutex): SendBatch
// blocks on the wedged publish forever and this hangs.
func TestSender_SendBatch_PublishDeferredWedge_AbandonsChannelAndFailsTailTransient(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	release := make(chan struct{})
	started := make(chan struct{})
	wedged := newWedgeableChannel(nil)
	wedged.publishDeferred = func(context.Context) (pendingPublish, error) {
		close(started)
		<-release // exactly like PublishWithDeferredConfirmWithContext under flow control
		return nil, errors.New("unreachable: released only at test cleanup")
	}
	healthy := newWedgeableChannel(func(context.Context) (publishResult, error) {
		return publishResult{PublishOK: true, ConfirmedTag: 9}, nil
	})

	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk", Timeout: time.Minute, Clock: clocktest.New()})
	require.False(t, s.cfg.Mandatory, "this test pins the pipelined (non-mandatory) path")
	// openChannel is called under s.mu; the batch then the subsequent Send run
	// sequentially in this goroutine, so a plain counter is race-free.
	channels := []*wedgeableChannel{wedged, healthy}
	openCalls := 0
	s.openChannel = func(amqpConnection, bool) (publisherChannel, error) {
		ch := channels[openCalls]
		openCalls++
		return ch, nil
	}

	msgs := []ports.OutboundMessage{
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "a", Payload: []byte("1")}), Address: "rk"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "b", Payload: []byte("2")}), Address: "rk"},
		{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "c", Payload: []byte("3")}), Address: "rk"},
	}

	// The batch deadline is a CONTEXT deadline (applyTimeout → WithTimeout on
	// the deadline-less ctx), so cancelling this ctx forces the wedge timeout
	// deterministically — no real sleep.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	results, err := s.SendBatch(ctx, msgs)
	require.NoError(t, err, "SendBatch must not return a whole-batch error")
	require.Len(t, results, 3)

	// (b) the wedged message and its unsent tail are transient.
	for i, r := range results {
		require.Equal(t, i, r.Index, "results must stay index-aligned")
		var be *shared.BridgeError
		require.True(t, errors.As(r.Err, &be), "message %d must carry a classified error: %v", i, r.Err)
		require.Equal(t, shared.ErrorTransient, be.Class,
			"a wedged batch publish and its unsent tail must be transient so the caller retries")
	}

	// (a) the wedged channel was abandoned and charged against the budget.
	s.mu.Lock()
	scNil := s.sc == nil
	s.mu.Unlock()
	require.True(t, scNil, "the wedged channel must be dropped so a later send reopens a fresh one")
	require.Equal(t, int64(1), s.abandoned.Load(), "the wedged channel must be charged against the abandoned budget")
	require.Equal(t, 1, openCalls, "only the first (wedged) channel should have been opened during the batch")

	// (c) a subsequent Send makes progress on a FRESH channel — proving the
	// mutex was released, not held across the reaper spawn (else this deadlocks).
	require.NoError(t, s.Send(context.Background(), msgs[0]), "a later publish must succeed on a fresh channel")
	require.Equal(t, 2, openCalls, "the subsequent Send must open a fresh channel")

	// (d) the broker unblocks: the reaper closes the abandoned channel and
	// releases the budget.
	require.False(t, wedged.IsClosed(), "the wedged channel must not be closed until its publish unblocks")
	close(release)
	wait.RequireClosed(t, wedged.closed, time.Second)
	wait.Until(t, time.Second, "abandoned budget released once the wedged publish unblocks", func() bool {
		return s.abandoned.Load() == 0
	})
	require.False(t, healthy.IsClosed(), "the healthy channel must stay open")
}

// TestSender_AbandonedBudgetExhausted_Boundaries pins the guard arithmetic:
// exhausted at and above the cap, allowed below it, and disabled for a
// non-positive cap (unlimited — preserves the pre-fix default behaviour).
func TestSender_AbandonedBudgetExhausted_Boundaries(t *testing.T) {
	s := &Sender{}
	s.cfg.MaxAbandonedPublishes = 2

	s.abandoned.Store(0)
	require.False(t, s.abandonedBudgetExhausted(), "below cap must be allowed")
	s.abandoned.Store(1)
	require.False(t, s.abandonedBudgetExhausted(), "one below cap must be allowed")
	s.abandoned.Store(2)
	require.True(t, s.abandonedBudgetExhausted(), "at cap must be exhausted")
	s.abandoned.Store(3)
	require.True(t, s.abandonedBudgetExhausted(), "above cap must be exhausted")

	s.cfg.MaxAbandonedPublishes = 0
	s.abandoned.Store(100)
	require.False(t, s.abandonedBudgetExhausted(), "a zero cap disables the guard (unlimited)")
	s.cfg.MaxAbandonedPublishes = -1
	require.False(t, s.abandonedBudgetExhausted(), "a negative cap disables the guard (unlimited)")
}

// TestSender_AbandonReservedChannel_ReleasesBudgetOnReap proves the budget does
// not leak: a reservation charges it synchronously, the reaper keeps the channel
// open (and the budget charged) until the wedged publish returns, then closes
// the channel and RELEASES the reservation. Mutation (drop the release after
// reapWedgedChannel): the budget stays charged forever and the final Until
// times out.
func TestSender_AbandonReservedChannel_ReleasesBudgetOnReap(t *testing.T) {
	s := &Sender{}
	s.cfg.MaxAbandonedPublishes = 1

	done := make(chan struct{})
	fc := &recordingCloser{closed: make(chan struct{})}
	require.True(t, s.tryReserveAbandonedPublish(), "reserving must succeed below the cap")
	s.abandonReservedChannel(done, fc)

	require.Equal(t, int64(1), s.abandoned.Load(), "reserving must charge the budget synchronously")
	require.True(t, s.abandonedBudgetExhausted())

	// Still parked: the reaper must not close the channel until the wedged
	// publish returns (Close serialises on the SDK send mutex the publish holds).
	wait.Silent(t, fc.closed, 20*time.Millisecond)
	require.True(t, s.abandonedBudgetExhausted(), "the budget stays charged while the publish is wedged")

	// The publish returns: the reaper closes the channel and releases the budget.
	close(done)
	wait.RequireClosed(t, fc.closed, time.Second)
	wait.Until(t, time.Second, "budget released after the reaper closes the channel", func() bool {
		return s.abandoned.Load() == 0
	})
	require.False(t, s.abandonedBudgetExhausted())
}

// TestSender_AbandonedBudget_ExhaustedRefusesSendAndBatchThenRecovers proves
// the budget is actually consulted by BOTH publish entry points: with the
// budget exhausted, Send and SendBatch fast-fail transient BEFORE opening a
// channel; once the wedged publishes unblock and the reapers free the budget,
// publishing resumes.
//
// Counterfactual (remove the budget guard from Send/SendBatch): the exhausted
// entry points would proceed to open a channel (openCalls > 0) instead of
// refusing.
func TestSender_AbandonedBudget_ExhaustedRefusesSendAndBatchThenRecovers(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	healthy := newWedgeableChannel(func(context.Context) (publishResult, error) {
		return publishResult{PublishOK: true, ConfirmedTag: 1}, nil
	})
	openCalls := 0
	s := NewSender(SenderConfig{
		Session: sess, RoutingKey: "rk", Timeout: time.Minute,
		MaxAbandonedPublishes: 2, Clock: clocktest.New(),
	})
	s.openChannel = func(amqpConnection, bool) (publisherChannel, error) {
		openCalls++
		return healthy, nil
	}

	// Park two abandoned reapers (their wedged publishes have not returned, so
	// they stay blocked on done) to exhaust a budget of two.
	done1, done2 := make(chan struct{}), make(chan struct{})
	c1 := &recordingCloser{closed: make(chan struct{})}
	c2 := &recordingCloser{closed: make(chan struct{})}
	require.True(t, s.tryReserveAbandonedPublish())
	s.abandonReservedChannel(done1, c1)
	require.True(t, s.tryReserveAbandonedPublish())
	s.abandonReservedChannel(done2, c2)
	require.True(t, s.abandonedBudgetExhausted(), "two abandons must exhaust a budget of two")

	msg := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x", Payload: []byte("p")}),
		Address:  "rk",
	}

	// Send refuses fast with a transient ErrBrokerBusy — no channel opened.
	err := s.Send(context.Background(), msg)
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be), "Send must return a classified error, got %v", err)
	require.Equal(t, shared.ErrCodeBrokerBusy, be.Code, "an exhausted budget must fast-fail Send with ErrBrokerBusy")

	// SendBatch too — every message attributed ErrBrokerBusy.
	results, berr := s.SendBatch(context.Background(), []ports.OutboundMessage{msg, msg})
	require.NoError(t, berr)
	require.Len(t, results, 2)
	for i := range results {
		require.True(t, errors.As(results[i].Err, &be), "batch message %d must be classified: %v", i, results[i].Err)
		require.Equal(t, shared.ErrCodeBrokerBusy, be.Code, "batch message %d must fast-fail ErrBrokerBusy", i)
	}

	require.Equal(t, 0, openCalls, "an exhausted budget must refuse before opening any channel")

	// The broker recovers: the wedged publishes return, the reapers close their
	// channels and RELEASE the budget.
	close(done1)
	close(done2)
	wait.RequireClosed(t, c1.closed, time.Second)
	wait.RequireClosed(t, c2.closed, time.Second)
	wait.Until(t, time.Second, "budget freed once the reapers drain", func() bool {
		return !s.abandonedBudgetExhausted()
	})

	// Publishing resumes and now opens a channel.
	require.NoError(t, s.Send(context.Background(), msg), "after the budget frees, Send proceeds to the channel")
	require.Equal(t, 1, openCalls, "the post-recovery Send must open a channel")
}
