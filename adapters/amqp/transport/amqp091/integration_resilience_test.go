// Validates resilience scenarios that surface bugs only under realistic
// broker behaviour: multi-receiver reconnect fan-out, mandatory-return
// detection, and consumer-tag reuse across reconnects.
package amqp091

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestIntegration_TwoReceivers_BothResumeAfterReconnect validates that
// when two receivers share a session and the underlying connection drops,
// both receivers re-establish their consumer after the session reconnects.
//
// Without event fan-out, only one receiver would observe the
// SessionConnected event and resume; the other would hang until ctx
// expires.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	r1 ──┐                          ┌──▶ resumes
//	     ├──[shared session]──drop──┤
//	r2 ──┘                          └──▶ resumes (regression: hung)
//
// ───────────────────────────────────────────────
func TestIntegration_TwoReceivers_BothResumeAfterReconnect(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	queueA := rabbitmqlocal.UniqueQueue("dual-recon-a")
	queueB := rabbitmqlocal.UniqueQueue("dual-recon-b")
	exchange := rabbitmqlocal.UniqueExchange("dual-recon-ex")

	sess := NewSession(
		SessionOptions{
			BrokerURL:      ep,
			ReconnectDelay: 100 * time.Millisecond,
		},
		domain.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: queueA, Config: &Config{Subscription: SubscriptionParams{Exchange: exchange, RoutingKey: queueA}}},
			{Topic: queueB, Config: &Config{Subscription: SubscriptionParams{Exchange: exchange, RoutingKey: queueB}}},
		},
		Publishers: []domain.PublisherPlan{{Topic: exchange}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	r1 := NewReceiver(ReceiverConfig{QueueName: queueA, PrefetchCount: 1, Session: sess})
	r2 := NewReceiver(ReceiverConfig{QueueName: queueB, PrefetchCount: 1, Session: sess})

	runCtx, runCancel := context.WithTimeout(ctx, 50*time.Second)
	defer runCancel()

	var receivedA, receivedB atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = r1.Run(runCtx, func(_ context.Context, d ports.Delivery) error {
			receivedA.Add(1)
			return d.Ack(runCtx)
		})
	}()
	go func() {
		defer wg.Done()
		_ = r2.Run(runCtx, func(_ context.Context, d ports.Delivery) error {
			receivedB.Add(1)
			return d.Ack(runCtx)
		})
	}()

	wait.RequireClosed(t, r1.Started(), 5*time.Second)
	wait.RequireClosed(t, r2.Started(), 5*time.Second)

	sender := NewSender(SenderConfig{Exchange: exchange, Session: sess, Timeout: 5 * time.Second})
	if err := sender.Send(ctx, &domain.Envelope{ID: "pre-A", Subject: queueA, Payload: []byte("a1")}); err != nil {
		t.Fatalf("send pre-A: %v", err)
	}
	if err := sender.Send(ctx, &domain.Envelope{ID: "pre-B", Subject: queueB, Payload: []byte("b1")}); err != nil {
		t.Fatalf("send pre-B: %v", err)
	}

	wait.Until(t, 10*time.Second, "receivers A and B each got >=1 message", func() bool {
		return receivedA.Load() >= 1 && receivedB.Load() >= 1
	})

	// Drop the underlying TCP connection by closing it directly. The
	// session's reconnect loop should observe this via NotifyClose, then
	// reconnect and emit SessionConnected — both receivers must observe it.
	conn := sess.Connection()
	if conn == nil {
		t.Fatal("connection nil before drop")
	}
	if err := conn.Close(); err != nil {
		t.Logf("conn.Close (expected): %v", err)
	}

	wait.Until(t, 15*time.Second, "session reconnected", func() bool {
		return sess.Health(ctx).Connected
	})

	// Re-run reconcile after reconnect to ensure new sender channel is open
	// and queue exists (it does; we just need a fresh sender after drop).
	sender2 := NewSender(SenderConfig{Exchange: exchange, Session: sess, Timeout: 5 * time.Second})

	startA := receivedA.Load()
	startB := receivedB.Load()

	sendCtx, sendCancel := context.WithCancel(ctx)
	defer sendCancel()
	go func() {
		for sendCtx.Err() == nil {
			_ = sender2.Send(sendCtx, &domain.Envelope{ID: "post-A", Subject: queueA, Payload: []byte("a2")})
			_ = sender2.Send(sendCtx, &domain.Envelope{ID: "post-B", Subject: queueB, Payload: []byte("b2")})
			select {
			case <-sendCtx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	wait.Until(t, 15*time.Second, "both receivers resumed after reconnect", func() bool {
		return receivedA.Load() > startA && receivedB.Load() > startB
	})
	sendCancel()

	runCancel()
	wg.Wait()
}

// TestIntegration_Sender_MandatoryUnroutable_ReturnsError validates that
// sending with Mandatory=true to a routing key that matches no binding
// surfaces an error. Without basic.return handling the publish would
// silently succeed.
func TestIntegration_Sender_MandatoryUnroutable_ReturnsError(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exchange := rabbitmqlocal.UniqueExchange("mand-unrouted-ex")

	sess := NewSession(SessionOptions{BrokerURL: ep}, domain.SessionEphemeral, nil)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	plan := domain.SessionPlan{
		Publishers: []domain.PublisherPlan{{Topic: exchange}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchange,
		RoutingKey: "this.key.has.no.binding",
		Mandatory:  true,
		Timeout:    5 * time.Second,
		Session:    sess,
	})
	defer func() { _ = sender.Close(ctx) }()
	err := sender.Send(ctx, &domain.Envelope{
		ID:      "unrouted-1",
		Subject: "unused",
		Payload: []byte("nobody home"),
	})
	if err == nil {
		t.Fatal("Send with Mandatory=true to unbound routing key should return an error " +
			"(broker returned the message via basic.return)")
	}
	t.Logf("got expected error: %v", err)
}

// TestIntegration_Sender_MandatoryRouted_Succeeds validates that a
// mandatory message that does have a route is delivered normally.
func TestIntegration_Sender_MandatoryRouted_Succeeds(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queue := rabbitmqlocal.UniqueQueue("mand-routed")
	exchange := rabbitmqlocal.UniqueExchange("mand-routed-ex")

	sess := NewSession(SessionOptions{BrokerURL: ep}, domain.SessionEphemeral, nil)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: queue, Config: &Config{Subscription: SubscriptionParams{Exchange: exchange, RoutingKey: queue}}},
		},
		Publishers: []domain.PublisherPlan{{Topic: exchange}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchange,
		RoutingKey: queue,
		Mandatory:  true,
		Timeout:    5 * time.Second,
		Session:    sess,
	})
	defer func() { _ = sender.Close(ctx) }()
	if err := sender.Send(ctx, &domain.Envelope{
		ID:      "routed-1",
		Subject: queue,
		Payload: []byte("delivered"),
	}); err != nil {
		t.Fatalf("send mandatory routed: %v", err)
	}
}

