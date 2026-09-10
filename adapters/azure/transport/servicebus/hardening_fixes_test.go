package servicebus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// --- Finding 4: ReceiveAndDelete settlement -------------------------------

// In ReceiveAndDelete mode the broker removes the message at receive time,
// so there is no lock to settle. Ack must be a no-op (the PeekLock-only
// CompleteMessage call would fail against an already-settled message).
func TestReceiveAndDelete_AckIsNoOp(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "rad-ack"})
	msg := &azservicebus.ReceivedMessage{MessageID: "rad-ack"}
	d := newDelivery(context.Background(), env, mock, nil, msg, deliveryTuning{lockDuration: 30 * time.Second}, nil, nil, nil, nil)
	d.receiveAndDelete = true

	if err := d.Ack(context.Background()); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.CompleteCalls) != 0 {
		t.Fatalf("CompleteMessage called %d times in ReceiveAndDelete mode, want 0", len(mock.CompleteCalls))
	}
}

// Retry cannot make an already-deleted message available again, so it must
// report ErrNotSupported. The runtime's retryOrFallback then DLQ-routes the
// message instead of looping — the safest no-loss choice.
func TestReceiveAndDelete_RetryReturnsNotSupported(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	sched := &mockRetryScheduler{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "rad-retry"})
	msg := &azservicebus.ReceivedMessage{MessageID: "rad-retry"}
	d := newDelivery(context.Background(), env, mock, sched, msg, deliveryTuning{lockDuration: 30 * time.Second}, nil, nil, nil, nil)
	d.receiveAndDelete = true

	err := d.Retry(context.Background(), 5*time.Second, errors.New("boom"))
	if !errors.Is(err, shared.ErrNotSupported) {
		t.Fatalf("Retry err = %v, want ErrNotSupported", err)
	}

	sched.mu.Lock()
	sc := sched.ScheduleCalls
	sched.mu.Unlock()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if sc != 0 {
		t.Fatalf("ScheduleCalls = %d, want 0", sc)
	}
	if len(mock.AbandonCalls) != 0 {
		t.Fatalf("AbandonCalls = %d, want 0", len(mock.AbandonCalls))
	}
	if len(mock.CompleteCalls) != 0 {
		t.Fatalf("CompleteCalls = %d, want 0", len(mock.CompleteCalls))
	}
}

// Extend has no lock to renew in ReceiveAndDelete mode and must no-op.
func TestReceiveAndDelete_ExtendIsNoOp(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "rad-ext"})
	msg := &azservicebus.ReceivedMessage{MessageID: "rad-ext"}
	d := newDelivery(context.Background(), env, mock, nil, msg, deliveryTuning{lockDuration: 30 * time.Second}, nil, nil, nil, nil)
	d.receiveAndDelete = true

	if err := d.Extend(context.Background(), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Extend: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.RenewCalls) != 0 {
		t.Fatalf("RenewMessageLock called %d times in ReceiveAndDelete mode, want 0", len(mock.RenewCalls))
	}
}

// A ReceiveAndDelete receiver must not start the auto-extend goroutine
// (there is no lock to renew). Verified through Run by capturing the
// delivery and confirming no lock renewals occur.
func TestReceiveAndDelete_DisablesAutoExtend(t *testing.T) {
	t.Parallel()

	callCount := 0
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			callCount++
			if callCount > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []*azservicebus.ReceivedMessage{{MessageID: "rad-1", Body: []byte("x")}}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:       "q",
		ReceiveMode:     "ReceiveAndDelete",
		AllowAtMostOnce: true,
		Client:          mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var rad bool
	_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
		rad = del.(*asbDelivery).receiveAndDelete
		cancel()
		return nil
	})

	if !rad {
		t.Fatal("delivery from a ReceiveAndDelete receiver must carry receiveAndDelete=true")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.RenewCalls) != 0 {
		t.Fatalf("auto-extend must be disabled in ReceiveAndDelete mode, got %d renews", len(mock.RenewCalls))
	}
}

// --- Finding 2: topic subscription delayed-retry fan-out ------------------

// A delivery whose receiver structurally disables delayed retry (a topic
// subscription, scheduler == nil) must fall back to AbandonMessage on a
// delayed Retry — never schedule (which would fan out to the topic) — and
// must not error.
func TestSubscription_DelayedRetry_FallsBackToAbandon(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "sub-retry"})
	msg := &azservicebus.ReceivedMessage{MessageID: "sub-retry"}
	// scheduler == nil mirrors a subscription receiver (see ensureClient).
	d := newDelivery(context.Background(), env, mock, nil, msg, deliveryTuning{lockDuration: 30 * time.Second}, nil, nil, nil, nil)
	d.delayedRetryDisabled = true

	if err := d.Retry(context.Background(), 10*time.Second, nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.AbandonCalls) != 1 {
		t.Fatalf("AbandonCalls = %d, want 1 (delayed retry must abandon, not fan out)", len(mock.AbandonCalls))
	}
	if len(mock.CompleteCalls) != 0 {
		t.Fatalf("CompleteCalls = %d, want 0", len(mock.CompleteCalls))
	}
}

