// Validates that session events fan out to multiple subscribers,
// so independent goroutines (receivers, observers) do not steal
// each other's reconnect notifications.
package amqp091

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSession_Subscribe_FanOut validates that two independent subscribers
// each receive the same SessionConnected event. Without fan-out, only one
// of the two would observe it (Go channels deliver each value to a single
// receiver).
//
// Scenario:
// ───────────────────────────────────────────────
//
//	pushEvent(SessionConnected)
//	     ├──▶ subscriber A  (must receive)
//	     └──▶ subscriber B  (must receive)
//
// ───────────────────────────────────────────────
func TestSession_Subscribe_FanOut(t *testing.T) {
	s := newResilienceSession(nil)

	chA, unsubA := s.Subscribe()
	defer unsubA()
	chB, unsubB := s.Subscribe()
	defer unsubB()

	s.pushEvent(ports.SessionConnected, nil)

	wait := func(ch <-chan ports.SessionEvent, name string) {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscriber %s: channel closed unexpectedly", name)
			}
			if ev.Type != ports.SessionConnected {
				t.Fatalf("subscriber %s: type = %v, want SessionConnected", name, ev.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %s: did not receive event", name)
		}
	}

	wait(chA, "A")
	wait(chB, "B")
}

// TestSession_Subscribe_DoesNotStealLegacyEvents validates that subscribers
// added via Subscribe() do not drain the legacy Events() channel; both
// surfaces independently receive each event.
func TestSession_Subscribe_DoesNotStealLegacyEvents(t *testing.T) {
	s := newResilienceSession(nil)

	sub, unsub := s.Subscribe()
	defer unsub()

	s.pushEvent(ports.SessionConnected, nil)

	select {
	case ev := <-sub:
		if ev.Type != ports.SessionConnected {
			t.Fatalf("subscriber: type = %v", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber: did not receive event")
	}

	select {
	case ev := <-s.Events():
		if ev.Type != ports.SessionConnected {
			t.Fatalf("legacy: type = %v", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy Events(): did not receive event")
	}
}

// TestSession_Subscribe_UnsubscribeClosesChannel validates that the
// returned unsubscribe function closes the per-subscriber channel and
// removes it from the broadcast list (no goroutine leak).
func TestSession_Subscribe_UnsubscribeClosesChannel(t *testing.T) {
	s := newResilienceSession(nil)

	ch, unsub := s.Subscribe()
	unsub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after unsubscribe")
	}

	s.pushEvent(ports.SessionConnected, nil)
	if got := s.subscriberCount(); got != 0 {
		t.Fatalf("subscriber count after unsubscribe = %d, want 0", got)
	}
}

// TestSession_Close_ClosesAllSubscribers validates that closing the
// session also closes every active subscriber channel.
func TestSession_Close_ClosesAllSubscribers(t *testing.T) {
	mc := newMockConnection()
	s := newResilienceSession(func(string) (amqpConnection, error) { return mc, nil })

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	chA, _ := s.Subscribe()
	chB, _ := s.Subscribe()

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for name, ch := range map[string]<-chan ports.SessionEvent{"A": chA, "B": chB} {
		select {
		case _, ok := <-ch:
			for ok {
				_, ok = <-ch
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %s: not closed after session Close", name)
		}
	}
}

// TestReceiver_TwoReceivers_BothReconnectAfterSessionConnected validates
// that when two receivers wait for reconnect on the same session, both
// observe the SessionConnected event (no event starvation).
//
// Without fan-out, only one of the two receivers would unblock — the
// other would hang until ctx expires.
func TestReceiver_TwoReceivers_BothReconnectAfterSessionConnected(t *testing.T) {
	sess := NewSession(
		SessionOptions{BrokerURL: "amqp://localhost/"},
		connectivity.SessionEphemeral,
		slog.Default(),
	)

	r1 := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q1"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}
	r2 := &Receiver{
		cfg:     ReceiverConfig{QueueName: "q2"},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	results := make(chan bool, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		results <- r1.waitForReconnect(ctx)
	}()
	go func() {
		defer wg.Done()
		results <- r2.waitForReconnect(ctx)
	}()

	wait.Until(t, 2*time.Second, "both receivers subscribed", func() bool {
		return sess.subscriberCount() >= 2
	})
	sess.pushEvent(ports.SessionConnected, nil)

	wg.Wait()
	close(results)

	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected both receivers to reconnect, got %d/2", count)
	}

	_ = sess.Close(context.Background())
}