// TestIntegration_ConsumerTag_ReuseAfterReconnect validates that a
// receiver with a user-supplied ConsumerTag survives a connection drop:
// the broker may still believe the previous tag is alive, so the receiver
// must avoid colliding with itself when re-consuming.
func TestIntegration_ConsumerTag_ReuseAfterReconnect(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	queue := rabbitmqlocal.UniqueQueue("tagreuse")
	exchange := rabbitmqlocal.UniqueExchange("tagreuse-ex")
	tag := "user-supplied-tag"

	sess := NewSession(
		SessionOptions{BrokerURL: ep, ReconnectDelay: 100 * time.Millisecond},
		domain.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: queue, Config: &Config{Subscription: SubscriptionParams{Exchange: exchange, RoutingKey: queue}}},
		},
		Publishers: []domain.PublisherPlan{{Topic: exchange}},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	recv := NewReceiver(ReceiverConfig{
		QueueName:     queue,
		ConsumerTag:   tag,
		PrefetchCount: 1,
		Session:       sess,
	})

	runCtx, runCancel := context.WithTimeout(ctx, 45*time.Second)
	defer runCancel()

	var received atomic.Int32
	go func() {
		_ = recv.Run(runCtx, func(_ context.Context, d ports.Delivery) error {
			received.Add(1)
			return d.Ack(runCtx)
		})
	}()

	wait.RequireClosed(t, recv.Started(), 5*time.Second)

	sender := NewSender(SenderConfig{Exchange: exchange, RoutingKey: queue, Session: sess, Timeout: 5 * time.Second})
	if err := sender.Send(ctx, &domain.Envelope{ID: "tag-pre", Payload: []byte("pre")}); err != nil {
		t.Fatalf("send pre: %v", err)
	}

	wait.Until(t, 5*time.Second, "receiver got pre-drop message", func() bool {
		return received.Load() >= 1
	})

	// Drop the connection without giving the broker time to clean up.
	conn := sess.Connection()
	if conn == nil {
		t.Fatal("conn nil before drop")
	}
	_ = conn.Close()

	wait.Until(t, 15*time.Second, "session reconnected", func() bool {
		return sess.Health(ctx).Connected
	})

	postSender := NewSender(SenderConfig{Exchange: exchange, RoutingKey: queue, Session: sess, Timeout: 5 * time.Second})
	startCount := received.Load()

	sendCtx, sendCancel := context.WithCancel(ctx)
	defer sendCancel()
	go func() {
		for sendCtx.Err() == nil {
			_ = postSender.Send(sendCtx, &domain.Envelope{ID: "tag-post", Payload: []byte("post")})
			select {
			case <-sendCtx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	wait.Until(t, 15*time.Second, "receiver resumed after reconnect with reused consumer tag", func() bool {
		return received.Load() > startCount
	})
	sendCancel()

	runCancel()
}
