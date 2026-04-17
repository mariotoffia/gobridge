// Validates session lifecycle contracts that previously had subtle gaps:
// concurrent Start ordering and redactURL parity with the amqp091 transport.
package amqp10

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/go-amqp"
)

// TestSession_Start_ConcurrentBlocksUntilReady validates that a second
// concurrent call to Start does not return success until the first call
// has actually established the connection. Previously the second caller
// observed s.starting=true and immediately returned nil even though
// s.conn was still nil; callers that interpret "Start returned nil" as
// "session is connected" then operated on a not-yet-ready session.
func TestSession_Start_ConcurrentBlocksUntilReady(t *testing.T) {
	s := newTestSession()

	dialStart := make(chan struct{}, 1)
	releaseDial := make(chan struct{})
	s.dial = func(_ context.Context, _ string, _ *amqp.ConnOptions) (amqpConn, error) {
		select {
		case dialStart <- struct{}{}:
		default:
		}
		<-releaseDial
		return &mockConn{}, nil
	}

	const callers = 5
	var returned atomic.Int32
	results := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range results {
		go func(idx int) {
			defer wg.Done()
			results[idx] = s.Start(context.Background())
			returned.Add(1)
		}(i)
	}

	<-dialStart
	time.Sleep(50 * time.Millisecond)

	if r := returned.Load(); r != 0 {
		t.Fatalf("%d of %d Start callers returned before dial completed; "+
			"concurrent Start must block (not silently report success while connecting)",
			r, callers)
	}
	if c := s.Conn(); c != nil {
		t.Fatal("Conn() returned non-nil before dial completed; concurrent Start race")
	}

	close(releaseDial)
	wg.Wait()

	if c := s.Conn(); c == nil {
		t.Fatal("Conn() nil after all Start calls returned")
	}
	for i, err := range results {
		if err != nil {
			t.Errorf("Start[%d] returned %v, want nil", i, err)
		}
	}

	_ = s.Close(context.Background())
}

// TestRedactURL_InvalidURL_Consistent validates that redactURL returns
// the same sentinel as the amqp091 transport for unparseable URLs, so
// log-grep tests across transports behave consistently.
func TestRedactURL_InvalidURL_Consistent(t *testing.T) {
	const sentinel = "<invalid-url>"
	got := redactURL("://bad url")
	if got != sentinel {
		t.Fatalf("redactURL(invalid) = %q, want %q (must match amqp091 sentinel)",
			got, sentinel)
	}
}
