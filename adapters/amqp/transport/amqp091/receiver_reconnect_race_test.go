// Validates that the Receiver's waitForReconnect handles a race where
// the session has ALREADY reconnected (SessionConnected was published)
// before the receiver gets to Subscribe.
//
// Without a "current connection" check, Subscribe joins the fan-out
// list AFTER the event was pushed and there is no further event to
// observe — the receiver hangs until ctx expires (or forever, if no
// deadline).
package amqp091

import (
	"context"
	"log/slog"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mariotoffia/gobridge/ports"
)

// TestReceiver_WaitForReconnect_AlreadyConnected_DoesNotHang validates
// the case where a receiver loses its consumer channel, but by the time
// it enters waitForReconnect the session has already reconnected and
// emitted SessionConnected. Without an "is currently connected" probe,
// the receiver subscribes too late, never sees the event, and hangs
// until ctx cancels.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	receiver: consumeLoop returns (channel lost)
//	session:  reconnects FAST, pushEvent(SessionConnected)
//	receiver: enters waitForReconnect, Subscribe()
//	receiver: hangs waiting for an event that has already been delivered
//
// ───────────────────────────────────────────────
//
// Expected: waitForReconnect should observe the existing connected
// state via Health() and return true immediately.
func TestReceiver_WaitForReconnect_AlreadyConnected_DoesNotHang(t *testing.T) {
	mc := newMockConnection()
	mc.NotifyCloseFn = func(ch chan *amqp.Error) chan *amqp.Error { return ch }

	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()
	// Drain the initial SessionConnected event from the legacy channel
	// to mimic a normal observer that has already processed it. The
	// fan-out subscribers list is empty at this point.
	select {
	case <-sess.Events():
	case <-time.After(time.Second):
		t.Fatal("did not see initial SessionConnected event")
	}

	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	// At this point session reports Connected=true but no future events
	// will arrive because nothing actually disconnected. waitForReconnect
	// must observe the current connected state and return true; otherwise
	// it hangs until ctx expires.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- r.waitForReconnect(ctx) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitForReconnect returned false despite session being connected; " +
				"the receiver subscribes AFTER SessionConnected was emitted and " +
				"never observes the existing connected state (race window)")
		}
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("waitForReconnect hung past the 2.5s observation window even though " +
			"the session is already connected; receiver missed the event due to " +
			"late Subscribe()")
	}
}

// TestReceiver_WaitForReconnect_FastReconnect_DoesNotMissEvent validates
// the more realistic race: receiver loses its channel, session pushes
// SessionConnected event from the reconnect goroutine BEFORE the
// receiver's goroutine reaches Subscribe(). With the fix, the receiver
// must still observe the connected state and proceed.
func TestReceiver_WaitForReconnect_FastReconnect_DoesNotMissEvent(t *testing.T) {
	mc := newMockConnection()
	mc.NotifyCloseFn = func(ch chan *amqp.Error) chan *amqp.Error { return ch }

	sess := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()
	// Drain the initial event so the fan-out and legacy queues are clean.
	select {
	case <-sess.Events():
	case <-time.After(time.Second):
	}

	r := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	// Simulate: SessionConnected was pushed BEFORE waitForReconnect
	// even runs (e.g., reconnect happened while the receiver was still
	// in consumeLoop returning). We push it now (with no subscribers)
	// so it is dropped from the per-subscriber list — that is exactly
	// the race window we want to cover.
	sess.pushEvent(ports.SessionConnected, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- r.waitForReconnect(ctx) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitForReconnect returned false despite session being connected " +
				"(SessionConnected was emitted before Subscribe); receiver should " +
				"observe current connected state to break the race")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("waitForReconnect hung — the SessionConnected pushed before Subscribe " +
			"was lost and there is no current-state probe to recover")
	}

	if h := sess.Health(context.Background()); !h.Connected {
		t.Fatalf("session unexpectedly not connected: %+v", h)
	}
}
