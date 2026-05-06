//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	amqp091adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	amqp10adapter "github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/rabbitmqlocal"
)

// ---------------------------------------------------------------------------
// AMQP 0-9-1 (RabbitMQ) helpers
// ---------------------------------------------------------------------------

func setupRabbitMQSession(
	t *testing.T, mode connectivity.SessionMode,
) *amqp091adapter.Session {
	t.Helper()
	ep := rabbitmqlocal.Endpoint(t)
	sess := amqp091adapter.NewSession(amqp091adapter.SessionOptions{
		BrokerURL:      ep,
		Heartbeat:      10 * time.Second,
		ConnectTimeout: 30 * time.Second,
	}, mode, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx), "RabbitMQ session Start")

	select {
	case <-sess.Events():
	case <-time.After(10 * time.Second):
	}

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

func newRabbitMQSender(
	t *testing.T, sess *amqp091adapter.Session,
	exchange, routingKey string,
) *amqp091adapter.Sender {
	t.Helper()
	return amqp091adapter.NewSender(amqp091adapter.SenderConfig{
		Exchange:   exchange,
		RoutingKey: routingKey,
		Timeout:    30 * time.Second,
		Session:    sess,
	})
}

func newRabbitMQReceiver(
	t *testing.T, sess *amqp091adapter.Session, queueName string,
) *amqp091adapter.Receiver {
	t.Helper()
	return amqp091adapter.NewReceiver(amqp091adapter.ReceiverConfig{
		QueueName:     queueName,
		PrefetchCount: 10,
		Session:       sess,
	})
}

// ---------------------------------------------------------------------------
// AMQP 1.0 (Artemis) helpers
// ---------------------------------------------------------------------------

func setupArtemisSession(
	t *testing.T, mode connectivity.SessionMode,
) *amqp10adapter.Session {
	t.Helper()
	ep := artemislocal.Endpoint(t)
	user, pass := artemislocal.Credentials()

	sess := amqp10adapter.NewSession(amqp10adapter.SessionOptions{
		Address:        ep,
		Username:       user,
		Password:       pass,
		ConnectTimeout: 30 * time.Second,
		IdleTimeout:    2 * time.Minute,
	}, mode, testLogger(t))

	ctx := context.Background()
	require.NoError(t, sess.Start(ctx), "Artemis session Start")

	select {
	case <-sess.Events():
	case <-time.After(10 * time.Second):
	}

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

func newArtemisSender(
	t *testing.T, sess *amqp10adapter.Session, address string,
) *amqp10adapter.Sender {
	t.Helper()
	s, err := amqp10adapter.NewSender(amqp10adapter.SenderConfig{
		Address: address,
		Timeout: 30 * time.Second,
		Session: sess,
	}, sess)
	require.NoError(t, err, "NewArtemisSender")
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func newArtemisReceiver(
	t *testing.T, sess *amqp10adapter.Session, address string,
) *amqp10adapter.Receiver {
	t.Helper()
	r, err := amqp10adapter.NewReceiver(amqp10adapter.ReceiverConfig{
		Address:    address,
		LinkCredit: 10,
		Session:    sess,
	}, sess)
	require.NoError(t, err, "NewArtemisReceiver")
	return r
}

// ---------------------------------------------------------------------------
// amqpCollector — subscribes and collects envelopes from AMQP queues
// ---------------------------------------------------------------------------

type amqpCollector struct {
	mu       sync.Mutex
	messages []*messaging.Envelope
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newRabbitMQCollector(
	t *testing.T, queueName string,
) *amqpCollector {
	t.Helper()
	sess := setupRabbitMQSession(t, connectivity.SessionEphemeral)
	recv := newRabbitMQReceiver(t, sess, queueName)
	recvCtx, recvCancel := context.WithCancel(context.Background())

	c := &amqpCollector{cancel: recvCancel}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			c.mu.Lock()
			c.messages = append(c.messages, del.Envelope())
			c.mu.Unlock()
			_ = del.Ack(recvCtx)
			return nil
		})
	}()

	t.Cleanup(func() {
		recvCancel()
		c.wg.Wait()
	})
	return c
}

func newArtemisCollector(
	t *testing.T, address string,
) *amqpCollector {
	t.Helper()
	sess := setupArtemisSession(t, connectivity.SessionEphemeral)
	recv := newArtemisReceiver(t, sess, address)
	recvCtx, recvCancel := context.WithCancel(context.Background())

	c := &amqpCollector{cancel: recvCancel}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		_ = recv.Run(recvCtx, func(_ context.Context, del ports.Delivery) error {
			c.mu.Lock()
			c.messages = append(c.messages, del.Envelope())
			c.mu.Unlock()
			_ = del.Ack(recvCtx)
			return nil
		})
	}()

	t.Cleanup(func() {
		recvCancel()
		c.wg.Wait()
	})
	return c
}

func (c *amqpCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

func (c *amqpCollector) getMessages() []*messaging.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]*messaging.Envelope, len(c.messages))
	copy(cp, c.messages)
	return cp
}

func countUniqueAMQP(c *amqpCollector) int {
	msgs := c.getMessages()
	seen := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		seen[m.ID] = struct{}{}
	}
	return len(seen)
}

// ---------------------------------------------------------------------------
// sendToRabbitMQ — send bulk messages directly to RabbitMQ
// ---------------------------------------------------------------------------

func sendToRabbitMQ(
	t *testing.T, sender *amqp091adapter.Sender,
	count int, idPrefix string,
) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		env := &messaging.Envelope{
			ID:        fmt.Sprintf("%s-%d", idPrefix, i),
			Subject:   "test",
			Payload:   []byte(fmt.Sprintf(`{"seq":%d}`, i)),
			CreatedAt: time.Now().UTC(),
		}
		require.NoError(t, sender.Send(ctx, env), "sendToRabbitMQ msg %d", i)
	}
}

// sendToArtemis sends bulk messages directly to Artemis.
func sendToArtemis(
	t *testing.T, sender *amqp10adapter.Sender,
	count int, idPrefix string,
) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		env := &messaging.Envelope{
			ID:        fmt.Sprintf("%s-%d", idPrefix, i),
			Subject:   "test",
			Payload:   []byte(fmt.Sprintf(`{"seq":%d}`, i)),
			CreatedAt: time.Now().UTC(),
		}
		require.NoError(t, sender.Send(ctx, env), "sendToArtemis msg %d", i)
	}
}

// amqpLogger returns a logger for AMQP tests that respects GOBRIDGE_LOG_LEVEL.
func amqpLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return testLogger(t)
}
