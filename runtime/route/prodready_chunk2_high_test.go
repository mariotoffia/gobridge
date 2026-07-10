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
)

// ════════════════════════════════════════════════════════════════════════════
// Chunk-2 HIGH findings — route runner & dispatch
//
// Every test here is deterministic: no time.Sleep sequences logic. Timing-driven
// paths use the injected fake clock; everything else is synchronous.
// ════════════════════════════════════════════════════════════════════════════

// countLessEnv builds an envelope with NO native receive-count header, modelling
// an MQTT/AMQP-0-9-1/HTTP source. A stable ID keeps the bridge-owned replay key
// constant across redeliveries (the ledger only caps a stable identity — the
// documented adapter-side dependency).
func countLessEnv(id string) *messaging.Envelope {
	return messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Payload: []byte("p")})
}

// ── HIGH-1 ──────────────────────────────────────────────────────────────────

// TestHigh1_CountLessSource_ReplayCapPoisons proves the bridge-owned replay
// ledger makes MaxReplayAttempts effective for a COUNT-LESS source. A
// deterministically-failing (transient) send on a source that reports no native
// receive count previously looped forever (receiveCount==0 kept every
// `rc >= MaxReplayAttempts` gate false). Now the ledger climbs one per
// redelivery, so after exactly MaxReplayAttempts retries the message poisons to
// the DLQ and settles — bounded, not infinite.
//
// Mutation check: revert effectiveAttempt to always return receiveCount (0 for a
// count-less source) and this test fails — the delivery is retried on every
// iteration and never poisons, so the `poisoned` assertion never trips.
func TestHigh1_CountLessSource_ReplayCapPoisons(t *testing.T) {
	const cap = 3
	store := &recordingDLQStore{}
	rec := &ports.RecordingExporter{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "high1",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: cap,
		},
		Sender:  stubSender{err: shared.ErrUnavailable}, // deterministic transient send failure
		DLQ:     dlq.New(store),
		Metrics: rec,
	})

	env := countLessEnv("high1-poison")

	retries := 0
	poisonedAt := -1
	// Drive well past the cap: a correct implementation poisons at delivery
	// cap+1; a regressed (uncapped) one keeps retrying, which this loop detects.
	for i := 0; i < cap+5; i++ {
		del := &stubDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("delivery %d: HandleDelivery: %v", i, err)
		}
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
		t.Fatalf("count-less source never poisoned within %d deliveries — MaxReplayAttempts is ineffective (infinite retry)", cap+5)
	}
	if poisonedAt != cap {
		t.Fatalf("poisoned at delivery %d, want %d (exactly MaxReplayAttempts=%d retries first)", poisonedAt, cap, cap)
	}
	if retries != cap {
		t.Fatalf("retries before poison = %d, want %d", retries, cap)
	}
	if got := store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1 (poison to DLQ at the cap)", got)
	}
	// The ledger must EVICT the poisoned key on the terminal ack so a pathological
	// stream cannot grow it unboundedly.
	if sz := r.replay.size(); sz != 0 {
		t.Fatalf("replay ledger retained %d keys after terminal settle, want 0 (evict-on-terminal)", sz)
	}
}

// ── HIGH-2 ──────────────────────────────────────────────────────────────────

// singleUseReceiver models a single-use transport: Run returns a transient error
// once, and Close (which RouteRunner.Run always invokes on exit) renders the
// instance unusable. A supervisor that re-ran the SAME closed instance would flap
// forever behind green liveness.
type singleUseReceiver struct {
	runCalls atomic.Int32
	closed   atomic.Int32
	runErr   error
}

func (rc *singleUseReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	rc.runCalls.Add(1)
	return rc.runErr
}
func (rc *singleUseReceiver) Close(context.Context) error { rc.closed.Add(1); return nil }

