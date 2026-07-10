package route

import (
	"context"
	"errors"
	stdruntime "runtime"
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

// waitTimerCount spins (yielding, never a logic-driving sleep) until the fake
// clock has at least n active timers, so a test drives a clock deadline only
// AFTER the production code under test has registered it. The wall-clock
// deadline is a stuck-test guard only — it never sequences logic. This mirrors
// the sanctioned fake-clock sync pattern in runtime/health_timeout_test.go.
func waitTimerCount(t *testing.T, clk *clocktest.Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for clk.TimerCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("fake clock never reached %d active timers (have %d)", n, clk.TimerCount())
		}
		stdruntime.Gosched()
	}
}

// --- shared test doubles for the Chunk-2 findings ---------------------------

// recordingDLQStore is a minimal DLQStore that counts Write calls so a test can
// prove a terminal path DID or did NOT retain the payload in the DLQ. Only Write
// is exercised by the runtime terminal paths under test; the reader/admin
// methods satisfy the interface and are never called.
type recordingDLQStore struct{ writes atomic.Int32 }

func (s *recordingDLQStore) Write(context.Context, routing.DLQEntry) error {
	s.writes.Add(1)
	return nil
}
func (s *recordingDLQStore) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}
func (s *recordingDLQStore) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (s *recordingDLQStore) Delete(context.Context, []string) (int, error) { return 0, nil }
func (s *recordingDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) {
	return 0, nil
}
func (s *recordingDLQStore) Purge(context.Context, time.Time) (int, error) { return 0, nil }

// countDropReason returns how many MessagesDropped counters carried the given
// reason tag — the terminal drop is tagged reason=<permanent|rejected|…>.
func countDropReason(rec *ports.RecordingExporter, reason string) int {
	n := 0
	for _, e := range rec.Entries() {
		if e.Kind != "counter" || e.Name != shared.MetricMessagesDropped {
			continue
		}
		for _, tag := range e.Tags {
			if tag.Key == shared.TagKeyReason && tag.Value == reason {
				n++
			}
		}
	}
	return n
}

// countTaggedCounter returns how many counters named `name` carried tag
// key=value.
func countTaggedCounter(rec *ports.RecordingExporter, name, key, value string) int {
	n := 0
	for _, e := range rec.Entries() {
		if e.Kind != "counter" || e.Name != name {
			continue
		}
		for _, tag := range e.Tags {
			if tag.Key == key && tag.Value == value {
				n++
			}
		}
	}
	return n
}

// permanentSendErr is a non-recoverable (permanent) transport error, so
// shared.IsRecoverableError classifies it into the terminal permanent path.
func permanentSendErr() error {
	return shared.NewBridgeError(shared.ErrCodeInternal, shared.ErrorPermanent, "permanent send failure")
}

