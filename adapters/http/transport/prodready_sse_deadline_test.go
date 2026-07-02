package transport_test

// Deterministic tests for the SSE per-write deadline introduced in the
// prod-ready remediation (findings: unbounded SSE writes / fronting
// WriteTimeout killing healthy streams). Both tests inject a fake clock
// so the asserted deadline values are exact, and a fake ResponseWriter
// exposing SetWriteDeadline so the handler's http.ResponseController
// path is driven without a real socket — no sleeps, no wall-clock
// flakiness.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// fakeSSEWriter is an http.ResponseWriter + http.Flusher that also
// implements SetWriteDeadline (the method http.ResponseController looks
// for). It records every armed deadline and signals every Write so a
// test can synchronise deterministically. When failErr is non-nil every
// Write returns it, simulating a connection whose per-write deadline has
// elapsed.
type fakeSSEWriter struct {
	mu        sync.Mutex
	hdr       http.Header
	deadlines []time.Time
	failErr   error
	writes    chan struct{}
}

func newFakeSSEWriter(failErr error) *fakeSSEWriter {
	return &fakeSSEWriter{
		hdr:     make(http.Header),
		failErr: failErr,
		writes:  make(chan struct{}, 16),
	}
}

func (w *fakeSSEWriter) Header() http.Header { return w.hdr }

func (w *fakeSSEWriter) WriteHeader(int) {}

func (w *fakeSSEWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	failErr := w.failErr
	w.mu.Unlock()
	select {
	case w.writes <- struct{}{}:
	default:
	}
	if failErr != nil {
		return 0, failErr
	}
	return len(b), nil
}

func (w *fakeSSEWriter) Flush() {}

func (w *fakeSSEWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, t)
	w.mu.Unlock()
	return nil
}

func (w *fakeSSEWriter) lastDeadline() (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.deadlines) == 0 {
		return time.Time{}, false
	}
	return w.deadlines[len(w.deadlines)-1], true
}

// newDeadlineSSESender builds an SSESender bound to a fake clock with the
// given WriteTimeout and a 1h heartbeat (so the heartbeat never fires
// during the test). It returns the sender and the fake clock.
func newDeadlineSSESender(t *testing.T, id string, writeTimeout time.Duration, start time.Time) (*transport.SSESender, *clocktest.Fake) {
	t.Helper()
	fake := clocktest.NewAt(start)
	factory := transport.NewFactory(transport.WithClock(fake))
	s, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: id,
		Config: transport.Config{
			Mode:              "sse",
			WriteTimeout:      writeTimeout,
			HeartbeatInterval: time.Hour,
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s.(*transport.SSESender), fake
}

// The per-write deadline must be re-armed before every frame using the
// current clock, so a healthy long-lived stream keeps pushing its
// deadline forward (overriding a fronting server's fixed WriteTimeout)
// instead of being killed mid-stream.
func TestSSESender_PerWriteDeadlineReArmsEachFrame(t *testing.T) {
	const writeTimeout = 5 * time.Second
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	sender, fake := newDeadlineSSESender(t, "sse-dl", writeTimeout, start)

	w := newFakeSSEWriter(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/transport/http/senders/sse-dl/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(w, req)
		close(done)
	}()

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.ClientCount() >= 1
	})

	// First frame: deadline armed at start + writeTimeout.
	send(t, sender, "e1")
	wait.RequireReceive(t, w.writes, 2*time.Second)
	if dl, ok := w.lastDeadline(); !ok || !dl.Equal(start.Add(writeTimeout)) {
		t.Fatalf("first frame deadline = %v (ok=%v), want %v", dl, ok, start.Add(writeTimeout))
	}

	// Advance the clock; the next frame must re-arm relative to the new
	// "now", proving the deadline is not a one-shot from connect time.
	fake.Advance(10 * time.Second)
	send(t, sender, "e2")
	wait.RequireReceive(t, w.writes, 2*time.Second)
	want := start.Add(10 * time.Second).Add(writeTimeout)
	if dl, ok := w.lastDeadline(); !ok || !dl.Equal(want) {
		t.Fatalf("re-armed deadline = %v (ok=%v), want %v", dl, ok, want)
	}

	cancel()
	wait.RequireClosed(t, done, 2*time.Second)
}

// A client whose write blocks until the per-write deadline elapses
// (modelled by a Write returning os.ErrDeadlineExceeded) must be evicted
// — the handler returns and the client is removed — instead of pinning
// the broadcast goroutine forever.
func TestSSESender_SlowClientEvictedOnWriteTimeout(t *testing.T) {
	const writeTimeout = 5 * time.Second
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	sender, _ := newDeadlineSSESender(t, "sse-evict", writeTimeout, start)

	w := newFakeSSEWriter(os.ErrDeadlineExceeded)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/transport/http/senders/sse-evict/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(w, req)
		close(done)
	}()

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.ClientCount() >= 1
	})

	send(t, sender, "evt")

	// The failing write must arm a deadline, then cause the handler to
	// return and evict the client.
	wait.RequireClosed(t, done, 2*time.Second)
	if _, ok := w.lastDeadline(); !ok {
		t.Fatal("expected a per-write deadline to be armed before the failing write")
	}
	wait.Until(t, 2*time.Second, "slow client evicted", func() bool {
		return sender.ClientCount() == 0
	})
}

// send broadcasts a one-off envelope with the given ID through the SSE
// sender.
func send(t *testing.T, sender *transport.SSESender, id string) {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      id,
		Subject: "s.topic",
		Payload: []byte(`{}`),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send(%s): %v", id, err)
	}
}
