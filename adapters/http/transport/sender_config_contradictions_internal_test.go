package transport

// White-box tests for the adversarial remediation of the SSE sender's
// ack-correctness invariants. These reach unexported state (s.clients,
// s.closing, sseClient.done, drainOnShutdown) to drive the exact races the
// black-box surface cannot force deterministically. No sleeps, no network:
// the fan-out is non-blocking so every case resolves synchronously.
//
//   - issue 1: a subscriber that deregistered (done closed) but is still in
//     the map must NOT count as a delivery target; a broadcast reaching only
//     such clients returns transient ErrUnavailable, never a false ack.
//   - issue 2: once Close has marked s.closing, Send must refuse to ack even
//     when a live client's buffer has room (else Close could drop the event).
//   - issue 3: drainOnShutdown flushes already-enqueued (already-acked)
//     events so a graceful Close does not silently drop them.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// recordingWriter is a minimal http.ResponseWriter + http.Flusher that
// records every frame written. It deliberately omits SetWriteDeadline so
// http.NewResponseController reports deadlines unsupported (armWriteDeadline
// then no-ops), keeping the drain path free of any wall-clock dependency.
type recordingWriter struct {
	hdr     http.Header
	writes  [][]byte
	flushes int
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{hdr: make(http.Header)}
}

func (w *recordingWriter) Header() http.Header { return w.hdr }
func (w *recordingWriter) WriteHeader(int)     {}
func (w *recordingWriter) Flush()              { w.flushes++ }

func (w *recordingWriter) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	w.writes = append(w.writes, cp)
	return len(b), nil
}

func r5Envelope(id string) ports.OutboundMessage {
	return ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: id, Subject: "s.r5", Payload: []byte(`{}`),
	})}
}

// --- issue 1: a deregistered subscriber is not a delivery target -------