// TestSendDirectHold_PermanentFailure_DropPolicyHonored proves Chunk-2 finding 1
// (dispatch.go:215): on a PERMANENT send failure the direct_hold path must
// honour on_permanent_failure=drop even when a DLQ store IS configured — the
// operator chose drop precisely so a sensitive payload is not retained in the
// DLQ. Before the fix the path wrote to the DLQ whenever a store existed,
// ignoring the policy.
func TestSendDirectHold_PermanentFailure_DropPolicyHonored(t *testing.T) {
	t.Run("drop policy: dropped, not DLQ'd, even with a store present", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		store := &recordingDLQStore{}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "perm-drop", Payload: []byte("p")})
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID: "r1",
			Policy: routing.RoutePolicy{
				DeliveryMode:       routing.DeliveryDirectHold,
				OnPermanentFailure: routing.FailureDrop,
			},
			Sender:  stubSender{err: permanentSendErr()},
			DLQ:     dlq.New(store),
			Metrics: rec,
		})
		del := &stubDelivery{env: env}

		if err := r.sendDirectHold(context.Background(), del, env, routing.DispatchPlan{BindingID: "b1", Address: "addr"}); err != nil {
			t.Fatalf("sendDirectHold returned error: %v", err)
		}
		if got := store.writes.Load(); got != 0 {
			t.Fatalf("DLQ store received %d writes; on_permanent_failure=drop must NOT retain the payload", got)
		}
		if got := countDropReason(rec, "permanent"); got != 1 {
			t.Fatalf("MessagesDropped{reason=permanent} = %d, want 1", got)
		}
		if got := countCounter(rec, shared.MetricDLQEntries); got != 0 {
			t.Fatalf("DLQEntries = %d, want 0 under drop policy", got)
		}
		if !del.acked {
			t.Fatal("a terminal drop must still settle (ack) the source delivery exactly once")
		}
	})

	t.Run("control: default policy DLQs the permanent failure", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		store := &recordingDLQStore{}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "perm-dlq", Payload: []byte("p")})
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID: "r1",
			Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}, // default OnPermanentFailure=dlq
			Sender:  stubSender{err: permanentSendErr()},
			DLQ:     dlq.New(store),
			Metrics: rec,
		})
		del := &stubDelivery{env: env}

		if err := r.sendDirectHold(context.Background(), del, env, routing.DispatchPlan{BindingID: "b1", Address: "addr"}); err != nil {
			t.Fatalf("sendDirectHold returned error: %v", err)
		}
		if got := store.writes.Load(); got != 1 {
			t.Fatalf("DLQ store writes = %d, want 1 under default dlq policy", got)
		}
		if got := countCounter(rec, shared.MetricDLQEntries); got != 1 {
			t.Fatalf("DLQEntries = %d, want 1", got)
		}
		if got := countDropReason(rec, "permanent"); got != 0 {
			t.Fatalf("MessagesDropped{reason=permanent} = %d, want 0 under dlq policy", got)
		}
	})
}

// TestHandleResolveError_PermanentFailure_DropPolicyHonored proves Chunk-2
// finding 2 (dispatch.go:452): a PERMANENT/rejected resolve error must honour
// on_permanent_failure=drop instead of unconditionally writing the DLQ when a
// store exists.
func TestHandleResolveError_PermanentFailure_DropPolicyHonored(t *testing.T) {
	rejected := shared.ErrNotFound.WithMessage("no destination")

	t.Run("drop policy: dropped, not DLQ'd, even with a store present", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		store := &recordingDLQStore{}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "resolve-drop", Payload: []byte("p")})
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID: "r1",
			Policy: routing.RoutePolicy{
				DeliveryMode:       routing.DeliveryDirectHold,
				OnPermanentFailure: routing.FailureDrop,
			},
			DLQ:     dlq.New(store),
			Metrics: rec,
		})
		del := &stubDelivery{env: env}

		if err := r.handleResolveError(context.Background(), del, env, rejected); err != nil {
			t.Fatalf("handleResolveError returned error: %v", err)
		}
		if got := store.writes.Load(); got != 0 {
			t.Fatalf("DLQ store received %d writes; on_permanent_failure=drop must NOT retain the payload", got)
		}
		if got := countDropReason(rec, "rejected"); got != 1 {
			t.Fatalf("MessagesDropped{reason=rejected} = %d, want 1", got)
		}
		if !del.acked {
			t.Fatal("a terminal rejected drop must still settle (ack) the source delivery")
		}
	})

	t.Run("control: default policy DLQs the rejected resolve error", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		store := &recordingDLQStore{}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "resolve-dlq", Payload: []byte("p")})
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID: "r1",
			Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
			DLQ:     dlq.New(store),
			Metrics: rec,
		})
		del := &stubDelivery{env: env}

		if err := r.handleResolveError(context.Background(), del, env, rejected); err != nil {
			t.Fatalf("handleResolveError returned error: %v", err)
		}
		if got := store.writes.Load(); got != 1 {
			t.Fatalf("DLQ store writes = %d, want 1 under default dlq policy", got)
		}
		if got := countCounter(rec, shared.MetricDLQEntries); got != 1 {
			t.Fatalf("DLQEntries = %d, want 1", got)
		}
	})
}

