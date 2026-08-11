package route

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// ════════════════════════════════════════════════════════════════════════════
// Adversarial-review blocking findings on the route dispatch side.
// Deterministic; no time.Sleep. Timing paths use the injected fake clock.
// ════════════════════════════════════════════════════════════════════════════

// ── ──────────────────────────────────────────────────────────────────────

// TestPanicRecovery_CountLessSource_ReplayCapPoisons proves the panic-recovery
// poison gate (recoverDelivery) routes through the bridge-owned replay ledger
// (replayCapReached), not the raw native receive count. A count-less source
// (receiveCount always 0) with a deterministically-panicking resolver/hook/tracer
// used to loop forever because `rc >= MaxReplayAttempts` never tripped. Now the
// retry branch records one ledger attempt per redelivery so the cap climbs and
// the message poisons to the DLQ after exactly MaxReplayAttempts retries.
//
// Mutation: revert recoverDelivery to `rc := receiveCount(env)` +
// `rc >= MaxReplayAttempts` and a count-less panic never poisons — this loop
// runs past cap+5 without an ack, so poisonedAt stays -1 and the test fails.
func TestPanicRecovery_CountLessSource_ReplayCapPoisons(t *testing.T) {
	const capN = 3
	store := &recordingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "b2",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: capN,
		},
		DLQ:     dlq.New(store),
		Metrics: &ports.RecordingExporter{},
	})

	env := countLessEnv("b2-panic-poison")
	cause := errors.New("resolver panicked deterministically")

	retries := 0
	poisonedAt := -1
	for i := 0; i < capN+5; i++ {
		del := &stubDelivery{env: env}
		r.recoverDelivery(context.Background(), del, cause)
		switch {
		case del.retried && !del.acked:
			retries++
		case del.acked && !del.retried:
			poisonedAt = i
		default:
			t.Fatalf("delivery %d: ambiguous settlement acked=%v retried=%v", i, del.acked, del.retried)
		}
		if poisonedAt >= 0 {
			break
		}
	}

	if poisonedAt < 0 {
		t.Fatalf("count-less panic never poisoned within %d deliveries — the panic path retries forever", capN+5)
	}
	if poisonedAt != capN {
		t.Fatalf("panic poisoned at delivery %d, want %d (exactly MaxReplayAttempts=%d retries first)", poisonedAt, capN, capN)
	}
	if retries != capN {
		t.Fatalf("retries before panic-poison = %d, want %d", retries, capN)
	}
	if got := store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1 (panic-poison to DLQ at the cap)", got)
	}
}

// ── ──────────────────────────────────────────────────────────────────────

// ackFailDelivery is a delivery whose terminal Ack fails deterministically (a
// broker hiccup at settle), letting a test exercise the "terminal settle failed"
// branch. Retry always succeeds so the climb-to-cap phase is unaffected.
type ackFailDelivery struct {
	env     *messaging.Envelope
	acked   bool
	retried bool
	ackErr  error
}

func (d *ackFailDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *ackFailDelivery) Ack(context.Context) error {
	if d.ackErr != nil {
		return d.ackErr
	}
	d.acked = true
	return nil
}
func (d *ackFailDelivery) Retry(context.Context, time.Duration, error) error {
	d.retried = true
	return nil
}
func (d *ackFailDelivery) Extend(context.Context, time.Time) error { return nil }

