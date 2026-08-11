package transport_test

// Deterministic tests for audit chunk, finding 2: SSE zero-delivery
// acknowledgement semantics. SSE egress is now SAFE-BY-DEFAULT:
// a broadcast that reached nobody returns a TRANSIENT (Unavailable-class)
// error so the route runner retries/DLQs instead of acking a lost event.
// Every zero-delivery outcome is also loud (ERROR level + counter).
// Accepting the loss (Send returns nil, source acked) requires the
// explicit Config.AtMostOnceAcceptLoss opt-in.
//
// No sleeps: the all-buffers-full case is driven by a blocking writer
// that parks the handler inside Write (so it never drains the client
// queue), letting the test fill the per-client buffer deterministically.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// blockingSSEWriter is an http.ResponseWriter + http.Flusher +
// SetWriteDeadline whose first Write parks until release is closed. It
// lets a test hold the SSE handler goroutine inside a single frame write
// so the per-client event queue is never drained — the deterministic way
// to fill the buffer and exercise the all-buffers-full drop path.
type blockingSSEWriter struct {
	hdr         http.Header
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func newBlockingSSEWriter() *blockingSSEWriter {
	return &blockingSSEWriter{
		hdr:     make(http.Header),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingSSEWriter) Header() http.Header { return w.hdr }
func (w *blockingSSEWriter) WriteHeader(int)     {}
func (w *blockingSSEWriter) Flush()              {}

func (w *blockingSSEWriter) Write(b []byte) (int, error) {
	w.enteredOnce.Do(func() { close(w.entered) })
	<-w.release
	return len(b), nil
}

// SetWriteDeadline is present so http.ResponseController reports write
// deadlines as SUPPORTED (returns nil), keeping the handler off the
// unsupported-deadline branch. The value is inert here.
func (w *blockingSSEWriter) SetWriteDeadline(time.Time) error { return nil }

// capturingHandler is a slog.Handler that records every emitted record so
// a test can assert a zero-delivery loss was logged at ERROR level.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// hasErrorContaining reports whether an ERROR-level record whose message
// contains sub was captured.
func (h *capturingHandler) hasErrorContaining(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == slog.LevelError && strings.Contains(r.Message, sub) {
			return true
		}
	}
	return false
}

// newChunk18Sender builds an SSESender via the public factory with the
// given config, recording exporter and logger. Mode defaults to "sse"
// and the heartbeat is pinned far out so it never fires mid-test.
func newChunk18Sender(t *testing.T, id string, cfg transport.Config, rec ports.MetricsExporter, logger *slog.Logger) *transport.SSESender {
	t.Helper()
	if cfg.Mode == "" {
		cfg.Mode = "sse"
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Hour
	}
	opts := []transport.FactoryOption{}
	if rec != nil {
		opts = append(opts, transport.WithFactoryMetrics(rec))
	}
	if logger != nil {
		opts = append(opts, transport.WithFactoryLogger(logger))
	}
	s, err := transport.NewFactory(opts...).NewSender(context.Background(),
		ports.SenderSpec{ID: id, Config: cfg}, nil)
	if err != nil {
		t.Fatalf("NewSender(%s): %v", id, err)
	}
	return s.(*transport.SSESender)
}

func chunk18Envelope(id string) ports.OutboundMessage {
	return ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: id, Subject: "s.sse", Payload: []byte(`{}`),
	})}
}