// panicOnSettledHook panics inside OnSettled — the observer callback the
// direct_hold success path fires AFTER a successful Send but BEFORE the source
// Ack.
type panicOnSettledHook struct{ settledCalls atomic.Int32 }

func (h *panicOnSettledHook) OnAttempt(context.Context, ports.DeliveryAttempt) {}
func (h *panicOnSettledHook) OnSettled(context.Context, ports.DeliveryOutcome) {
	h.settledCalls.Add(1)
	panic("hook boom")
}

// oneShotReceiver emits a single delivery then returns cooperatively, so a test
// can drive the full RouteRunner.Run pipeline (goroutine spawn + panic-recovery)
// deterministically without a broker.
type oneShotReceiver struct {
	del      ports.Delivery
	closedAt atomic.Int32
}

func (rc *oneShotReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	_ = emit(ctx, rc.del)
	return nil
}
func (rc *oneShotReceiver) Close(context.Context) error { rc.closedAt.Add(1); return nil }

// TestDeliveryHook_Panic_DoesNotDoubleSettle proves Chunk-2 finding 3
// (dispatch.go:138): a panic in a delivery hook must NOT alter settlement. A
// hook that panics in OnSettled after a successful Send would, before the fix,
// unwind into the delivery goroutine's panic-recovery path — which, seeing the
// delivery not yet Acked, RETRIES it → a downstream DUPLICATE. With the fix the
// panic is contained in the hook wrapper, the successful send is Acked exactly
// once, and the delivery is never retried.
func TestDeliveryHook_Panic_DoesNotDoubleSettle(t *testing.T) {
	rec := &ports.RecordingExporter{}
	hook := &panicOnSettledHook{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "hook-panic", Payload: []byte("p")})
	del := &stubDelivery{env: env}
	rcv := &oneShotReceiver{del: del}

	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:  "r1",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver: rcv,
		Sender:   stubSender{}, // send succeeds
		Hook:     hook,
		Metrics:  rec,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Run returns after the receiver returns AND in-flight deliveries drain, so
	// by the time it returns the delivery has been fully processed.
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if hook.settledCalls.Load() == 0 {
		t.Fatal("OnSettled was never invoked; test cannot prove containment")
	}
	if !del.acked {
		t.Fatal("a successful send whose hook panicked must still be Acked (settlement unchanged by the observer)")
	}
	if del.retried {
		t.Fatal("hook panic caused a RETRY of an already-sent message — duplicate delivery")
	}
	if got := countTaggedCounter(rec, shared.MetricDeliveryPanics, shared.TagKeyReason, "hook"); got == 0 {
		t.Fatal("a suppressed hook panic must be counted (DeliveryPanics{reason=hook}) so it is never silent")
	}
}

// hangingSender ignores ctx entirely and blocks in Send until released,
// modelling an SDK call that never observes cancellation/timeout.
type hangingSender struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *hangingSender) Send(context.Context, ports.OutboundMessage) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release // ignores ctx: a truly hung sender
	return nil
}

