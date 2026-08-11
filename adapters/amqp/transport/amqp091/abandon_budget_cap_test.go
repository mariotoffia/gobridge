// ═══════════════════════════════════════════════
// Adversarial-review remediation tests: the abandoned-publish budget is a HARD
// cap on already-admitted callers, not a TOCTOU entry check.
//
// The pre-fix guard checked abandonedBudgetExhausted() OUTSIDE s.mu at entry,
// then charged the budget only AFTER a publish wedged. N callers arriving while
// abandoned==0 all passed the entry check, queued behind s.mu, and each charged
// on timeout → abandoned grew far past MaxAbandonedPublishes. The entry check
// only bounded callers arriving AFTER exhaustion.
//
// The fix reserves the budget atomically UNDER s.mu, BEFORE the wedgeable
// publish (tryReserveAbandonedPublish). Because Send/SendBatch hold s.mu across
// the whole publish, every admitted caller's reservation observes the prior
// charges, so the cap bounds admitted callers — not just late arrivals. That
// end-to-end property is the composition of two independently-verified
// invariants below: a hard cap that never overshoots (TestSender_TryReserve*)
// and a charge issued BEFORE the publish, under the lock, so the next serialized
// caller sees it (TestSender_Send_ReservesBudgetBeforePublish).
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSender_TryReserveAbandonedPublish_HardCap pins the deterministic hard-cap
// invariant: exactly cap reservations may be outstanding, the count never
// overshoots, and releasing a slot admits exactly one more. Mutation (drop the
// `cur >= limit` guard so the CAS always succeeds): the (cap+1)th reservation is
// admitted and abandoned overshoots — both requires below fail.
func TestSender_TryReserveAbandonedPublish_HardCap(t *testing.T) {
	const cap = 3
	s := &Sender{}
	s.cfg.MaxAbandonedPublishes = cap

	for i := 1; i <= cap; i++ {
		require.Truef(t, s.tryReserveAbandonedPublish(),
			"reservation %d of %d must be admitted", i, cap)
		require.Equal(t, int64(i), s.abandoned.Load())
	}
	// At the cap every further reservation is refused and the count is pinned.
	require.False(t, s.tryReserveAbandonedPublish(), "the (cap+1)th reservation must be refused")
	require.False(t, s.tryReserveAbandonedPublish(), "the cap is a hard ceiling, not a one-shot")
	require.Equal(t, int64(cap), s.abandoned.Load(), "a refused reservation must not charge the budget")

	// Releasing one slot admits exactly one more, never more.
	s.releaseAbandonedPublish()
	require.Equal(t, int64(cap-1), s.abandoned.Load())
	require.True(t, s.tryReserveAbandonedPublish(), "a freed slot must admit one more")
	require.False(t, s.tryReserveAbandonedPublish(), "only the freed slot is re-admitted")
	require.Equal(t, int64(cap), s.abandoned.Load())
}

// TestSender_TryReserveAbandonedPublish_NeverExceedsCapUnderContention races
// many callers at the reservation primitive and asserts the count never exceeds
// the cap (the atomic CAS is what guarantees it). Run under -race. Mutation
// (drop the cap guard): all goroutines are admitted, succeeded and abandoned
// both blow past the cap, and the LessOrEqual assertions fail.
func TestSender_TryReserveAbandonedPublish_NeverExceedsCapUnderContention(t *testing.T) {
	const cap = 8
	const goroutines = 100

	s := &Sender{}
	s.cfg.MaxAbandonedPublishes = cap

	start := make(chan struct{})
	var succeeded atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // barrier: release all callers at once to maximise contention
			if s.tryReserveAbandonedPublish() {
				succeeded.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.LessOrEqual(t, succeeded.Load(), int64(cap),
		"no more than cap reservations may ever be admitted, even under contention")
	require.LessOrEqual(t, s.abandoned.Load(), int64(cap),
		"the charged count must never exceed the cap")
	require.Equal(t, succeeded.Load(), s.abandoned.Load(),
		"the charged count must equal the number of admitted reservations")
	// With goroutines >> cap the cap is certainly saturated.
	require.Equal(t, int64(cap), succeeded.Load(), "all cap slots are taken when demand exceeds the cap")
}

// TestSender_Send_ReservesBudgetBeforePublish proves the reservation is charged
// BEFORE the wedgeable publish, under s.mu — not after the timeout unlocks.
// While the publish is genuinely wedged (started, not yet returned), abandoned
// must already be 1. Because Send holds s.mu across the publish, the next
// serialized caller's reservation therefore observes this charge — which is what
// bounds already-admitted callers. Mutation (make tryReserveAbandonedPublish a
// no-op check that does not charge): abandoned is still 0 while the publish
// wedges, so N admitted callers could each wedge before any charges — the exact
// TOCTOU the fix closes — and the require.Equal below fails.
func TestSender_Send_ReservesBudgetBeforePublish(t *testing.T) {
	mc := newMockConnection()
	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	require.NoError(t, sess.Start(context.Background()))
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the parked reaper's publish return

	wedged := newWedgeableChannel(func(context.Context) (publishResult, error) {
		close(started)
		<-release // ignore ctx, exactly like PublishWithDeferredConfirmWithContext
		return publishResult{PublishOK: true, ConfirmedTag: 1}, nil
	})

	s := NewSender(SenderConfig{Session: sess, RoutingKey: "rk", Timeout: time.Minute, Clock: clocktest.New()})
	s.openChannel = func(amqpConnection, bool) (publisherChannel, error) { return wedged, nil }

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Payload: []byte("hi")})
	msg := ports.OutboundMessage{Envelope: env, Address: "rk"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- s.Send(ctx, msg) }()

	<-started
	require.Equal(t, int64(1), s.abandoned.Load(),
		"Send must reserve the abandoned-publish slot BEFORE issuing the wedgeable publish")

	// Force the wedge to time out so Send returns; the reservation is retained
	// by the reaper (still blocked on the un-returned publish) until cleanup.
	cancel()
	err := wait.RequireReceive(t, res, 2*time.Second)
	require.Error(t, err, "a wedged publish must return a transient error")
	require.Equal(t, int64(1), s.abandoned.Load(),
		"the reservation stays charged while the reaper waits for the wedged publish")
}
