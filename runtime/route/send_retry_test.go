package route

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A direct_hold route retries a recoverable send inside the bridge, with
// backoff, while the route's SendRetryBudget allows, and only then makes its
// replay-or-dead-letter decision. Every test here drives the waits with a fake
// clock and an un-jittered backoff (1 s doubling to 30 s), so each wait is exact.

const sendRetryRoute = "send-retry"

// flakySender fails its first `failures` sends with err and succeeds after
// that. A negative `failures` fails every send. It counts every physical send.
type flakySender struct {
	err      error
	failures int32
	sends    atomic.Int32
}

func (s *flakySender) Send(context.Context, ports.OutboundMessage) error {
	if n := s.sends.Add(1); s.failures < 0 || n <= s.failures {
		return s.err
	}
	return nil
}

// egressHook records every egress OnAttempt and counts OnSettled.
type egressHook struct {
	mu       sync.Mutex
	attempts []ports.DeliveryAttempt
	settled  int
}

func (h *egressHook) OnAttempt(_ context.Context, a ports.DeliveryAttempt) {
	if a.Direction != ports.DirectionEgress {
		return
	}
	h.mu.Lock()
	h.attempts = append(h.attempts, a)
	h.mu.Unlock()
}

func (h *egressHook) OnSettled(context.Context, ports.DeliveryOutcome) {
	h.mu.Lock()
	h.settled++
	h.mu.Unlock()
}

func (h *egressHook) snapshot() ([]ports.DeliveryAttempt, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ports.DeliveryAttempt(nil), h.attempts...), h.settled
}

// deliverySend is one OnDelivery invocation: the envelope handed to the
// callback and that physical send's error.
type deliverySend struct {
	env *messaging.Envelope
	err error
}

// deliveryCallbackLog records every OnDelivery invocation in order.
type deliveryCallbackLog struct {
	mu    sync.Mutex
	sends []deliverySend
}

func (l *deliveryCallbackLog) record(env *messaging.Envelope, err error) {
	l.mu.Lock()
	l.sends = append(l.sends, deliverySend{env: env, err: err})
	l.mu.Unlock()
}

func (l *deliveryCallbackLog) snapshot() []deliverySend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]deliverySend(nil), l.sends...)
}

type sendRetryFixture struct {
	r          *RouteRunner
	clk        *clocktest.Fake
	hook       *egressHook
	rec        *ports.RecordingExporter
	store      *recordingDLQStore
	onDelivery *deliveryCallbackLog
}

// newSendRetryFixture builds a direct_hold route with the given budget. A zero
// budget is unset, so the route runs with the default budget.
func newSendRetryFixture(budget time.Duration, sender ports.Sender) *sendRetryFixture {
	return newSendRetryFixtureWithBackoff(budget, routing.BackoffPolicy{
		InitialInterval: time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2,
		JitterFactor:    routing.JitterDisabled,
	}, sender)
}

// newSendRetryFixtureWithBackoff is newSendRetryFixture with its own backoff.
func newSendRetryFixtureWithBackoff(budget time.Duration, backoff routing.BackoffPolicy, sender ports.Sender) *sendRetryFixture {
	f := &sendRetryFixture{
		clk:        clocktest.New(),
		hook:       &egressHook{},
		rec:        &ports.RecordingExporter{},
		store:      &recordingDLQStore{},
		onDelivery: &deliveryCallbackLog{},
	}
	f.r = NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: sendRetryRoute,
		Policy: routing.RoutePolicy{
			DeliveryMode:    routing.DeliveryDirectHold,
			SendRetryBudget: budget,
			Backoff:         backoff,
		},
		Sender:     sender,
		DLQ:        dlq.New(f.store),
		Metrics:    f.rec,
		Hook:       f.hook,
		Clock:      f.clk,
		OnDelivery: f.onDelivery.record,
	})
	return f
}

// handle runs the delivery on its own goroutine: a held send waits on the fake
// clock, and only the test moves it.
func (f *sendRetryFixture) handle(ctx context.Context, del ports.Delivery) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f.r.HandleDelivery(ctx, del) }()
	return done
}