// TestHigh2_SingleUseReceiver_RestartEscalatesTerminal proves route supervision
// no longer flaps a closed single-use receiver. The FIRST Run surfaces the
// transient receiver error and closes the (single-use) receiver; a supervised
// re-entry returns ErrRouteReceiverClosed — which wraps ErrRouteTerminal, the
// single predicate superviseRoute escalates on — WITHOUT re-running the dead
// instance. Since AddRoute stores built instances (not factories), escalate is
// the lazy-correct choice: an orchestrator restarts the pod with fresh
// transports instead of the runtime silently flapping.
//
// Mutation check: delete the `if r.receiverClosed.Load()` re-entry guard at the
// top of Run and this fails — the second Run re-invokes receiver.Run (runCalls
// climbs to 2) and returns the bare transient error, not a terminal one.
func TestHigh2_SingleUseReceiver_RestartEscalatesTerminal(t *testing.T) {
	transient := shared.NewBridgeError(shared.ErrCodeUnavailable, shared.ErrorTransient, "broker stream dropped")
	rcv := &singleUseReceiver{runErr: transient}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "high2",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: rcv,
		Sender:   stubSender{},
	})

	// First supervised run: surfaces the transient error and closes the receiver.
	err1 := r.Run(context.Background())
	if !errors.Is(err1, transient) {
		t.Fatalf("first Run error = %v, want the transient receiver error", err1)
	}
	if errors.Is(err1, ErrRouteTerminal) {
		t.Fatalf("first Run must surface a TRANSIENT error (isolate + backoff), not terminal: %v", err1)
	}
	if got := rcv.runCalls.Load(); got != 1 {
		t.Fatalf("receiver.Run calls after first Run = %d, want 1", got)
	}
	if got := rcv.closed.Load(); got < 1 {
		t.Fatal("RouteRunner.Run must close its single-use receiver on exit")
	}

	// Second supervised run (the restart): MUST escalate terminal and NOT re-run
	// the dead receiver.
	err2 := r.Run(context.Background())
	if !errors.Is(err2, ErrRouteTerminal) {
		t.Fatalf("restart error = %v, want it to wrap ErrRouteTerminal (escalate, not flap)", err2)
	}
	if got := rcv.runCalls.Load(); got != 1 {
		t.Fatalf("receiver.Run re-invoked on restart (calls=%d); a closed single-use receiver must NOT be re-run", got)
	}
}

// ── HIGH-3 ──────────────────────────────────────────────────────────────────

// countingHangSender counts Send invocations. Its FIRST Send blocks forever
// (ignoring ctx), modelling an SDK call wedged inside a broker; any later Send
// returns immediately. Blocking only the first call lets the cap-removal mutation
// FAIL CLEANLY (later sends return at once, exposing calls>1) instead of
// deadlocking the test.
type countingHangSender struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *countingHangSender) Send(context.Context, ports.OutboundMessage) error {
	if s.calls.Add(1) == 1 {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return nil
}

// TestHigh3_HungSender_CapsLeakedGoroutinesAndWedges proves the per-binding
// hung-send cap. The first send to a binding hangs; on the ceiling boundedSend
// latches the binding and WEDGES the route. Every subsequent CONSECUTIVE send to
// the same binding is refused BEFORE spawning, so no matter how many redeliveries
// arrive at most ONE sender goroutine is ever parked per binding (Go cannot kill
// the parked goroutine, so cap + stop-accepting is the only bound). The wedge is
// terminal so the route stops accepting and superviseRoute escalates.
//
// Mutation check: delete the `if r.sendHung(binding)` pre-spawn refusal in
// boundedSend and this fails — each consecutive send spawns another Send
// goroutine, so calls climbs past 1.
func TestHigh3_HungSender_CapsLeakedGoroutinesAndWedges(t *testing.T) {
	sender := &countingHangSender{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(sender.release) // let the single parked goroutine exit when the test ends

	clk := clocktest.New()
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "high3",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			SendTimeout:  30 * time.Second, // fired via the fake clock, never wall time
		},
		Sender: sender,
		Clock:  clk,
	})

	const binding = "b1"
	msg := ports.OutboundMessage{Envelope: countLessEnv("high3"), Address: "addr"}

	// First send: hangs, then the wedge ceiling (SendTimeout + margin = 35s) fires
	// → transient timeout + wedge.
	first := make(chan error, 1)
	go func() { first <- r.boundedSend(context.Background(), sender, msg, binding) }()
	<-sender.entered
	waitTimerCount(t, clk, 1)
	clk.Advance(35 * time.Second)
	if err := <-first; !shared.IsRecoverableError(err) {
		t.Fatalf("first hung send: err = %v, want a transient timeout", err)
	}
	if !r.isWedged() {
		t.Fatal("a hung send must WEDGE the route so it stops accepting new work")
	}

	// N consecutive further sends to the SAME binding: each refused pre-spawn.
	for i := 0; i < 5; i++ {
		err := r.boundedSend(context.Background(), sender, msg, binding)
		if !shared.IsRecoverableError(err) {
			t.Fatalf("consecutive send %d: err = %v, want a transient timeout (refused)", i, err)
		}
	}

	if got := sender.calls.Load(); got != 1 {
		t.Fatalf("Send invoked %d times; the per-binding cap must park at most ONE goroutine", got)
	}
}

