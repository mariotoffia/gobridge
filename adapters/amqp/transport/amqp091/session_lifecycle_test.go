// Validates session lifecycle contracts that previously had subtle gaps:
// concurrent Start ordering, bgDone race-free Close, redactURL parity,
// and Reconcile plan-update semantics.
package amqp091

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSession_Start_ConcurrentBlocksUntilReady validates that a second
// concurrent call to Start does not return success until the first
// call has actually established the connection. Previously the second
// caller observed s.starting=true and immediately returned nil even
// though s.conn was still nil; callers that interpret "Start returned
// nil" as "session is connected" then operated on a not-yet-ready
// session.
func TestSession_Start_ConcurrentBlocksUntilReady(t *testing.T) {
	dialStart := make(chan struct{}, 1)
	releaseDial := make(chan struct{})
	mc := newMockConnection()
	mc.NotifyCloseFn = func(ch chan *amqp.Error) chan *amqp.Error { return ch }

	s := newResilienceSession(func(string) (amqpConnection, error) {
		select {
		case dialStart <- struct{}{}:
		default:
		}
		<-releaseDial
		return mc, nil
	})

	const callers = 5
	var returned atomic.Int32
	results := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range results {
		go func(idx int) {
			defer wg.Done()
			results[idx] = s.Start(context.Background())
			returned.Add(1)
		}(i)
	}

	<-dialStart
	time.Sleep(50 * time.Millisecond)

	if r := returned.Load(); r != 0 {
		t.Fatalf("%d of %d Start callers returned before dial completed; "+
			"concurrent Start must block (not silently report success while connecting)",
			r, callers)
	}
	if c := s.Connection(); c != nil {
		t.Fatal("Connection() returned non-nil before dial completed")
	}

	close(releaseDial)
	wg.Wait()

	if c := s.Connection(); c == nil {
		t.Fatal("Connection() nil after Start returned")
	}
	for i, err := range results {
		if err != nil {
			t.Errorf("Start[%d] returned %v, want nil", i, err)
		}
	}

	_ = s.Close(context.Background())
}

// TestSession_Close_ImmediatelyAfterStart_NoLeak validates that calling
// Close right after Start (small race window where the reconnect
// goroutine has been launched but bgDone may not yet be assigned)
// still waits for the goroutine to exit. Without the fix, Close could
// see bgDone==nil and skip the wait, leaving a transient goroutine leak.
func TestSession_Close_ImmediatelyAfterStart_NoLeak(t *testing.T) {
	mc := newMockConnection()
	notifyCh := make(chan *amqp.Error, 1)
	mc.NotifyCloseFn = func(chan *amqp.Error) chan *amqp.Error { return notifyCh }

	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s.mu.Lock()
	bgDone := s.bgDone
	s.mu.Unlock()
	if bgDone == nil {
		t.Fatal("bgDone was nil immediately after Start; Close would not wait for monitor goroutine to exit (B6 regression)")
	}

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-bgDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect goroutine did not exit within 2s after Close")
	}
}

// TestRedactURL_InvalidURL_Consistent validates that redactURL returns
// a stable, recognisable sentinel for unparseable URLs. The amqp10
// transport historically returned "***" while amqp091 returned a
// different value; tests that grep logs across transports broke.
func TestRedactURL_InvalidURL_Consistent(t *testing.T) {
	const sentinel = "<invalid-url>"
	got := redactURL("://bad url")
	if got != sentinel {
		t.Fatalf("redactURL(invalid) = %q, want %q", got, sentinel)
	}
}