// TestBoundedSend_HungSenderDoesNotWedgeDispatch proves Chunk-2 finding 4
// (dispatch.go:82): a sender that ignores its ctx/timeout must not wedge the
// dispatcher. boundedSend enforces a hard ceiling (SendTimeout), so on a hung
// sender sendDirectHold unblocks, classifies the send as a transient timeout and
// RETRIES (never falsely acks). Before the fix the dispatcher blocked forever on
// the cooperative-only sendCtx.
func TestBoundedSend_HungSenderDoesNotWedgeDispatch(t *testing.T) {
	sender := &hangingSender{entered: make(chan struct{}), release: make(chan struct{})}
	// Release the parked sender goroutine when the test ends so it exits cleanly
	// (the ponytail ceiling documents that such a goroutine may otherwise park
	// until the sender itself returns).
	defer close(sender.release)

	clk := clocktest.New()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "hung-send", Payload: []byte("p")})
	del := &stubDelivery{env: env}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "r1",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			// Fired deterministically via the injected fake clock, never wall time.
			SendTimeout: 30 * time.Second,
		},
		Sender: sender,
		Clock:  clk,
	})

	done := make(chan error, 1)
	go func() {
		done <- r.sendDirectHold(context.Background(), del, env, routing.DispatchPlan{BindingID: "b1", Address: "addr"})
	}()

	// The sender is now parked ignoring ctx; boundedSend has registered its
	// wedge ceiling timer on the injected clock. Fire it (SendTimeout + margin =
	// 35s) to prove the dispatcher unblocks even though the sender never returns.
	<-sender.entered
	waitTimerCount(t, clk, 1)
	clk.Advance(35 * time.Second)

	// MUST return without any real timeout: if the send bound did not unblock the
	// dispatcher this receive wedges and the test times out (regression signal).
	if err := <-done; err != nil {
		t.Fatalf("sendDirectHold returned error: %v", err)
	}

	if del.acked {
		t.Fatal("a send that never confirmed must NOT be acked (that would drop the message)")
	}
	if !del.retried {
		t.Fatal("a bounded-out (transient timeout) send must be retried so the source redelivers")
	}
}

// nonCooperativeReceiver's Run ignores ctx cancellation and only returns once
// Close is invoked — modelling a broker client whose receive loop unblocks on
// Close, not on context cancellation.
type nonCooperativeReceiver struct {
	started     chan struct{}
	closeSignal chan struct{}
	closeOnce   sync.Once
	startOnce   sync.Once
}

func (rc *nonCooperativeReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	rc.startOnce.Do(func() { close(rc.started) })
	<-rc.closeSignal // ignores ctx: only Close unblocks Run
	return nil
}
func (rc *nonCooperativeReceiver) Close(context.Context) error {
	rc.closeOnce.Do(func() { close(rc.closeSignal) })
	return nil
}

// TestRun_NonCooperativeReceiver_ShutdownBounded proves Chunk-2 finding 5
// (runner.go:192): receiver shutdown must not rely on a cooperative Run return.
// A receiver whose Run only unblocks on Close would, before the fix, deadlock —
// Close was called only AFTER Run returned. Post Wave B the watcher grants a
// bounded GRACE on cancellation and force-closes the still-stuck receiver only
// after ReceiverCloseTimeout, driven here by the injected fake clock.
func TestRun_NonCooperativeReceiver_ShutdownBounded(t *testing.T) {
	rcv := &nonCooperativeReceiver{
		started:     make(chan struct{}),
		closeSignal: make(chan struct{}),
	}
	clk := clocktest.New()
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID:              "r1",
		Policy:               routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold},
		Receiver:             rcv,
		Sender:               stubSender{},
		ReceiverCloseTimeout: 10 * time.Second,
		Clock:                clk,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	<-rcv.started // Run is now blocked in the non-cooperative receiver
	cancel()      // shutdown: watcher enters its bounded grace before force-close

	// The watcher registers its grace timer on the injected clock; fire it to
	// force-close the genuinely-stuck receiver so Run unblocks and returns.
	waitTimerCount(t, clk, 1)
	clk.Advance(10 * time.Second)

	// MUST return without any real timeout (regression wedges this receive).
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// --- Wave B finding #5: drain-then-close preserves in-flight acks -----------

// torningDelivery models an in-flight ack that (a) takes ackDur to complete and
// (b) depends on the receiver's transport still being alive. If Close tore the
// transport down (torn=true) before the ack's clock deadline fires, the ack
// FAILS → the source will redeliver → DUPLICATE egress. This is exactly the
// hazard a concurrent (non-drained) receiver Close introduces.
type torningDelivery struct {
	env      *messaging.Envelope
	clk      *clocktest.Fake
	torn     *atomic.Bool
	landed   *atomic.Int64 // optional: bumped when an ack lands (drain witness)
	ackDur   time.Duration
	reached  chan struct{}
	acked    atomic.Bool
	requeued atomic.Bool
}