// ── HIGH-4 ──────────────────────────────────────────────────────────────────

// blockingProcessor ignores ctx cancellation and blocks in Process until the
// test releases it, modelling a processor that overruns ProcessorTimeout and is
// abandoned by the runner.
type blockingProcessor struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProcessor) Name() string { return "blocking" }
func (p *blockingProcessor) Process(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
	p.once.Do(func() { close(p.entered) })
	<-p.release // ignores ctx: a truly non-cooperative processor
	return nil
}

// TestHigh4_ProcessorTimeout_InvokesCircuitBreakerHook proves the chain reports a
// GENUINE processor-timeout abandon (not a shutdown-grace abandon) to the
// route-level circuit-breaker hook exactly once. This is the wiring HIGH-4 adds:
// WithChainOnProcessorTimeout fired from chain.go's genuine-timeout branch.
//
// Mutation check: delete the `if cfg.onProcessorTimeout != nil` call in chain.go
// (or move it under the shutdown-grace branch) and this fails — the hook never
// fires on a genuine timeout.
func TestHigh4_ProcessorTimeout_InvokesCircuitBreakerHook(t *testing.T) {
	proc := &blockingProcessor{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(proc.release)

	clk := clocktest.New()
	var abandons atomic.Int32

	// root is a live (non-cancelled) context so the chain classifies the overrun
	// as a genuine processor-timeout, NOT shutdown-grace.
	env := countLessEnv("high4-chain")
	done := make(chan error, 1)
	go func() {
		done <- RunChain(context.Background(), []ports.Processor{proc}, env,
			WithChainTimeout(30*time.Second),
			WithChainClock(clk),
			WithChainOnProcessorTimeout(func() { abandons.Add(1) }),
		)
	}()

	<-proc.entered
	waitTimerCount(t, clk, 1) // the per-processor budget timer is armed
	clk.Advance(30 * time.Second)

	err := <-done
	if !errors.Is(err, shared.ErrProcessorTimeout) {
		t.Fatalf("RunChain err = %v, want ErrProcessorTimeout", err)
	}
	if got := abandons.Load(); got != 1 {
		t.Fatalf("circuit-breaker hook fired %d times, want 1 (genuine timeout abandon)", got)
	}
}

// TestHigh4_AbandonedProcessorCircuitBreaker proves the route-level breaker: once
// maxAbandonedProcessors abandons accumulate WITHOUT an intervening terminal
// settle the route wedges (terminal), and a terminal settle resets the counter so
// the breaker measures abandons-since-last-resolution rather than a lifetime
// total. This bounds the residual HIGH-4 hazard when HIGH-1's ledger is defeated
// (unstable per-message identity) or the cap is disabled (MaxReplayAttempts=0).
//
// Mutation check: remove the wedge call in onProcessorAbandoned and this fails —
// the route never wedges no matter how many goroutines are abandoned.
func TestHigh4_AbandonedProcessorCircuitBreaker(t *testing.T) {
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "high4",
		Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Sender:  stubSender{},
	})

	// A terminal settle keeps resetting the counter, so a poison-then-settle
	// message (cap fires well below the breaker) never trips it.
	for i := 0; i < maxAbandonedProcessors*3; i++ {
		r.onProcessorAbandoned()
		if i%2 == 1 {
			r.resetAbandonedProcessors() // simulate an intervening terminal settle
		}
		if r.isWedged() {
			t.Fatalf("route wedged after %d abandons WITH interleaved resets; the breaker must measure abandons-since-settle", i+1)
		}
	}

	// Now accumulate abandons with NO reset: the breaker must trip at the ceiling.
	r.resetAbandonedProcessors()
	for i := 0; i < maxAbandonedProcessors; i++ {
		if r.isWedged() {
			t.Fatalf("wedged early at abandon %d, want exactly at %d", i, maxAbandonedProcessors)
		}
		r.onProcessorAbandoned()
	}
	if !r.isWedged() {
		t.Fatalf("route did not wedge after %d consecutive abandoned processors", maxAbandonedProcessors)
	}
	if !errors.Is(r.wedgeError(), ErrRouteTerminal) {
		t.Fatalf("wedge error = %v, want it to wrap ErrRouteTerminal (escalate)", r.wedgeError())
	}
}