// The receiver must tag subscription deliveries with delayedRetryDisabled=true
// and queue deliveries with false, and must never attach a scheduler to a
// subscription (its entity name is the topic → fan-out).
func TestReceiver_DelayedRetryDisabledWiring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         ReceiverConfig
		wantDisable bool
	}{
		{
			name:        "queue enables delayed retry",
			cfg:         ReceiverConfig{QueueName: "q"},
			wantDisable: false,
		},
		{
			name:        "subscription disables delayed retry",
			cfg:         ReceiverConfig{TopicName: "t", SubscriptionName: "s"},
			wantDisable: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			callCount := 0
			mock := &mockASBClient{
				ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
					callCount++
					if callCount > 1 {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return []*azservicebus.ReceivedMessage{{MessageID: "m", Body: []byte("x")}}, nil
				},
			}
			cfg := tc.cfg
			cfg.AutoExtend = boolPtr(false)
			cfg.Client = mock

			recv, err := NewReceiver(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			var got *asbDelivery
			_ = recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
				got = del.(*asbDelivery)
				cancel()
				return nil
			})

			if got == nil {
				t.Fatal("no delivery captured")
			}
			if got.delayedRetryDisabled != tc.wantDisable {
				t.Fatalf("delayedRetryDisabled = %v, want %v", got.delayedRetryDisabled, tc.wantDisable)
			}
			if tc.wantDisable && got.scheduler != nil {
				t.Fatal("subscription delivery must not carry a scheduler (would fan out to topic)")
			}
		})
	}
}

// --- Finding 3: emit failure cancels the per-delivery context -------------

// When emit returns an error the poll loop must cancel the delivery context
// it handed to emit. That context parents the auto-extend goroutine, so
// cancelling it stops the goroutine and lets the broker lock lapse for
// redelivery — instead of holding the message invisible after Run returns.
func TestReceiver_EmitError_CancelsDeliveryContext(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{
		ReceiveMessagesFn: func(_ context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return []*azservicebus.ReceivedMessage{{MessageID: "emit-err", Body: []byte("x")}}, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		AutoExtend: boolPtr(false),
		Client:     mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("pipeline rejected delivery")
	var emitCtx context.Context
	runErr := recv.Run(context.Background(), func(c context.Context, _ ports.Delivery) error {
		emitCtx = c
		return sentinel
	})

	if !errors.Is(runErr, sentinel) {
		t.Fatalf("Run err = %v, want sentinel", runErr)
	}
	if emitCtx == nil {
		t.Fatal("emit was not called")
	}
	if emitCtx.Err() == nil {
		t.Fatal("delivery context must be cancelled after emit error (finding 3: prevents auto-extend leak)")
	}
}

// --- Finding 6: auto-extend max failures cancels the processing context ----

// After autoExtendMaxFailures consecutive lock-renewal errors the loop must
// cancel the processing (delivery) context — the one handed to emit — not
// just its own private auto-extend context. That signals the in-flight
// pipeline to abort, because the lock can no longer be held.
func TestAutoExtend_MaxFailures_CancelsProcessingContext(t *testing.T) {
	t.Parallel()

	// renewCalled is an unbuffered handshake: the loop's RenewMessageLock
	// call blocks on the send until the test receives, so each fake tick is
	// fully consumed before the test advances the clock again. This replaces
	// the previous sleep-based pacing with a deterministic rendezvous.
	renewCalled := make(chan struct{})
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCalled <- struct{}{}
			return errors.New("lock lost")
		},
	}

	deliveryCtx, deliveryCancel := context.WithCancel(context.Background())
	defer deliveryCancel()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "maxfail"})
	msg := &azservicebus.ReceivedMessage{MessageID: "maxfail"}
	fake := clocktest.New()
	d := newDelivery(deliveryCtx, env, mock, nil, msg, deliveryTuning{lockDuration: 2 * time.Second, autoExtend: true}, deliveryCancel, nil, nil, fake)
	defer d.stop()

	// newDelivery spawns autoExtendLoop, which registers its ticker from a
	// background goroutine. Wait until the ticker exists so the first Advance
	// cannot race ahead of registration and drop the tick.
	wait.Until(t, 5*time.Second, "autoExtendLoop registers its ticker", func() bool {
		return fake.TickerCount() > 0
	})

	// interval = lockDuration/2 = 1s, so each 1.1s Advance fires exactly one
	// tick. Block on the handshake after each so ticks are neither lost nor
	// coalesced (the fake ticker channel has capacity 1).
	for i := 0; i < autoExtendMaxFailures; i++ {
		fake.Advance(1100 * time.Millisecond)
		<-renewCalled
	}

	// After autoExtendMaxFailures consecutive errors the loop must cancel the
	// processing (delivery) context handed to emit — not just its own private
	// auto-extend context.
	select {
	case <-deliveryCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("processing context must be cancelled after auto-extend max failures (finding 6)")
	}
}