func (d *torningDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *torningDelivery) Ack(context.Context) error {
	// Register the in-flight ack's completion timer on the injected clock, then
	// announce we are parked so the test can advance deterministically.
	timer := d.clk.NewTimer(d.ackDur)
	close(d.reached)
	<-timer.C()
	if d.torn.Load() {
		// Transport torn down under the in-flight ack → ack cannot land → requeue.
		d.requeued.Store(true)
		return errors.New("transport closed under in-flight ack")
	}
	d.acked.Store(true)
	if d.landed != nil {
		d.landed.Add(1)
	}
	return nil
}
func (d *torningDelivery) Retry(context.Context, time.Duration, error) error {
	d.requeued.Store(true)
	return nil
}
func (d *torningDelivery) Extend(context.Context, time.Time) error { return nil }

// tearDownReceiver emits N deliveries and then (non-cooperatively) blocks Run on
// Close — modelling a broker client whose receive loop only unblocks on Close.
// Close tears down the transport the in-flight acks depend on (sets torn) and
// snapshots how many deliveries are still in-flight AT teardown: on the pre-fix
// concurrent-close path that snapshot is N (Close raced the drain); on the
// drain-then-close path it is 0 (acks completed first).
type tearDownReceiver struct {
	deliveries      []*torningDelivery
	torn            *atomic.Bool
	inflightAtClose atomic.Int64
	closeCalled     atomic.Bool
	runner          *RouteRunner
	started         chan struct{}
	closeSignal     chan struct{}
	closeOnce       sync.Once
	startOnce       sync.Once
}

func (rc *tearDownReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	rc.startOnce.Do(func() { close(rc.started) })
	for _, d := range rc.deliveries {
		if err := emit(ctx, d); err != nil {
			return err
		}
	}
	<-rc.closeSignal // non-cooperative: ignores ctx, only Close unblocks Run
	return nil
}
func (rc *tearDownReceiver) Close(context.Context) error {
	rc.closeOnce.Do(func() {
		rc.closeCalled.Store(true)
		// Snapshot in-flight BEFORE tearing the transport down, so the assertion
		// sees whether the drain happened first.
		rc.inflightAtClose.Store(rc.runner.InFlight())
		rc.torn.Store(true)
		close(rc.closeSignal)
	})
	return nil
}