// ── HIGH-5 ──────────────────────────────────────────────────────────────────

// countingSender counts Send calls so a test can prove the route DEFAULT sender
// was never invoked for a rejected wrong-target plan.
type countingSender struct{ calls atomic.Int32 }

func (s *countingSender) Send(context.Context, ports.OutboundMessage) error {
	s.calls.Add(1)
	return nil
}

// fixedResolver returns a fixed set of plans, modelling a custom
// DestinationResolver.
type fixedResolver struct{ plans []routing.DispatchPlan }

func (r fixedResolver) Resolve(context.Context, *messaging.Envelope) ([]routing.DispatchPlan, error) {
	return r.plans, nil
}

// TestHigh5_UndeclaredBindingPlan_RejectedBeforeSend proves the direct-hold
// fail-closed guard. On a route that DECLARES bindings, a resolver plan whose
// BindingID is NOT declared is rejected BEFORE dispatch: the default sender is
// never invoked (no wrong-target delivery), the source is never acked as success,
// and the plan routes through the normal retry-then-poison path (a recoverable
// rejection, consistent with the shared-outbox orphan guard).
//
// Mutation check: delete the validatePlanBindings call in resolvePlans and this
// fails — senderForBinding falls back to the default sender, which sends the
// wrong-target message (calls == 1) and the source is acked as success.
func TestHigh5_UndeclaredBindingPlan_RejectedBeforeSend(t *testing.T) {
	dflt := &countingSender{}
	store := &recordingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "high5",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 1, // small cap so the poison outcome is observable quickly
		},
		Sender:   dflt, // the route DEFAULT sender — must NOT be used for an undeclared binding
		DLQ:      dlq.New(store),
		Bindings: []routing.DestinationBinding{{ID: "declared", Address: "topic/declared"}},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{
			{BindingID: "undeclared", Address: "topic/attacker-chosen"},
		}},
	})

	env := countLessEnv("high5-orphan")

	// First delivery (below the cap): the rejection is recoverable → retry, never
	// a default-sender send, never a success ack.
	del1 := &stubDelivery{env: env}
	if err := r.HandleDelivery(context.Background(), del1); err != nil {
		t.Fatalf("HandleDelivery(1): %v", err)
	}
	if got := dflt.calls.Load(); got != 0 {
		t.Fatalf("default sender invoked %d times for an undeclared binding — wrong-target delivery", got)
	}
	if del1.acked {
		t.Fatal("source acked for a rejected wrong-target plan — delivery outside the configured binding set")
	}
	if !del1.retried {
		t.Fatal("a below-cap rejection must unsettle (retry), never a default-sender send")
	}

	// Second delivery (at the cap): the recoverable rejection now poisons to DLQ
	// (per policy), still never touching the default sender.
	del2 := &stubDelivery{env: env}
	if err := r.HandleDelivery(context.Background(), del2); err != nil {
		t.Fatalf("HandleDelivery(2): %v", err)
	}
	if got := dflt.calls.Load(); got != 0 {
		t.Fatalf("default sender invoked %d times at the cap — wrong-target delivery", got)
	}
	if got := store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1 (rejected plan poisons per policy at the cap)", got)
	}
	if !del2.acked || del2.retried {
		t.Fatalf("at the cap the rejected plan must settle terminally (acked=%v retried=%v)", del2.acked, del2.retried)
	}
}