// Zero subscribers, default config: Send returns a TRANSIENT
// (Unavailable-class) error (the safe default) and the loss is loud —
// MetricSSENoSubscribers fires and an ERROR record is logged.
func TestSSE_ZeroSubscribers_DefaultFailsTransient(t *testing.T) {
	rec := &ports.RecordingExporter{}
	cap := &capturingHandler{}
	sender := newChunk18Sender(t, "zero-default", transport.Config{}, rec, slog.New(cap))

	err := sender.Send(context.Background(), chunk18Envelope("e0"))
	if err == nil {
		t.Fatal("default zero-delivery must return a transient error, got nil")
	}
	if !shared.IsRecoverableError(err) {
		t.Fatalf("zero-delivery error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("zero-delivery error must be Unavailable-class, got %v", err)
	}
	if got := len(rec.FindEntries(transport.MetricSSENoSubscribers)); got != 1 {
		t.Fatalf("expected 1 %s emission, got %d", transport.MetricSSENoSubscribers, got)
	}
	if !cap.hasErrorContaining("no subscribers") {
		t.Fatal("zero-delivery must be logged at ERROR level so silent loss is loud")
	}
	// Every no-subscriber emission must be tagged with the owning sender.
	assertTagged(t, rec.FindEntries(transport.MetricSSENoSubscribers), "sender_id", "zero-default")
}

// Zero subscribers with the explicit at_most_once_accept_loss opt-out:
// Send returns nil (accepts loss, source acked) but still counts the loss
// and logs it at ERROR level.
func TestSSE_ZeroSubscribers_AcceptLossAcksButLogsError(t *testing.T) {
	rec := &ports.RecordingExporter{}
	cap := &capturingHandler{}
	sender := newChunk18Sender(t, "zero-acceptloss",
		transport.Config{AtMostOnceAcceptLoss: true}, rec, slog.New(cap))

	if err := sender.Send(context.Background(), chunk18Envelope("e0")); err != nil {
		t.Fatalf("accept-loss zero-delivery must ack (nil), got %v", err)
	}
	if got := len(rec.FindEntries(transport.MetricSSENoSubscribers)); got != 1 {
		t.Fatalf("expected 1 %s emission, got %d", transport.MetricSSENoSubscribers, got)
	}
	if !cap.hasErrorContaining("no subscribers") {
		t.Fatal("zero-delivery must be logged at ERROR level even when loss is accepted")
	}
	assertTagged(t, rec.FindEntries(transport.MetricSSENoSubscribers), "sender_id", "zero-acceptloss")
}

// Zero subscribers, FailOnZeroDelivery set: Send returns a TRANSIENT
// (Unavailable-class) error instead of acking the producer.
func TestSSE_ZeroSubscribers_FailOnZeroReturnsTransient(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := newChunk18Sender(t, "zero-fail",
		transport.Config{FailOnZeroDelivery: true}, rec, nil)

	err := sender.Send(context.Background(), chunk18Envelope("e0"))
	if err == nil {
		t.Fatal("FailOnZeroDelivery: zero subscribers must return an error, got nil")
	}
	// Transient so the runner RETRIES (a briefly-disconnected subscriber
	// may reconnect) then DLQs per the route's retry-exhaustion policy —
	// never an immediate permanent dead-letter.
	if !shared.IsRecoverableError(err) {
		t.Fatalf("zero-delivery error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("zero-delivery error must be Unavailable-class, got %v", err)
	}
}

// All connected clients' buffers full: with FailOnZeroDelivery set, a
// broadcast that is dropped for 100% of subscribers returns the same
// transient error as the no-subscriber case; MetricSSEAllDropped fires
// and the loss is logged at ERROR level.
func TestSSE_AllBuffersFull_FailOnZeroReturnsTransient(t *testing.T) {
	rec := &ports.RecordingExporter{}
	cap := &capturingHandler{}
	// Buffer of 1: after the handler parks in Write draining the queue,
	// one queued event fills it and the next is dropped for the only
	// client → 100% dropped.
	sender := newChunk18Sender(t, "alldrop-fail",
		transport.Config{ClientBufferSize: 1, FailOnZeroDelivery: true}, rec, slog.New(cap))

	w := newBlockingSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest("GET", "/transport/http/senders/alldrop-fail/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(w, req)
		close(done)
	}()
	t.Cleanup(func() {
		close(w.release)
		cancel()
		wait.RequireClosed(t, done, 2*time.Second)
	})

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.ClientCount() >= 1
	})

	// e0 is queued, the handler reads it and parks inside Write — the
	// buffer is now empty and the handler will not drain further.
	if err := sender.Send(context.Background(), chunk18Envelope("e0")); err != nil {
		t.Fatalf("Send(e0): %v", err)
	}
	wait.RequireClosed(t, w.entered, 2*time.Second)

	// e1 fills the size-1 buffer (still a delivery: buffer had room).
	if err := sender.Send(context.Background(), chunk18Envelope("e1")); err != nil {
		t.Fatalf("Send(e1) filled the buffer, must still ack: %v", err)
	}
	// e2 finds the buffer full for the only client → 100% dropped.
	err := sender.Send(context.Background(), chunk18Envelope("e2"))
	if err == nil {
		t.Fatal("FailOnZeroDelivery: all-buffers-full must return an error, got nil")
	}
	if !shared.IsRecoverableError(err) {
		t.Fatalf("all-dropped error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("all-dropped error must be Unavailable-class, got %v", err)
	}
	if got := len(rec.FindEntries(transport.MetricSSEAllDropped)); got != 1 {
		t.Fatalf("expected 1 %s emission, got %d", transport.MetricSSEAllDropped, got)
	}
	if !cap.hasErrorContaining("all subscriber buffers full") {
		t.Fatal("all-dropped must be logged at ERROR level")
	}
	assertTagged(t, rec.FindEntries(transport.MetricSSEAllDropped), "sender_id", "alldrop-fail")
}

// All connected clients' buffers full, default config: the broadcast is
// dropped for 100% of subscribers, so Send returns the same TRANSIENT
// (Unavailable-class) error as the no-subscriber case (the safe
// default) and still counts the all-dropped loss on MetricSSEAllDropped.
func TestSSE_AllBuffersFull_DefaultFailsTransient(t *testing.T) {
	rec := &ports.RecordingExporter{}
	sender := newChunk18Sender(t, "alldrop-default",
		transport.Config{ClientBufferSize: 1}, rec, nil)

	w := newBlockingSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest("GET", "/transport/http/senders/alldrop-default/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(w, req)
		close(done)
	}()
	t.Cleanup(func() {
		close(w.release)
		cancel()
		wait.RequireClosed(t, done, 2*time.Second)
	})

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.ClientCount() >= 1
	})

	if err := sender.Send(context.Background(), chunk18Envelope("e0")); err != nil {
		t.Fatalf("Send(e0): %v", err)
	}
	wait.RequireClosed(t, w.entered, 2*time.Second)
	if err := sender.Send(context.Background(), chunk18Envelope("e1")); err != nil {
		t.Fatalf("Send(e1): %v", err)
	}
	// e2 finds the buffer full for the only client → 100% dropped.
	err := sender.Send(context.Background(), chunk18Envelope("e2"))
	if err == nil {
		t.Fatal("default all-buffers-full must return a transient error, got nil")
	}
	if !shared.IsRecoverableError(err) {
		t.Fatalf("all-dropped error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("all-dropped error must be Unavailable-class, got %v", err)
	}
	if got := len(rec.FindEntries(transport.MetricSSEAllDropped)); got != 1 {
		t.Fatalf("expected 1 %s emission, got %d", transport.MetricSSEAllDropped, got)
	}
}

