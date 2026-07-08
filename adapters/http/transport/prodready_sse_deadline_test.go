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
// current WALL clock (finding 8: SetWriteDeadline is an OS/kernel socket
// deadline, so the sender uses time.Now(), not the injected clock), so a
// healthy long-lived stream keeps pushing its deadline forward
// (overriding a fronting server's fixed WriteTimeout) instead of being
// killed mid-stream. Asserted with a monotonic wall-clock bracket
// [before+timeout, after+timeout] — deterministic (never exact-equal to a
// read clock), never flaky.
func TestSSESender_PerWriteDeadlineReArmsEachFrame(t *testing.T) {
	const writeTimeout = 5 * time.Second
	sender, _ := newDeadlineSSESender(t, "sse-dl", writeTimeout, time.Now())

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

	// First frame: the deadline is armed from the wall clock observed
	// during the write, so it lands in [before+timeout, after+timeout].
	before1 := time.Now()
	send(t, sender, "e1")
	wait.RequireReceive(t, w.writes, 2*time.Second)
	after1 := time.Now()
	dl1, ok := w.lastDeadline()
	if !ok {
		t.Fatal("first frame armed no write deadline")
	}
	if dl1.Before(before1.Add(writeTimeout)) || dl1.After(after1.Add(writeTimeout)) {
		t.Fatalf("first frame deadline %v outside [%v, %v]", dl1,
			before1.Add(writeTimeout), after1.Add(writeTimeout))
	}

	// Second frame must arm a FRESH deadline (re-armed each frame, not a
	// one-shot from connect): at or after the first, and inside its own
	// wall-clock bracket.
	before2 := time.Now()
	send(t, sender, "e2")
	wait.RequireReceive(t, w.writes, 2*time.Second)
	after2 := time.Now()
	dl2, ok := w.lastDeadline()
	if !ok {
		t.Fatal("second frame armed no write deadline")
	}
	if dl2.Before(before2.Add(writeTimeout)) || dl2.After(after2.Add(writeTimeout)) {
		t.Fatalf("second frame deadline %v outside [%v, %v]", dl2,
			before2.Add(writeTimeout), after2.Add(writeTimeout))
	}
	if dl2.Before(dl1) {
		t.Fatalf("deadline must be re-armed forward each frame: dl2 %v < dl1 %v", dl2, dl1)
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

// newMetricsSSESender builds an SSESender via the public factory wired to
// a recording metrics exporter with the given WriteTimeout (heartbeat
// pinned to 1h so it never fires mid-test).
func newMetricsSSESender(t *testing.T, id string, writeTimeout time.Duration) (*transport.SSESender, *recordingMetrics) {
	t.Helper()
	rec := &recordingMetrics{}
	factory := transport.NewFactory(transport.WithFactoryMetrics(rec))
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
	return s.(*transport.SSESender), rec
}

// When the ResponseWriter chain cannot set a per-write deadline,
// slow-client eviction is inert and a stalled reader can pin the handler
// goroutine (HTTP-N1). A bare httptest.ResponseRecorder is exactly such a
// writer — it flushes but has no SetWriteDeadline, so
// http.ResponseController.SetWriteDeadline returns http.ErrNotSupported —
// so the handler must emit MetricSSEDeadlineUnsupported. Operators need a
// countable signal to alert on, not just a log line.
func TestSSESender_DeadlineUnsupportedEmitsMetric(t *testing.T) {
	sender, rec := newMetricsSSESender(t, "sse-nodl", 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/transport/http/senders/sse-nodl/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	wait.Until(t, 2*time.Second, "deadline-unsupported metric emitted", func() bool {
		return rec.counterCount(transport.MetricSSEDeadlineUnsupported) >= 1
	})

	cancel()
	wait.RequireClosed(t, done, 2*time.Second)
}

// Negative control: a writer that DOES support SetWriteDeadline keeps
// eviction working, so the unsupported-deadline metric must stay at zero.
func TestSSESender_DeadlineSupportedDoesNotEmitMetric(t *testing.T) {
	sender, rec := newMetricsSSESender(t, "sse-hasdl", 5*time.Second)

	w := newFakeSSEWriter(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/transport/http/senders/sse-hasdl/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(w, req)
		close(done)
	}()

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.ClientCount() >= 1
	})
	// Drive one frame so the handler has provably passed the deadline probe
	// before asserting the metric stayed at zero.
	send(t, sender, "e1")
	wait.RequireReceive(t, w.writes, 2*time.Second)

	if got := rec.counterCount(transport.MetricSSEDeadlineUnsupported); got != 0 {
		t.Fatalf("deadline-supported writer must not emit %s, got %d", transport.MetricSSEDeadlineUnsupported, got)
	}

	cancel()
	wait.RequireClosed(t, done, 2*time.Second)
}