// TestRun_GracefulShutdown_DrainsBeforeClose proves Wave B finding #4/#5
// (runner.go): on ctx cancellation the watcher must NOT force-close the receiver
// concurrently with the in-flight ack drain. It grants a bounded grace so the
// cooperative drain-then-close path can settle every in-flight ack FIRST; only a
// genuinely-stuck receiver is force-closed after ReceiverCloseTimeout.
//
// Pre-fix (immediate force-close on ctx.Done) this test FAILS: Close races the
// drain, tears the transport down under N parked acks → requeue (duplicate) and
// inflightAtClose == N. Post-fix every ack lands (requeued == 0) and the
// force-close snapshot sees an already-drained runner (inflightAtClose == 0).
func TestRun_GracefulShutdown_DrainsBeforeClose(t *testing.T) {
	const (
		n         = 3
		ackDur    = 1 * time.Second
		closeTO   = 10 * time.Second
		sendBound = 30 * time.Second
	)
	clk := clocktest.New()
	var torn atomic.Bool

	rcv := &tearDownReceiver{
		torn:        &torn,
		started:     make(chan struct{}),
		closeSignal: make(chan struct{}),
	}
	for i := 0; i < n; i++ {
		// Each delivery gets its OWN envelope — the delivery goroutines run
		// concurrently and mutate headers, so a shared envelope would race.
		denv := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "drain", Payload: []byte("p")})
		rcv.deliveries = append(rcv.deliveries, &torningDelivery{
			env:     denv,
			clk:     clk,
			torn:    &torn,
			ackDur:  ackDur,
			reached: make(chan struct{}),
		})
	}

	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "r1",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			SendTimeout:  sendBound, // fake-clock driven; never fires in this test
		},
		Receiver:             rcv,
		Sender:               stubSender{},
		ReceiverCloseTimeout: closeTO,
		Clock:                clk,
	})
	rcv.runner = r // back-ref so Close can snapshot in-flight

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	<-rcv.started
	// Wait until every delivery is parked in its in-flight ack (ack timer
	// registered on the fake clock). Each boundedSend ceiling timer is created
	// and Stopped before its delivery reaches Ack, so at this point exactly N
	// active timers exist — the N in-flight acks.
	for _, d := range rcv.deliveries {
		<-d.reached
	}
	if got := r.InFlight(); got != n {
		t.Fatalf("expected %d in-flight deliveries parked in ack, got %d", n, got)
	}
	// Capture the idle transition channel while InFlight == n (no lost wakeup).
	idle := r.IdleChanged()

	cancel() // graceful shutdown begins

	// Unifying, deadlock-free sync for BOTH pre- and post-fix behaviour:
	//  - pre-fix: the watcher force-closes immediately → closeCalled becomes true.
	//  - post-fix: the watcher registers its grace timer → TimerCount reaches n+1.
	deadline := time.Now().Add(5 * time.Second)
	for !rcv.closeCalled.Load() && clk.TimerCount() < n+1 {
		if time.Now().After(deadline) {
			t.Fatal("watcher neither force-closed nor registered a grace timer after cancel")
		}
		stdruntime.Gosched()
	}

	// Fire the in-flight ack timers FIRST (deadline ackDur < grace closeTO): on
	// the drain-then-close path the transport is still alive so every ack lands.
	clk.Advance(ackDur)
	<-idle // block until InFlight drains to 0

	// Now fire the watcher's grace timer to force-close the (still parked) Run.
	clk.Advance(closeTO)

	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Every ack must have landed; nothing requeued (no duplicate egress).
	var acked, requeued int
	for _, d := range rcv.deliveries {
		if d.acked.Load() {
			acked++
		}
		if d.requeued.Load() {
			requeued++
		}
	}
	if acked != n {
		t.Fatalf("expected all %d acks to land, got %d", n, acked)
	}
	if requeued != 0 {
		t.Fatalf("expected 0 requeues (no duplicate egress), got %d", requeued)
	}
	if got := rcv.inflightAtClose.Load(); got != 0 {
		t.Fatalf("receiver Close must run AFTER the in-flight drain (inflightAtClose=0), got %d "+
			"— Close raced the ack drain (duplicate-egress hazard)", got)
	}
}

// --- Wave B finding #23: cooperative drain-then-close ordering --------------

// coopReceiver models the REAL production shutdown ordering that protects
// amqp091: Run returns PROMPTLY on ctx.Done() → the runner closes watchDone →
// wg.Wait() drains the in-flight acks → closeReceiver() runs LAST. It does not
// rely on the phase-2 grace/force-close branch at all.
//
// Close records how many acks had already LANDED at teardown (ackedAtClose).
// That counter — bumped INSIDE each delivery's Ack, which happens-before its
// wg.Done(), which happens-before wg.Wait() returns, which happens-before
// closeReceiver() — is the deterministic drain witness. (The runner's InFlight
// counter is decremented in a trailing defer that runs AFTER wg.Done(), so a raw
// InFlight() snapshot at the cooperative close is inherently racy; the landed
// count is the reliable happens-before proof that Close saw a drained runner.)
type coopReceiver struct {
	deliveries   []*torningDelivery
	torn         *atomic.Bool
	landed       *atomic.Int64
	ackedAtClose atomic.Int64
	closeCalled  atomic.Bool
	started      chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
}