// A client whose handler has exited (done closed) yet is momentarily still
// in the map has a buffered channel that would ACCEPT a write nobody will
// ever read. The pre-fix snapshot fan-out enqueued there and counted it as
// delivery, acking a broadcast that reached zero live subscribers. The
// live-map fan-out must skip a done client, so the broadcast reaches nobody
// and — under the safe default — returns transient ErrUnavailable.
func TestSSESender_Send_DeregisteredSubscriberIsNotDelivery(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newSSESender(sseSenderConfig{
		id:      "sse-gone",
		metrics: rec,
		clock:   clocktest.New(),
		// acceptZeroDeliveryLoss defaults false: reaching nobody live must
		// surface a transient error, never an ack.
	})
	// done CLOSED (handler gone) but buffer has room: the discriminating
	// shape. A snapshot fan-out would enqueue and miscount delivery.
	done := make(chan struct{})
	close(done)
	gone := &sseClient{id: "gone", events: make(chan []byte, 4), done: done}
	s.clients["gone"] = gone

	err := s.Send(context.Background(), r5Envelope("evt-gone"))
	if err == nil {
		t.Fatal("Send to a deregistered-only subscriber set must not ack, got nil")
	}
	if !shared.IsRecoverableError(err) {
		t.Fatalf("zero-live-delivery error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("zero-live-delivery error must be Unavailable-class, got %v", err)
	}
	// The event must NOT have been enqueued into the gone client's buffer.
	select {
	case <-gone.events:
		t.Fatal("event was enqueued to a deregistered client (phantom delivery)")
	default:
	}
	if n := len(rec.FindEntries(MetricSSEAllDropped)); n != 1 {
		t.Fatalf("expected 1 %s (delivered to nobody live), got %d", MetricSSEAllDropped, n)
	}
}

// A live client (nil/open done) alongside a gone one must still receive the
// event — the done skip must not suppress a real delivery, and one live
// enqueue is enough to ack.
func TestSSESender_Send_LiveSubscriberDeliversDespiteGonePeer(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newSSESender(sseSenderConfig{id: "sse-mixed", metrics: rec, clock: clocktest.New()})

	goneDone := make(chan struct{})
	close(goneDone)
	s.clients["gone"] = &sseClient{id: "gone", events: make(chan []byte, 4), done: goneDone}
	live := &sseClient{id: "live", events: make(chan []byte, 4), done: make(chan struct{})}
	s.clients["live"] = live

	if err := s.Send(context.Background(), r5Envelope("evt-mixed")); err != nil {
		t.Fatalf("a live subscriber received the event, Send must ack: %v", err)
	}
	if got := len(live.events); got != 1 {
		t.Fatalf("live client must have exactly the one enqueued event, got %d", got)
	}
	if n := len(rec.FindEntries(MetricSSEAllDropped)); n != 0 {
		t.Fatalf("a live delivery must not count %s, got %d", MetricSSEAllDropped, n)
	}
}

// --- issue 2: no ack once Close has marked closing --------------------

// With s.closing set (as Close sets it under the write lock before closing
// shutdown), Send must refuse to ack EVEN THOUGH a live client's buffer has
// room and accept-loss is enabled — otherwise Close could tear the stream
// down after Send acked, dropping the event. Accept-loss is the
// discriminating knob: without the closing guard Send would enqueue and
// return nil here.
func TestSSESender_Send_ClosingRefusesAckWithLiveBuffer(t *testing.T) {
	s := newSSESender(sseSenderConfig{
		id:                     "sse-closing",
		acceptZeroDeliveryLoss: true,
		metrics:                &ports.RecordingExporter{},
		clock:                  clocktest.New(),
	})
	live := &sseClient{id: "live", events: make(chan []byte, 4), done: make(chan struct{})}
	s.clients["live"] = live

	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()

	err := s.Send(context.Background(), r5Envelope("evt-closing"))
	if err == nil {
		t.Fatal("Send once closing is marked must not ack, got nil")
	}
	if !shared.IsRecoverableError(err) {
		t.Fatalf("closing error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("closing error must be Unavailable-class, got %v", err)
	}
	select {
	case <-live.events:
		t.Fatal("event enqueued despite closing — Close could drop an acked event")
	default:
	}
}

// --- issue 3: drainOnShutdown flushes already-acked events -------------

// Events already enqueued to a client (already reported delivered by Send)
// must be written to the wire when the handler unwinds on Close, not
// abandoned. drainOnShutdown makes a single non-blocking pass — the buffer
// is quiescent because Send refuses to enqueue once closing is set — and
// flushes every queued frame.
func TestSSESender_drainOnShutdown_FlushesQueuedEvents(t *testing.T) {
	s := newSSESender(sseSenderConfig{id: "sse-drain", clock: clocktest.New()})
	client := &sseClient{id: "c", events: make(chan []byte, 4), done: make(chan struct{})}
	client.events <- []byte("event: message\ndata: a\n\n")
	client.events <- []byte("event: message\ndata: b\n\n")

	w := newRecordingWriter()
	rc := http.NewResponseController(w)

	s.drainOnShutdown(w, rc, w, client)

	if got := len(w.writes); got != 2 {
		t.Fatalf("drainOnShutdown must write both queued events, wrote %d", got)
	}
	if string(w.writes[0]) != "event: message\ndata: a\n\n" ||
		string(w.writes[1]) != "event: message\ndata: b\n\n" {
		t.Fatalf("drainOnShutdown wrote frames out of order or corrupted: %q", w.writes)
	}
	// A fully drained buffer leaves nothing behind.
	select {
	case <-client.events:
		t.Fatal("drainOnShutdown left an event in the buffer")
	default:
	}
}

// parkingWriter parks its FIRST Write until release is closed (recording
// the frame once it proceeds), then records every subsequent frame. It
// lets a test hold the SSE handler inside Write(e0) so a second event can
// be enqueued+acked and the shutdown/drain path exercised deterministically.
type parkingWriter struct {
	mu       sync.Mutex
	hdr      http.Header
	writes   [][]byte
	entered  chan struct{}
	release  chan struct{}
	parkOnce sync.Once
}

func newParkingWriter() *parkingWriter {
	return &parkingWriter{
		hdr:     make(http.Header),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *parkingWriter) Header() http.Header { return w.hdr }
func (w *parkingWriter) WriteHeader(int)     {}
func (w *parkingWriter) Flush()              {}

func (w *parkingWriter) Write(b []byte) (int, error) {
	w.parkOnce.Do(func() {
		close(w.entered)
		<-w.release
	})
	w.mu.Lock()
	w.writes = append(w.writes, append([]byte(nil), b...))
	w.mu.Unlock()
	return len(b), nil
}

func (w *parkingWriter) wroteContaining(sub string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.writes {
		if strings.Contains(string(b), sub) {
			return true
		}
	}
	return false
}

// End-to-end wiring for issue 3: an event Send already ACKED (enqueued
// before Close) must be written to the wire when the handler unwinds on
// shutdown, not abandoned. Deterministic by construction:
//   - e0 is read by the handler, which parks inside Write(e0) — the buffer
//     is now empty and the handler is not draining.
//   - e1 is enqueued while s.closing is still false, so Send ACKS it.
//   - s.closing is set and s.shutdown is closed IN ORDER *before* the write
//     is released, so when the handler unwinds it MUST take the priority
//     shutdown branch (never the event case), and that branch must drain e1.
//
// Because the priority branch is the deterministic route here, a mutation
// that drops drainOnShutdown from it (returning without draining) fails
// this test every run — the acked e1 never reaches the wire.
func TestSSESender_ServeHTTP_DrainsAckedEventOnShutdown(t *testing.T) {
	s := newSSESender(sseSenderConfig{
		id:                "drain-wire",
		heartbeatInterval: time.Hour, // fake clock; never fires mid-test
		clientBufferSize:  4,
		clock:             clocktest.New(),
	})
	w := newParkingWriter()
	req := httptest.NewRequest("GET", "/transport/http/senders/drain-wire/events", nil)

	handlerDone := make(chan struct{})
	go func() {
		s.ServeHTTP(w, req)
		close(handlerDone)
	}()

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return s.ClientCount() >= 1
	})

	// e0: handler reads it and parks inside Write(e0); buffer now empty.
	if err := s.Send(context.Background(), r5Envelope("e0")); err != nil {
		t.Fatalf("Send(e0): %v", err)
	}
	wait.RequireClosed(t, w.entered, 2*time.Second)

	// e1: enqueued while not closing → ACKED (buffered), awaiting a read.
	if err := s.Send(context.Background(), r5Envelope("e1")); err != nil {
		t.Fatalf("Send(e1) must ack an enqueue to a live client: %v", err)
	}

	// Shut down in the same order Close does — mark closing under the lock,
	// then close shutdown — BEFORE releasing the parked write, so the
	// handler is guaranteed to unwind into the priority shutdown branch.
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	close(s.shutdown)

	close(w.release)
	wait.RequireClosed(t, handlerDone, 2*time.Second)

	if !w.wroteContaining(`"id":"e1"`) {
		t.Fatal("acked event e1 was not drained to the wire on shutdown (issue 3)")
	}
}
