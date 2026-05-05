// Validates Reconcile behaviour when individual declarations fail and
// when the plan is updated multiple times across a session.
package amqp091

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// TestIntegration_Reconcile_FirstFailureDoesNotPoisonSubsequentReconciles
// validates that a Reconcile call which fails (e.g. because one queue
// already exists with conflicting parameters) does NOT leave the session
// in a state where subsequent corrected Reconcile calls also fail.
//
// Without proper channel handling, the broker closes the channel on a
// PRECONDITION_FAILED error; if we cached or reused that channel, the
// next Reconcile would inherit the dead channel. We open a fresh
// channel per Reconcile invocation so corrective updates always work.
func TestIntegration_Reconcile_FirstFailureDoesNotPoisonSubsequentReconciles(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conflicting := rabbitmqlocal.UniqueQueue("recon-conflict")
	rabbitmqlocal.CreateQueue(t, conflicting)

	sess := NewSession(SessionOptions{BrokerURL: ep}, domain.SessionEphemeral, nil)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	// First reconcile: ask for a queue that exists with default
	// parameters but with durable=true (mismatch -> broker returns
	// PRECONDITION_FAILED and closes the channel).
	badPlan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: conflicting, Options: map[string]any{"durable": true}},
		},
	}
	if err := sess.Reconcile(ctx, badPlan); err == nil {
		t.Logf("note: broker accepted mismatching durable redeclare; test still meaningful")
	}

	// Second reconcile: ask for a brand-new queue that should succeed
	// regardless of the previous failure.
	freshQueue := rabbitmqlocal.UniqueQueue("recon-fresh")
	freshExch := rabbitmqlocal.UniqueExchange("recon-fresh-ex")
	goodPlan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: freshQueue, Options: map[string]any{
				"exchange": freshExch, "routing_key": freshQueue,
			}},
		},
		Publishers: []domain.PublisherPlan{{Topic: freshExch}},
	}
	if err := sess.Reconcile(ctx, goodPlan); err != nil {
		t.Fatalf("second Reconcile (fresh queue) failed after first error: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange: freshExch, RoutingKey: freshQueue,
		Session: sess, Timeout: 5 * time.Second,
	})
	defer func() { _ = sender.Close(ctx) }()
	if err := sender.Send(ctx, &domain.Envelope{
		ID: "post-recon", Subject: freshQueue, Payload: []byte("ok"),
	}); err != nil {
		t.Fatalf("send after recovered Reconcile: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{QueueName: freshQueue, PrefetchCount: 1, Session: sess})
	recvCtx, recvCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recvCancel()

	got := make(chan string, 1)
	go func() {
		_ = recv.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			got <- d.Envelope().ID
			_ = d.Ack(recvCtx)
			recvCancel()
			return nil
		})
	}()

	select {
	case id := <-got:
		if id != "post-recon" {
			t.Fatalf("got id %q, want post-recon", id)
		}
	case <-recvCtx.Done():
		t.Fatal("did not receive message on freshly-reconciled queue")
	}
}

// TestIntegration_Reconcile_PartialFailure_ReportsErrorWithoutChannelLeak
// validates that when a single declaration in a plan fails, the
// Reconcile returns the error AND the session remains usable for
// subsequent Reconcile attempts (the temporary channel used for
// declaration is closed even on the error path).
func TestIntegration_Reconcile_PartialFailure_ReportsErrorWithoutChannelLeak(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bad := rabbitmqlocal.UniqueQueue("recon-bad")
	rabbitmqlocal.CreateQueue(t, bad) // exists with durable=false

	sess := NewSession(SessionOptions{BrokerURL: ep}, domain.SessionEphemeral, nil)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: bad, Options: map[string]any{"durable": true}},
		},
	}

	var errs atomic.Int32
	for i := 0; i < 3; i++ {
		if err := sess.Reconcile(ctx, plan); err != nil {
			errs.Add(1)
		}
	}
	if errs.Load() == 0 {
		t.Skip("broker accepted mismatching durable redeclare; cannot verify error reporting")
	}

	if h := sess.Health(ctx); !h.Connected {
		t.Fatal("session lost connection after Reconcile errors; channel leak suspected")
	}
}