func (rc *coopReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	rc.startOnce.Do(func() { close(rc.started) })
	for _, d := range rc.deliveries {
		if err := emit(ctx, d); err != nil {
			return err
		}
	}
	<-ctx.Done() // cooperative: return promptly on cancellation
	return nil
}
func (rc *coopReceiver) Close(context.Context) error {
	rc.closeOnce.Do(func() {
		rc.closeCalled.Store(true)
		// Snapshot the landed-ack count and tear the transport down. On the
		// drain-then-close path every ack has already landed (== N).
		rc.ackedAtClose.Store(rc.landed.Load())
		rc.torn.Store(true)
	})
	return nil
}

// TestRun_CooperativeShutdown_DrainsBeforeClose proves the production ordering
// that actually protects amqp091 on graceful shutdown: a receiver whose Run
// returns promptly on ctx.Done() must have ALL its in-flight acks drained by
// wg.Wait() BEFORE closeReceiver() runs. This exercises the cooperative
// close(watchDone) → wg.Wait() → closeReceiver() path (NOT the phase-2 grace
// force-close branch).
//
// Pre-fix (watcher force-closes immediately on ctx.Done) this FAILS: Close runs
// at cancel, before any ack lands → torn tears the transport down under the N
// parked acks → every ack requeues (duplicate egress) and ackedAtClose == 0.
// Post-fix every ack lands (requeued == 0) and Close observes a fully-drained
// runner (ackedAtClose == N).
func TestRun_CooperativeShutdown_DrainsBeforeClose(t *testing.T) {
	const (
		n         = 3
		ackDur    = 1 * time.Second
		closeTO   = 10 * time.Second
		sendBound = 30 * time.Second
	)
	clk := clocktest.New()
	var (
		torn   atomic.Bool
		landed atomic.Int64
	)

	rcv := &coopReceiver{
		torn:    &torn,
		landed:  &landed,
		started: make(chan struct{}),
	}
	for i := 0; i < n; i++ {
		// Each delivery gets its OWN envelope — the delivery goroutines run
		// concurrently and mutate headers, so a shared envelope would race.
		denv := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "coop-drain", Payload: []byte("p")})
		rcv.deliveries = append(rcv.deliveries, &torningDelivery{
			env:     denv,
			clk:     clk,
			torn:    &torn,
			landed:  &landed,
			ackDur:  ackDur,
			reached: make(chan struct{}),
		})
	}

	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "r1",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			SendTimeout:  sendBound, // fake-clock driven; never fires in this test
		},
		Receiver:             rcv,
		Sender:               stubSender{},
		ReceiverCloseTimeout: closeTO,
		Clock:                clk,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	<-rcv.started
	// Wait until every delivery is parked in its in-flight ack.
	for _, d := range rcv.deliveries {
		<-d.reached
	}
	if got := r.InFlight(); got != n {
		t.Fatalf("expected %d in-flight deliveries parked in ack, got %d", n, got)
	}

	cancel() // graceful shutdown: coopReceiver.Run returns promptly on ctx.Done

	// Fire the in-flight ack timers so the cooperative wg.Wait() drain completes;
	// the transport is still alive (Close has not run) so every ack lands. Run
	// then returns via close(watchDone) → wg.Wait() → closeReceiver().
	clk.Advance(ackDur)

	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !rcv.closeCalled.Load() {
		t.Fatal("receiver Close was never called on graceful shutdown")
	}
	var acked, requeued int
	for _, d := range rcv.deliveries {
		if d.acked.Load() {
			acked++
		}
		if d.requeued.Load() {
			requeued++
		}
	}
	if acked != n {
		t.Fatalf("expected all %d acks to land, got %d", n, acked)
	}
	if requeued != 0 {
		t.Fatalf("expected 0 requeues (no duplicate egress), got %d", requeued)
	}
	// Close ran AFTER the drain: it observed all N acks already landed.
	if got := rcv.ackedAtClose.Load(); got != n {
		t.Fatalf("closeReceiver must run AFTER the in-flight ack drain (ackedAtClose=%d), got %d "+
			"— Close raced/preceded the ack drain (duplicate-egress hazard)", n, got)
	}
}
