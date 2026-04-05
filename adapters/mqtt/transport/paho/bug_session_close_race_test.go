package paho

import (
	"context"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// GAP-2 (QA): MQTT Session Close vs pushEvent Race
//
// Close() sets s.closed=true then closes s.events. pushEvent checks s.closed
// under the same mutex. This test verifies no panic occurs when pushEvent
// is called concurrently with Close (e.g., from OnConnectionDown callback).
// ═══════════════════════════════════════════════════════════════════════════

// TestSession_ConcurrentPushEventAndClose launches goroutines pushing events
// while Close() is called concurrently. Must not panic on send-to-closed-channel.
func TestSession_ConcurrentPushEventAndClose(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "close-race-test",
	}, domain.SessionEphemeral, nil)

	var wg sync.WaitGroup
	const pushers = 10
	const pushIterations = 100

	// Launch goroutines pushing events.
	for range pushers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range pushIterations {
				sess.pushEvent(ports.SessionReconnecting, domain.ErrUnavailable)
			}
		}()
	}

	// Close concurrently while events are being pushed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Close(context.Background())
	}()

	// All goroutines must finish without panic.
	wg.Wait()
}

// TestSession_CloseIdempotent verifies that calling Close multiple times
// does not panic or corrupt state.
func TestSession_CloseIdempotent(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "close-idempotent-test",
	}, domain.SessionEphemeral, nil)

	// Close twice — second should be a no-op (no panic on double channel close).
	err1 := sess.Close(context.Background())
	err2 := sess.Close(context.Background())
	if err1 != nil {
		t.Logf("first close error: %v", err1)
	}
	if err2 != nil {
		t.Logf("second close error: %v", err2)
	}
}
