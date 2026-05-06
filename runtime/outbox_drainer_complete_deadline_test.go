package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

type senderFunc func(context.Context, *messaging.Envelope) error

func (f senderFunc) Send(ctx context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	return f(ctx, env)
}

func TestOutboxDrainer_CompleteSurvivesNearBatchDeadline(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	pk := persistence.OutboxPartitionKey("sess-complete-deadline", "")
	if _, err := leaseStore.Acquire(context.Background(), "sess-complete-deadline", token.Owner, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	rec := persistence.OutboxRecord{
		ID:         "rec-complete-deadline",
		RouteID:    "route-1",
		EnvelopeID: "env-complete-deadline",
		BindingID:  "bind-1",
		SessionID:  "sess-complete-deadline",
		Envelope:   messaging.Envelope{ID: "env-complete-deadline", Payload: []byte("payload")},
		Status:     persistence.OutboxPending,
	}
	if err := outbox.Persist(context.Background(), []persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	deadlineCh := make(chan time.Time, 1)
	outbox.CompleteCtxFn = func(ctx context.Context, _ []string, _ persistence.LeaseToken) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("complete context has no deadline")
		}
		deadlineCh <- deadline
		if remaining := time.Until(deadline); remaining <= 50*time.Millisecond {
			return context.DeadlineExceeded
		}
		return nil
	}

	policy := routing.RoutePolicy{}.WithDefaults()
	policy.SendTimeout = 300 * time.Millisecond
	batchCh := make(chan int, 1)
	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:           outbox,
		LeaseStore:            leaseStore,
		Sender:                &ctxAwareSender{latency: 105 * time.Millisecond},
		DLQ:                   goruntime.NewDLQRouter(dlqStore),
		RouteID:               "route-1",
		PartitionKey:          pk,
		LeaseID:               "sess-complete-deadline",
		OwnerID:               token.Owner,
		Policy:                policy,
		Strategy:              persistence.NewFixedPoll(10 * time.Millisecond),
		DrainBatchSize:        1,
		DrainMaxBatchSize:     1,
		DrainMaxConcurrency:   1,
		PerRecordDrainTimeout: 120 * time.Millisecond,
		MaxDrainTimeout:       120 * time.Millisecond,
		BatchTimeoutFloor:     10 * time.Millisecond,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
		OnBatchComplete: func(n int) { batchCh <- n },
	})

	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = drainer.Run(runCtx)
		close(done)
	}()

	select {
	case n := <-batchCh:
		if n != 1 {
			t.Fatalf("expected one completed record, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("drain did not complete; completed=%d", outbox.CompletedCount())
	}
	cancel()
	<-done

	select {
	case deadline := <-deadlineCh:
		if remaining := time.Until(deadline); remaining <= 0 {
			t.Fatalf("complete deadline was not in the future: %v", remaining)
		}
	default:
		t.Fatal("Complete was not called")
	}
	if got := outbox.CompletedCount(); got != 1 {
		t.Fatalf("expected record to transition to completed, got %d completed", got)
	}
}

func TestOutboxDrainer_CompleteRespectsRuntimeShutdown(t *testing.T) {
	token := persistence.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox := NewFakeOutboxStore()
	leaseStore := NewFakeLeaseStore()
	dlqStore := NewFakeDLQStore()

	pk := persistence.OutboxPartitionKey("sess-complete-shutdown", "")
	if _, err := leaseStore.Acquire(context.Background(), "sess-complete-shutdown", token.Owner, 30*time.Second, nil); err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	rec := persistence.OutboxRecord{
		ID:         "rec-complete-shutdown",
		RouteID:    "route-1",
		EnvelopeID: "env-complete-shutdown",
		BindingID:  "bind-1",
		SessionID:  "sess-complete-shutdown",
		Envelope:   messaging.Envelope{ID: "env-complete-shutdown", Payload: []byte("payload")},
		Status:     persistence.OutboxPending,
	}
	if err := outbox.Persist(context.Background(), []persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	completeStarted := make(chan struct{}, 1)
	completeReturned := make(chan time.Duration, 1)
	outbox.CompleteCtxFn = func(ctx context.Context, _ []string, _ persistence.LeaseToken) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		completeStarted <- struct{}{}
		start := time.Now()
		<-ctx.Done()
		completeReturned <- time.Since(start)
		return ctx.Err()
	}

	runCtx, cancel := context.WithCancel(context.Background())
	sender := senderFunc(func(_ context.Context, _ *messaging.Envelope) error {
		cancel()
		return nil
	})

	policy := routing.RoutePolicy{}.WithDefaults()
	policy.SendTimeout = 40 * time.Millisecond
	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:         outbox,
		LeaseStore:          leaseStore,
		Sender:              sender,
		DLQ:                 goruntime.NewDLQRouter(dlqStore),
		RouteID:             "route-1",
		PartitionKey:        pk,
		LeaseID:             "sess-complete-shutdown",
		OwnerID:             token.Owner,
		Policy:              policy,
		Strategy:            persistence.NewFixedPoll(10 * time.Millisecond),
		DrainBatchSize:      1,
		DrainMaxBatchSize:   1,
		DrainMaxConcurrency: 1,
		BatchTimeoutFloor:   100 * time.Millisecond,
		TokenFn: func() (persistence.LeaseToken, bool) {
			return token, true
		},
	})

	done := make(chan struct{})
	go func() {
		_ = drainer.Run(runCtx)
		close(done)
	}()

	select {
	case <-completeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Complete did not run after runtime shutdown")
	}

	select {
	case elapsed := <-completeReturned:
		if elapsed < 30*time.Millisecond {
			t.Fatalf("complete context was canceled by runtime shutdown instead of its timeout: %v", elapsed)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("complete context was not bounded by the complete timeout: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Complete did not return by its timeout")
	}
	<-done
}