// after Close, Send must fail CLOSED with a TRANSIENT error rather
// than ack (or accept-loss) an event whose subscribers Close has torn
// down. The accept-loss subtest is the discriminating one — without the
// shutdown check Send would observe zero clients and return nil (silent
// drop during a reload/shutdown window); with it, Close wins and Send
// returns a retryable error so the source is not acked.
func TestSSE_SendAfterCloseReturnsTransient(t *testing.T) {
	cases := []struct {
		name string
		cfg  transport.Config
	}{
		{"safe_default", transport.Config{}},
		{"accept_loss_opt_out", transport.Config{AtMostOnceAcceptLoss: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := newChunk18Sender(t, "closed-"+tc.name, tc.cfg, &ports.RecordingExporter{}, nil)

			// No clients connected, so Close returns immediately after
			// closing the shutdown channel.
			if err := sender.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}

			err := sender.Send(context.Background(), chunk18Envelope("after-close"))
			if err == nil {
				t.Fatal("Send after Close must return a transient error, got nil")
			}
			if !shared.IsRecoverableError(err) {
				t.Fatalf("shutdown error must be transient/recoverable, got %v", err)
			}
			if !errors.Is(err, shared.ErrUnavailable) {
				t.Fatalf("shutdown error must be Unavailable-class, got %v", err)
			}
		})
	}
}

// assertTagged fails unless every entry carries a tag key=value.
func assertTagged(t *testing.T, entries []ports.MetricEntry, key, value string) {
	t.Helper()
	if len(entries) == 0 {
		t.Fatalf("no metric entries to check for tag %s=%s", key, value)
	}
	for i, e := range entries {
		found := false
		for _, tag := range e.Tags {
			if tag.Key == key && tag.Value == value {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("entry %d (%s) missing tag %s=%s; tags=%v", i, e.Name, key, value, e.Tags)
		}
	}
}