// awaitRetryWait blocks until send n has been reported and the wait after it is
// armed. OnAttempt fires after boundedSend has stopped its wedge-ceiling timer,
// so the one armed timer is then the retry wait.
func (f *sendRetryFixture) awaitRetryWait(t *testing.T, n int) {
	t.Helper()
	wait.Until(t, 5*time.Second, "retry wait armed after the failed send", func() bool {
		attempts, _ := f.hook.snapshot()
		return len(attempts) == n && f.clk.TimerCount() == 1
	})
}

func (f *sendRetryFixture) counter(name string) int {
	return countTaggedCounter(f.rec, name, shared.TagKeyRouteID, sendRetryRoute)
}

// assertRetryMetrics checks both in-process retry counters.
func (f *sendRetryFixture) assertRetryMetrics(t *testing.T, retries, exhausted int) {
	t.Helper()
	if got := f.counter(shared.MetricSendRetries); got != retries {
		t.Errorf("SendRetries = %d, want %d", got, retries)
	}
	if got := f.counter(shared.MetricSendRetryBudgetExhausted); got != exhausted {
		t.Errorf("SendRetryBudgetExhausted = %d, want %d", got, exhausted)
	}
}

// TestSendRetry_GeneratedIdentityRecoversWithoutDeadLetter pins the reason the
// retry exists. A message whose identity the adapter generated cannot be
// counted by the replay ledger, so a failed send poisons it at once. Retried in
// process, a destination that recovers within the budget delivers it: no
// dead-letter record, no source redelivery.
//
// Mutation check: drop the retry loop and this fails — no retry wait is armed
// and the first failure poisons the message to the DLQ.
func TestSendRetry_GeneratedIdentityRecoversWithoutDeadLetter(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: 1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-recovers")}

	done := f.handle(context.Background(), del)
	f.awaitRetryWait(t, 1)
	f.clk.Advance(time.Second)
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if !del.acked || del.retried {
		t.Fatalf("settlement acked=%v retried=%v, want acked only", del.acked, del.retried)
	}
	if got := f.store.writes.Load(); got != 0 {
		t.Fatalf("DLQ writes = %d, want 0", got)
	}
	if got := sender.sends.Load(); got != 2 {
		t.Fatalf("sends = %d, want 2", got)
	}
	attempts, settled := f.hook.snapshot()
	if len(attempts) != 2 || settled != 1 {
		t.Fatalf("OnAttempt = %d, OnSettled = %d, want 2 and 1", len(attempts), settled)
	}
	if attempts[0].Err == nil || attempts[1].Err != nil {
		t.Fatalf("attempt errors = [%v, %v], want [failure, nil]", attempts[0].Err, attempts[1].Err)
	}
	for i, a := range attempts {
		if a.Attempt != 1 {
			t.Errorf("send %d reported Attempt = %d, want 1: an in-process retry keeps the delivery's attempt number", i+1, a.Attempt)
		}
	}
	f.assertRetryMetrics(t, 1, 0)
}

// TestSendRetry_AlwaysFailingSendSpendsBudgetThenPoisons pins the budget
// arithmetic and what follows it. With a 10 s budget the sends land at t=0, 1,
// 3 and 7 s; the next wait (8 s) would end at 15 s, past the budget, so the loop
// stops and the message takes the poison path it always took.
//
// Mutation check: compare the elapsed time alone against the budget and this
// fails — the loop waits past the budget for a fifth send.
func TestSendRetry_AlwaysFailingSendSpendsBudgetThenPoisons(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: -1}
	f := newSendRetryFixture(10*time.Second, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-exhausted")}

	done := f.handle(context.Background(), del)
	for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		f.awaitRetryWait(t, i+1)
		f.clk.Advance(d)
	}
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if got := sender.sends.Load(); got != 4 {
		t.Fatalf("sends = %d, want 4", got)
	}
	if !del.acked || del.retried {
		t.Fatalf("settlement acked=%v retried=%v, want the terminal poison ack", del.acked, del.retried)
	}
	if got := f.store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1", got)
	}
	if got := countTaggedCounter(f.rec, shared.MetricDLQEntries, shared.TagKeyCategory, "unstable_identity"); got != 1 {
		t.Fatalf("DLQEntries{category=unstable_identity} = %d, want 1", got)
	}
	attempts, settled := f.hook.snapshot()
	if len(attempts) != 4 || settled != 1 {
		t.Fatalf("OnAttempt = %d, OnSettled = %d, want 4 and 1", len(attempts), settled)
	}
	f.assertRetryMetrics(t, 3, 1)
}

