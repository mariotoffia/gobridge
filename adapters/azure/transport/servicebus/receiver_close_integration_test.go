//go:build integration

package servicebus_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	servicebus "github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/asblocal"
)

// validates that cancelling the Run context triggers a graceful shutdown
// that completes within the 10-second close timeout.
func TestIntegration_ReceiverClose_GracefulShutdown(t *testing.T) {
	ctx := context.Background()
	queue := asblocal.TestQueue

	recv := newTestReceiver(t, servicebus.ReceiverConfig{
		QueueName: queue,
	})
	defer recv.Close(context.Background()) //nolint:errcheck

	runCtx, runCancel := context.WithCancel(ctx)

	runDone := make(chan error, 1)
	go func() {
		runDone <- recv.Run(runCtx, func(_ context.Context, del ports.Delivery) error {
			_ = del.Ack(runCtx)
			return nil
		})
	}()

	select {
	case <-recv.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not start")
	}

	// Cancel the context to trigger shutdown.
	start := time.Now()
	runCancel()

	select {
	case err := <-runDone:
		elapsed := time.Since(start)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
		if elapsed > 12*time.Second {
			t.Fatalf("graceful shutdown took %v, expected < 12s", elapsed)
		}
		t.Logf("graceful shutdown completed in %v", elapsed)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s after cancellation")
	}
}

// validates that the receiver can receive a message, process it, and then
// shut down cleanly when the context is cancelled.
func TestIntegration_ReceiverClose_ReceiveThenShutdown(t *testing.T) {
	ctx := context.Background()
	queue := asblocal.TestQueue

	// Send a message first.
	sender := newTestSender(t, queue)
	defer sender.Close(ctx) //nolint:errcheck

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        fmt.Sprintf("close-test-%d", time.Now().UnixNano()),
		Subject:   "close-test",
		Payload:   []byte("close-test-payload"),
		CreatedAt: time.Now(),
	})

	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recv := newTestReceiver(t, servicebus.ReceiverConfig{
		QueueName: queue,
	})
	defer recv.Close(context.Background()) //nolint:errcheck

	runCtx, runCancel := context.WithCancel(ctx)
	var received []ports.Delivery
	var mu sync.Mutex

	runDone := make(chan error, 1)
	go func() {
		runDone <- recv.Run(runCtx, func(ctx context.Context, del ports.Delivery) error {
			if err := del.Ack(ctx); err != nil {
				return err
			}
			mu.Lock()
			received = append(received, del)
			mu.Unlock()
			// Cancel after receiving the message.
			runCancel()
			return nil
		})
	}()

	select {
	case err := <-runDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(30 * time.Second):
		runCancel()
		t.Fatal("Run did not return within 30s")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) == 0 {
		t.Fatal("expected at least 1 delivery before shutdown")
	}

	got := received[0].Envelope()
	if string(got.Payload) != "close-test-payload" {
		t.Errorf("payload = %q, want %q", got.Payload, "close-test-payload")
	}
	t.Logf("received message %s and shut down cleanly", got.ID)
}