// --- Finding 7: max_wait_time bounds the receive --------------------------

// MaxWaitTime is applied as a per-receive context deadline (the Azure SDK
// has no max-wait option of its own).
func TestReceiver_MaxWaitTime_SetsReceiveDeadline(t *testing.T) {
	t.Parallel()

	var hadDeadline bool
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			_, hadDeadline = ctx.Deadline()
			return nil, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:   "q",
		MaxWaitTime: 5 * time.Second,
		Client:      mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := recv.ensureClient(context.Background()); err != nil {
		t.Fatalf("ensureClient: %v", err)
	}
	if _, err := recv.pollAndConvert(context.Background()); err != nil {
		t.Fatalf("pollAndConvert: %v", err)
	}
	if !hadDeadline {
		t.Fatal("ReceiveMessages context must carry the max_wait_time deadline")
	}
}

// A receive that ends because the per-receive max_wait_time elapsed (parent
// still live) is a normal idle long-poll, surfaced as (nil, nil) — not a
// transport error that would trigger poll backoff.
func TestReceiver_MaxWaitTime_DeadlineIsNormalEmptyPoll(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			// Block until our per-receive deadline fires, then report it —
			// exactly as the Azure SDK does on an idle long-poll.
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:   "q",
		MaxWaitTime: 5 * time.Millisecond,
		Client:      mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := recv.ensureClient(context.Background()); err != nil {
		t.Fatalf("ensureClient: %v", err)
	}
	raws, err := recv.pollAndConvert(context.Background())
	if err != nil {
		t.Fatalf("idle max_wait_time deadline must not surface as an error, got %v", err)
	}
	if len(raws) != 0 {
		t.Fatalf("expected no messages, got %d", len(raws))
	}
}

// A cancellation that originates from the parent context (graceful shutdown)
// must still surface as an error so the poll loop stops.
func TestReceiver_ParentCancel_SurfacesError(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:   "q",
		MaxWaitTime: 5 * time.Second,
		Client:      mock,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := recv.ensureClient(context.Background()); err != nil {
		t.Fatalf("ensureClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := recv.pollAndConvert(ctx); err == nil {
		t.Fatal("parent cancellation must surface as an error")
	}
}

// --- Finding 8: VisibilityTimeoutProvider ---------------------------------

// The Factory must declare a visibility window so the runtime validator can
// check SendTimeout against it.
func TestFactory_VisibilityTimeout(t *testing.T) {
	t.Parallel()

	f := NewFactory(nil)

	var vp ports.VisibilityTimeoutProvider = f
	if got := vp.VisibilityTimeout(); got != 30*time.Second {
		t.Fatalf("VisibilityTimeout = %v, want 30s", got)
	}
}

// --- Central egress header policy -----------------------------------------

// On egress the ASB ACL strips ONLY internal-only reserved headers (the
// bridge's own dispatch bookkeeping) and lets bridge-to-bridge propagated
// headers and application headers through as ApplicationProperties. The
// envelope is built with MustEnvelopeWithReserved so the reserved headers
// are actually present when envelopeToMessage runs (NewEnvelope would have
// stripped them at construction), exactly as the runtime pipeline hands a
// stamped envelope to the sender.
func TestEnvelopeToMessage_EgressHeaderPolicy(t *testing.T) {
	t.Parallel()

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "egress-1",
		Subject: "evt",
		Payload: []byte("{}"),
		Headers: map[string]any{
			// bridge-to-bridge propagated -> preserved
			messaging.HeaderCorrelationID:  "corr",
			messaging.HeaderCausationID:    "cause",
			messaging.HeaderIdempotencyKey: "idem",
			messaging.HeaderTenantID:       "acme",
			messaging.HeaderForwardedFrom:  "bridge-a",
			messaging.HeaderTraceParent:    "00-trace",
			// internal-only -> stripped
			messaging.HeaderRouteID:       "route",
			messaging.HeaderRouteOverride: "override",
			messaging.HeaderSourceID:      "source",
			messaging.HeaderContentType:   "application/json",
			// application header -> preserved
			"app-custom": "v",
		},
	})

	msg := envelopeToMessage(env, "", nil)

	for _, k := range []string{
		messaging.HeaderCorrelationID, messaging.HeaderCausationID,
		messaging.HeaderIdempotencyKey, messaging.HeaderTenantID,
		messaging.HeaderForwardedFrom, messaging.HeaderTraceParent, "app-custom",
	} {
		if _, ok := msg.ApplicationProperties[k]; !ok {
			t.Errorf("header %q should be preserved on egress, but was dropped", k)
		}
	}
	for _, k := range []string{
		messaging.HeaderRouteID, messaging.HeaderRouteOverride,
		messaging.HeaderSourceID, messaging.HeaderContentType,
	} {
		if _, ok := msg.ApplicationProperties[k]; ok {
			t.Errorf("internal-only header %q leaked to ApplicationProperties on egress", k)
		}
	}
}