// TestTerminalAckFailure_KeepsLedgerEntry proves the replay ledger is evicted
// ONLY after a terminal Ack SUCCEEDS. When the poison/DLQ terminal settle's Ack
// fails, the source redelivers a message we already DLQ'd; if the ledger had been
// cleared first, the redelivery would re-enter at count 0, earn a fresh
// MaxReplayAttempts budget, and write a SECOND DLQ entry (repeating while Ack
// keeps failing). Keeping the entry means the redelivery is immediately re-capped.
//
// Mutation: move forgetReplayAttempts back BEFORE del.Ack in ackDelivery and the
// entry is cleared despite the failed ack — attemptsFor drops to 0 and
// replayCapReached is no longer over — so this test fails.
func TestTerminalAckFailure_KeepsLedgerEntry(t *testing.T) {
	const capN = 3
	store := &recordingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "b3",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: capN,
		},
		Sender:  stubSender{err: shared.ErrUnavailable}, // deterministic transient send failure
		DLQ:     dlq.New(store),
		Metrics: &ports.RecordingExporter{},
	})

	env := countLessEnv("b3-ackfail")
	key := replayKey(env)

	// Climb the ledger to the cap with `capN` transient-failure retries.
	for i := 0; i < capN; i++ {
		del := &stubDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("retry delivery %d: HandleDelivery: %v", i, err)
		}
		if !del.retried || del.acked {
			t.Fatalf("delivery %d: expected a below-cap retry, got acked=%v retried=%v", i, del.acked, del.retried)
		}
	}
	if _, over := r.replayCapReached(env); !over {
		t.Fatalf("ledger did not reach the cap after %d retries", capN)
	}

	// The poison delivery whose terminal Ack FAILS (broker hiccup at settle).
	poison := &ackFailDelivery{env: env, ackErr: errors.New("broker hiccup at settle")}
	err := r.HandleDelivery(context.Background(), poison)
	if err == nil {
		t.Fatal("expected the terminal Ack failure to propagate from HandleDelivery")
	}
	if got := store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1 (poison written before the failed ack)", got)
	}

	// because the terminal Ack FAILED, the ledger entry must be RETAINED so a
	// redelivery is immediately re-capped — no fresh budget, no duplicate DLQ.
	if n := r.replay.attemptsFor(key); n != capN {
		t.Fatalf("ledger attempts after a FAILED terminal Ack = %d, want %d (entry must be retained until the settle lands)", n, capN)
	}
	if _, over := r.replayCapReached(env); !over {
		t.Fatal("after a failed terminal Ack a redelivery must be immediately re-capped, but the ledger was cleared")
	}
}

// ── ──────────────────────────────────────────────────────────────────────

// releaseSender parks in Send until the test releases it, then returns a fixed
// error. Unlike a truly-hung sender it DOES return once released — modelling a
// COOPERATIVE sender that aborts (e.g. via its ctx deadline) at SendTimeout.
type releaseSender struct {
	entered chan struct{}
	release chan struct{}
	err     error
	entOnce sync.Once
	relOnce sync.Once
}

func (s *releaseSender) Send(context.Context, ports.OutboundMessage) error {
	s.entOnce.Do(func() { close(s.entered) })
	<-s.release
	return s.err
}

func (s *releaseSender) unblock() { s.relOnce.Do(func() { close(s.release) }) }

// TestCooperativeSenderAtSendTimeout_NotWedged proves the wedge ceiling is
// strictly LARGER than SendTimeout, so a cooperative sender that returns AT
// SendTimeout wins the ceiling race and is retried as a transient — it does NOT
// falsely wedge the route (which would trigger an avoidable pod restart on
// ordinary transient slowness). The distinguishing lever: advancing the fake
// clock EXACTLY to SendTimeout must NOT fire the ceiling.
//
// Mutation: set the boundedSend ceiling back to r.policy.SendTimeout and this
// fails — advancing to SendTimeout fires the ceiling (armed timers drop to 0),
// so the TimerCount==1 assertion trips (and the route wedges the returning
// cooperative sender).
func TestCooperativeSenderAtSendTimeout_NotWedged(t *testing.T) {
	sender := &releaseSender{entered: make(chan struct{}), release: make(chan struct{}), err: shared.ErrUnavailable}
	defer sender.unblock() // ensure the parked goroutine exits even on early failure

	clk := clocktest.New()
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "b4",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			SendTimeout:  30 * time.Second, // wedge ceiling = 30 + min(30,5) = 35s
		},
		Sender:  sender,
		Metrics: &ports.RecordingExporter{},
		Clock:   clk,
	})

	const binding = "b1"
	msg := ports.OutboundMessage{Envelope: countLessEnv("b4"), Address: "addr"}

	errc := make(chan error, 1)
	go func() { errc <- r.boundedSend(context.Background(), sender, msg, binding) }()
	<-sender.entered
	waitTimerCount(t, clk, 1)

	// Advance EXACTLY to SendTimeout. The wedge ceiling (35s) must NOT fire here,
	// so the one armed timer is still counted — a cooperative sender aborting at
	// SendTimeout has to win the race.
	clk.Advance(30 * time.Second)
	if n := clk.TimerCount(); n != 1 {
		t.Fatalf("the wedge ceiling fired at SendTimeout (armed timers=%d, want 1); it must EXCEED SendTimeout "+
			"so a cooperative sender wins the race instead of being falsely wedged into a pod restart", n)
	}

	// The cooperative sender now returns its transient error. boundedSend must
	// surface it (retryable) and must NOT have wedged the route.
	sender.unblock()
	err := <-errc
	if !shared.IsRecoverableError(err) {
		t.Fatalf("cooperative send error = %v, want a recoverable (retryable) error", err)
	}
	if r.isWedged() {
		t.Fatal("a cooperative sender that returned must NOT wedge the route (nothing leaked to cap)")
	}
}
