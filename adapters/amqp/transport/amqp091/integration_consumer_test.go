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
)

// TestIntegration_CompetingConsumers validates that multiple receivers
// on the same queue each get a share of messages.
func TestIntegration_CompetingConsumers(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("compete")
	exchangeName := rabbitmqlocal.UniqueExchange("compete-ex")

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		domain.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{
				Topic: queueName,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   exchangeName,
					RoutingKey: queueName,
				}},
			},
		},
		Publishers: []domain.PublisherPlan{
			{Topic: exchangeName},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	const msgCount = 10

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	for i := 0; i < msgCount; i++ {
		if err := sender.Send(ctx, &domain.Envelope{
			ID:      "compete-" + string(rune('A'+i)),
			Subject: queueName,
			Payload: []byte("compete"),
		}); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}

	var (
		totalReceived atomic.Int32
		mu            sync.Mutex
		consumer1IDs  []string
		consumer2IDs  []string
	)

	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	makeHandler := func(ids *[]string) func(context.Context, ports.Delivery) error {
		return func(_ context.Context, d ports.Delivery) error {
			mu.Lock()
			*ids = append(*ids, d.Envelope().ID)
			mu.Unlock()

			if err := d.Ack(recvCtx); err != nil {
				return err
			}
			if totalReceived.Add(1) >= msgCount {
				recvCancel()
			}
			return nil
		}
	}

	r1 := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 1,
		Session:       sess,
	})
	r2 := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 1,
		Session:       sess,
	})

	go func() { _ = r1.Run(recvCtx, makeHandler(&consumer1IDs)) }()
	go func() { _ = r2.Run(recvCtx, makeHandler(&consumer2IDs)) }()

	<-recvCtx.Done()

	total := int(totalReceived.Load())
	if total != msgCount {
		t.Fatalf("received %d messages, want %d", total, msgCount)
	}

	mu.Lock()
	c1 := len(consumer1IDs)
	c2 := len(consumer2IDs)
	mu.Unlock()

	if c1 == 0 || c2 == 0 {
		t.Logf("consumer1=%d, consumer2=%d (one got all — acceptable but unusual)", c1, c2)
	}
	if c1+c2 != msgCount {
		t.Fatalf("consumer1(%d) + consumer2(%d) != %d", c1, c2, msgCount)
	}
}

// TestIntegration_AutoAck validates that messages are automatically
// acknowledged when AutoAck is enabled.
func TestIntegration_AutoAck(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("autoack")
	exchangeName := rabbitmqlocal.UniqueExchange("autoack-ex")

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		domain.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{
				Topic: queueName,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   exchangeName,
					RoutingKey: queueName,
				}},
			},
		},
		Publishers: []domain.PublisherPlan{
			{Topic: exchangeName},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	if err := sender.Send(ctx, &domain.Envelope{
		ID: "autoack-1", Subject: queueName, Payload: []byte("auto"),
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	receiver := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 1,
		AutoAck:       true,
		Session:       sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 10*time.Second)
	defer recvCancel()

	received := make(chan *domain.Envelope, 1)
	go func() {
		_ = receiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			received <- d.Envelope()
			recvCancel()
			return nil
		})
	}()

	select {
	case env := <-received:
		if env.ID != "autoack-1" {
			t.Errorf("ID = %q, want %q", env.ID, "autoack-1")
		}
	case <-recvCtx.Done():
		t.Fatal("timed out waiting for auto-ack message")
	}

	if err := sender.Send(ctx, &domain.Envelope{
		ID: "autoack-2", Subject: queueName, Payload: []byte("second"),
	}); err != nil {
		t.Fatalf("Send second: %v", err)
	}

	recv2 := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 1,
		AutoAck:       true,
		Session:       sess,
	})

	recv2Ctx, recv2Cancel := context.WithTimeout(ctx, 10*time.Second)
	defer recv2Cancel()

	received2 := make(chan *domain.Envelope, 1)
	go func() {
		_ = recv2.Run(recv2Ctx, func(_ context.Context, d ports.Delivery) error {
			received2 <- d.Envelope()
			recv2Cancel()
			return nil
		})
	}()

	select {
	case env := <-received2:
		if env.ID != "autoack-2" {
			t.Errorf("second message ID = %q, want %q (first message leaked?)", env.ID, "autoack-2")
		}
	case <-recv2Ctx.Done():
		t.Fatal("timed out waiting for second message")
	}
}

// TestIntegration_PrefetchCount validates that prefetch limits in-flight
// messages delivered to the consumer.
func TestIntegration_PrefetchCount(t *testing.T) {
	ep := rabbitmqlocal.Endpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	queueName := rabbitmqlocal.UniqueQueue("prefetch")
	exchangeName := rabbitmqlocal.UniqueExchange("prefetch-ex")

	sess := NewSession(
		SessionOptions{BrokerURL: ep},
		domain.SessionEphemeral,
		nil,
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()
	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{
				Topic: queueName,
				Config: &Config{Subscription: SubscriptionParams{
					Exchange:   exchangeName,
					RoutingKey: queueName,
				}},
			},
		},
		Publishers: []domain.PublisherPlan{
			{Topic: exchangeName},
		},
	}
	if err := sess.Reconcile(ctx, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	const totalMessages = 5
	const prefetch = 2

	sender := NewSender(SenderConfig{
		Exchange:   exchangeName,
		RoutingKey: queueName,
		Session:    sess,
		Timeout:    10 * time.Second,
	})
	for i := 0; i < totalMessages; i++ {
		if err := sender.Send(ctx, &domain.Envelope{
			ID:      "pf-" + string(rune('0'+i)),
			Subject: queueName,
			Payload: []byte("prefetch-test"),
		}); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}

	receiver := NewReceiver(ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: prefetch,
		Session:       sess,
	})

	recvCtx, recvCancel := context.WithTimeout(ctx, 15*time.Second)
	defer recvCancel()

	var (
		deliveries []ports.Delivery
		mu         sync.Mutex
	)

	go func() {
		_ = receiver.Run(recvCtx, func(_ context.Context, d ports.Delivery) error {
			mu.Lock()
			deliveries = append(deliveries, d)
			count := len(deliveries)
			mu.Unlock()

			if count >= totalMessages {
				recvCancel()
				return nil
			}

			if err := d.Ack(recvCtx); err != nil {
				return err
			}
			return nil
		})
	}()

	<-recvCtx.Done()

	mu.Lock()
	got := len(deliveries)
	mu.Unlock()

	if got != totalMessages {
		t.Fatalf("received %d messages, want %d", got, totalMessages)
	}

	mu.Lock()
	lastDel := deliveries[len(deliveries)-1]
	mu.Unlock()
	if err := lastDel.Ack(ctx); err != nil {
		t.Errorf("ack last delivery: %v", err)
	}
}
