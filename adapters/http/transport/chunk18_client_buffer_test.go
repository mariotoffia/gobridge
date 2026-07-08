package transport_test

// Deterministic test for audit chunk C18, finding 7: the per-client SSE
// event-queue depth is configurable via Config.ClientBufferSize (formerly
// hardcoded at 256). A sender built with a small buffer must drop the
// (N+1)th queued event — proof the configured size, not the 256 default,
// governs the queue. Driven by a blocking writer that parks the handler
// inside Write so the queue is never drained; no sleeps.

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestChunk18_SSE_ClientBufferSizeHonored(t *testing.T) {
	const bufferSize = 3
	rec := &ports.RecordingExporter{}
	sender := newChunk18Sender(t, "buf-cfg",
		transport.Config{ClientBufferSize: bufferSize}, rec, nil)

	w := newBlockingSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest("GET", "/transport/http/senders/buf-cfg/events", nil).WithContext(ctx)
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

	// e0 is dequeued by the handler, which then parks inside Write. The
	// buffer is now empty and stays undrained for the rest of the test.
	if err := sender.Send(context.Background(), chunk18Envelope("e0")); err != nil {
		t.Fatalf("Send(e0): %v", err)
	}
	wait.RequireClosed(t, w.entered, 2*time.Second)

	// Exactly bufferSize events fit without a drop.
	for i := 0; i < bufferSize; i++ {
		if err := sender.Send(context.Background(), chunk18Envelope("fill")); err != nil {
			t.Fatalf("Send(fill %d): %v", i, err)
		}
	}
	if got := len(rec.FindEntries(transport.MetricSSEDroppedEvents)); got != 0 {
		t.Fatalf("filling exactly %d slots must not drop, got %d drop(s) — buffer smaller than configured?",
			bufferSize, got)
	}

	// The next event overflows the configured buffer and is dropped. With
	// the old hardcoded 256 buffer this event would still fit, so the drop
	// proves ClientBufferSize is honored.
	if err := sender.Send(context.Background(), chunk18Envelope("overflow")); err != nil {
		t.Fatalf("Send(overflow) must still ack by default: %v", err)
	}
	drops := rec.FindEntries(transport.MetricSSEDroppedEvents)
	if len(drops) != 1 {
		t.Fatalf("the (N+1)th event must drop exactly once, got %d", len(drops))
	}
	assertTagged(t, drops, "sender_id", "buf-cfg")
}