// TestSendRetry_RetryAfterHintSetsTheWait pins that a destination's RetryAfter
// hint (SQS throttling, for example) replaces the backoff: the resend waits the
// full 5 s hint, not the 1 s first backoff interval.
//
// Mutation check: compute the wait from the backoff alone and this fails — the
// resend happens after 1 s.
func TestSendRetry_RetryAfterHintSetsTheWait(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable.WithRetryAfter(5 * time.Second), failures: 1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-hint")}

	done := f.handle(context.Background(), del)
	f.awaitRetryWait(t, 1)

	// Advance fires every timer that falls due before it returns, so a wait
	// shorter than the hint has already ended here.
	f.clk.Advance(4 * time.Second)
	if n := f.clk.TimerCount(); n != 1 {
		t.Fatalf("armed timers after 4 s = %d, want 1: the retry wait ended before the 5 s hint", n)
	}
	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends after 4 s = %d, want 1", got)
	}

	f.clk.Advance(time.Second)
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if got := sender.sends.Load(); got != 2 {
		t.Fatalf("sends after 5 s = %d, want 2", got)
	}
	if !del.acked {
		t.Fatal("the resend succeeded but the delivery was not acked")
	}
	f.assertRetryMetrics(t, 1, 0)
}

// TestSendRetry_CancelDuringWaitLeavesDeliveryUnsettled pins that the bridge
// cancelling its own delivery during a retry wait is not a message failure. The
// wait ends, nothing is sent again, and the delivery is left unsettled for the
// source to redeliver — even for an uncountable message, which any settle
// decision would poison.
//
// Mutation check: wait on the timer alone, ignoring the delivery context, and
// this fails — the delivery never returns until the clock moves.
func TestSendRetry_CancelDuringWaitLeavesDeliveryUnsettled(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: -1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-cancelled")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := f.handle(ctx, del)
	f.awaitRetryWait(t, 1)
	cancel()
	err := wait.RequireReceive(t, done, 5*time.Second)

	if !errors.Is(err, errDeliveryAbandoned) || !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleDelivery error = %v, want the abandoned-delivery error carrying context.Canceled", err)
	}
	if del.acked || del.retried {
		t.Fatalf("settlement acked=%v retried=%v, want neither", del.acked, del.retried)
	}
	if got := f.store.writes.Load(); got != 0 {
		t.Fatalf("DLQ writes = %d, want 0", got)
	}
	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if _, settled := f.hook.snapshot(); settled != 0 {
		t.Fatalf("OnSettled = %d, want 0 for an unsettled delivery", settled)
	}
	f.assertRetryMetrics(t, 0, 0)
}

// deadDeliveryContext is a delivery context that is already done through Err
// while its Done channel never closes. That is precisely the state a retry wait
// is in when its timer and the delivery context become ready in the same
// instant and the wait resumes on the timer: the context is dead, but the arm
// that observed it is the timer's. Presenting the state directly keeps this
// test off that coin flip — nothing here depends on which arm a select picks.
type deadDeliveryContext struct{ context.Context }

// Done reports a channel that never closes, so only the retry timer can end the
// wait.
func (deadDeliveryContext) Done() <-chan struct{} { return nil }

// TestSendRetry_WaitEndingOnACancelledContextDoesNotSendAgain pins the stop
// condition at the END of a retry wait. The bridge may cancel a held delivery
// while its wait is running out; whichever of the two the wait notices first, a
// delivery the bridge has already given up on must not be sent again — a sender
// that ignores its context would publish it, and the send would be counted as
// an in-process retry that never had a chance. The wait ends, nothing more is
// sent, and the delivery is left unsettled for the source.
//
// Mutation check: end the wait on the timer without re-reading the delivery
// context and this fails — a second physical send happens and one SendRetries
// is counted.
func TestSendRetry_WaitEndingOnACancelledContextDoesNotSendAgain(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: -1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-cancelled-wait")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := f.handle(deadDeliveryContext{ctx}, del)
	f.awaitRetryWait(t, 1)
	cancel()
	f.clk.Advance(time.Second) // the wait runs out on a context that is already done
	err := wait.RequireReceive(t, done, 5*time.Second)

	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1: a cancelled delivery must not be sent again", got)
	}
	if !errors.Is(err, errDeliveryAbandoned) || !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleDelivery error = %v, want the abandoned-delivery error carrying context.Canceled", err)
	}
	if del.acked || del.retried {
		t.Fatalf("settlement acked=%v retried=%v, want neither", del.acked, del.retried)
	}
	if got := f.store.writes.Load(); got != 0 {
		t.Fatalf("DLQ writes = %d, want 0", got)
	}
	if _, settled := f.hook.snapshot(); settled != 0 {
		t.Fatalf("OnSettled = %d, want 0 for an unsettled delivery", settled)
	}
	f.assertRetryMetrics(t, 0, 0)
}