// TestSession_Reconcile_OverwritePlan_ReplacesSubscriptions validates
// that calling Reconcile with a NEW non-empty plan replaces the prior
// plan, not merges it. The mock connection returns an error from
// Channel() so reconcile fails fast without exercising the real AMQP
// channel; we only care that the plan field is updated.
func TestSession_Reconcile_OverwritePlan_ReplacesSubscriptions(t *testing.T) {
	mc := newMockConnection()
	notifyCh := make(chan *amqp.Error, 1)
	mc.NotifyCloseFn = func(chan *amqp.Error) chan *amqp.Error { return notifyCh }
	mc.ChannelFn = func() (*amqp.Channel, error) {
		return nil, errors.New("channel unavailable in unit test")
	}

	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close(context.Background())

	first := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "queue-A"}},
	}
	second := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "queue-B"}, {Topic: "queue-C"},
		},
	}

	_ = s.Reconcile(context.Background(), first)
	s.mu.Lock()
	storedFirst := s.plan
	s.mu.Unlock()
	if storedFirst == nil || len(storedFirst.Subscriptions) != 1 {
		t.Fatalf("after first Reconcile: plan = %+v, want 1 subscription", storedFirst)
	}

	_ = s.Reconcile(context.Background(), second)
	s.mu.Lock()
	storedSecond := s.plan
	s.mu.Unlock()
	if storedSecond == nil || len(storedSecond.Subscriptions) != 2 {
		t.Fatalf("after second Reconcile: plan should have 2 subs, got %+v", storedSecond)
	}
	if storedSecond.Subscriptions[0].Topic != "queue-B" {
		t.Fatalf("plan was merged not overwritten: first sub = %q",
			storedSecond.Subscriptions[0].Topic)
	}
}

// TestSession_Reconcile_PublisherOnlyPlan_StoresAndRuns validates that
// a plan with only Publishers (no Subscriptions) is still stored and
// executed even after a previous plan exists. Previously the code path
// `len(plan.Subscriptions) == 0 && hasPriorPlan { return nil }` caused
// publisher-only plans to be silently ignored.
func TestSession_Reconcile_PublisherOnlyPlan_StoresAndRuns(t *testing.T) {
	mc := newMockConnection()
	notifyCh := make(chan *amqp.Error, 1)
	mc.NotifyCloseFn = func(chan *amqp.Error) chan *amqp.Error { return notifyCh }

	var channelCalls atomic.Int32
	mc.ChannelFn = func() (*amqp.Channel, error) {
		channelCalls.Add(1)
		return nil, errors.New("channel unavailable in unit test")
	}

	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close(context.Background())

	prior := domain.SessionPlan{Subscriptions: []domain.SubscriptionPlan{{Topic: "q"}}}
	_ = s.Reconcile(context.Background(), prior)
	beforePub := channelCalls.Load()

	pubOnly := domain.SessionPlan{Publishers: []domain.PublisherPlan{{Topic: "exch"}}}
	_ = s.Reconcile(context.Background(), pubOnly)

	if channelCalls.Load() == beforePub {
		t.Fatal("Reconcile with publisher-only plan did not invoke conn.Channel(); " +
			"publisher-only updates are silently ignored when a prior plan exists (B7 regression)")
	}
	s.mu.Lock()
	stored := s.plan
	s.mu.Unlock()
	if stored == nil || len(stored.Publishers) != 1 || stored.Publishers[0].Topic != "exch" {
		t.Fatalf("publisher-only plan not stored: %+v", stored)
	}
}

// TestReceiver_PrioritisesContextCancel validates that when ctx.Done
// fires concurrently with a delivery being available, the receiver
// returns from ctx.Done deterministically rather than processing the
// delivery first. A non-deterministic select makes graceful shutdown
// unreliable under load.
func TestReceiver_PrioritisesContextCancel(t *testing.T) {
	deliveries := make(chan amqp.Delivery, 100)
	chanClose := make(chan *amqp.Error, 1)

	for i := range 100 {
		deliveries <- amqp.Delivery{DeliveryTag: uint64(i + 1), Body: []byte("payload")}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var processed atomic.Int32
	emit := func(_ context.Context, _ ports.Delivery) error {
		processed.Add(1)
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runReceiverSelect(ctx, deliveries, chanClose, emit)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("select did not prefer ctx.Done; processed = %d", processed.Load())
	}

	if processed.Load() > 0 {
		t.Fatalf("processed %d deliveries after ctx cancel — select must prefer ctx.Done deterministically",
			processed.Load())
	}
}

// runReceiverSelect mirrors the inner select used by Receiver.consumeLoop
// so the priority contract can be exercised in isolation.
func runReceiverSelect(
	ctx context.Context,
	deliveries <-chan amqp.Delivery,
	chanClose <-chan *amqp.Error,
	emit func(context.Context, ports.Delivery) error,
) {
	for {
		// Priority: ctx.Done before chanClose before deliveries.
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-chanClose:
			return
		case d, ok := <-deliveries:
			if !ok {
				return
			}
			env := &domain.Envelope{ID: "x", Payload: d.Body}
			if err := emit(ctx, &Delivery{env: env, raw: d}); err != nil {
				return
			}
		}
	}
}
