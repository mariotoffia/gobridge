// Validates that session events fan out to multiple subscribers,
// so independent goroutines (receivers, observers) do not steal
// each other's reconnect notifications.
package amqp10

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSession_Subscribe_FanOut validates that two independent subscribers
// each receive the same SessionConnected event.
func TestSession_Subscribe_FanOut(t *testing.T) {
	s := newTestSession()

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
	s := newTestSession()

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
// removes it from the broadcast list.
func TestSession_Subscribe_UnsubscribeClosesChannel(t *testing.T) {
	s := newTestSession()

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
	s := newTestSession()
	s.dial = mockDialFunc(&mockConn{}, nil)

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

// TestSession_Subscribe_ClosedSession validates that Subscribe on a
// closed session returns an already-closed channel.
func TestSession_Subscribe_ClosedSession(t *testing.T) {
	s := newTestSession()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	ch, unsub := s.Subscribe()
	defer unsub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed for closed session")
		}
	case <-time.After(time.Second):
		t.Fatal("channel from closed session was not pre-closed")
	}
}

// TestSession_Subscribe_TwoReceivers_BothReconnectAfterSessionConnected
// validates that when two observers wait for reconnect on the same
// session, both observe the SessionConnected event (no starvation).
func TestSession_Subscribe_TwoReceivers_BothReconnectAfterSessionConnected(t *testing.T) {
	s := newTestSession()

	var wg sync.WaitGroup
	results := make(chan bool, 2)

	// Simulate two receivers waiting for reconnect.
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, unsub := s.Subscribe()
			defer unsub()
			select {
			case ev, ok := <-events:
				results <- ok && ev.Type == ports.SessionConnected
			case <-time.After(3 * time.Second):
				results <- false
			}
		}()
	}

	// STARTUP: wait for both goroutines to have subscribed.
	wait.Until(t, time.Second, "subscribers registered", func() bool {
		return s.subscriberCount() >= 2
	})
	s.pushEvent(ports.SessionConnected, nil)

	wg.Wait()
	close(results)

	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected both observers to reconnect, got %d/2", count)
	}
}