// TestSendRetry_WedgeStopsFurtherAttempts pins that a wedged route stops
// retrying. The first send hangs past its wedge ceiling, which wedges the
// route; the loop must not wait for another send, and the delivery goes
// straight to the decision it always got: a transient failure the source
// redelivers.
//
// Mutation check: drop the wedge check from the retry condition and this fails
// — the delivery waits on the clock for a retry the wedged route cannot make.
func TestSendRetry_WedgeStopsFurtherAttempts(t *testing.T) {
	sender := &countingHangSender{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(sender.release) // let the parked send goroutine exit when the test ends
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: countLessEnv("send-retry-wedge")}

	done := f.handle(context.Background(), del)
	wait.RequireClosed(t, sender.entered, 5*time.Second)
	waitTimerCount(t, f.clk, 1) // the wedge ceiling of the hung send
	// The default SendTimeout is 30 s, so the ceiling is 30 s + 5 s margin.
	f.clk.Advance(35 * time.Second)
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if !f.r.isWedged() {
		t.Fatal("the hung send did not wedge the route; the test did not exercise the wedge")
	}
	if got := sender.calls.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if !del.retried || del.acked {
		t.Fatalf("settlement acked=%v retried=%v, want a source redelivery", del.acked, del.retried)
	}
	if attempts, _ := f.hook.snapshot(); len(attempts) != 1 {
		t.Fatalf("OnAttempt = %d, want 1", len(attempts))
	}
	f.assertRetryMetrics(t, 0, 0)
}

// TestSendRetry_DisabledBudgetSendsOnce pins the opt-out: with the budget
// disabled a transient failure gets exactly one send and today's outcome — an
// uncountable message is poisoned — even though a second send would succeed.
//
// Mutation check: drop the enabled-budget guard from the retry condition and
// this fails — the disabled route counts SendRetryBudgetExhausted.
func TestSendRetry_DisabledBudgetSendsOnce(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: 1}
	f := newSendRetryFixture(routing.SendRetryBudgetDisabled, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-disabled")}

	if err := wait.RequireReceive(t, f.handle(context.Background(), del), 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if !del.acked || del.retried {
		t.Fatalf("settlement acked=%v retried=%v, want the terminal poison ack", del.acked, del.retried)
	}
	if got := countTaggedCounter(f.rec, shared.MetricDLQEntries, shared.TagKeyCategory, "unstable_identity"); got != 1 {
		t.Fatalf("DLQEntries{category=unstable_identity} = %d, want 1", got)
	}
	f.assertRetryMetrics(t, 0, 0)
}

// TestSendRetry_PermanentErrorIsNotRetried pins that only a recoverable failure
// is retried: a permanent one is dead-lettered after its single send.
//
// Mutation check: retry every failed send and this fails — the delivery waits
// to send the permanent failure again.
func TestSendRetry_PermanentErrorIsNotRetried(t *testing.T) {
	sender := &flakySender{err: permanentSendErr(), failures: -1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: countLessEnv("send-retry-permanent")}

	if err := wait.RequireReceive(t, f.handle(context.Background(), del), 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	if got := sender.sends.Load(); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if !del.acked {
		t.Fatal("a permanent failure must settle terminally")
	}
	if got := countTaggedCounter(f.rec, shared.MetricDLQEntries, shared.TagKeyCategory, "permanent"); got != 1 {
		t.Fatalf("DLQEntries{category=permanent} = %d, want 1", got)
	}
	f.assertRetryMetrics(t, 0, 0)
}
