package transport_test

// Deterministic test for audit chunk, finding 4: adapter metrics must
// be tagged with the owning component id. Before the fix every SSE sender
// emitted the SSEClients gauge on the SAME untagged series, so with more
// than one sender in a process the last writer clobbered the others. Here
// two senders with distinct ids share one exporter and must produce two
// distinct, correctly-tagged SSEClients series.

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestMetrics_MultiSenderSSEClientsTaggedDistinctly(t *testing.T) {
	rec := &ports.RecordingExporter{}
	// One factory, one shared exporter, two senders — exactly the
	// single-process, multi-route topology that used to clobber.
	factory := transport.NewFactory(transport.WithFactoryMetrics(rec))

	newSender := func(id string) *transport.SSESender {
		s, err := factory.NewSender(context.Background(), ports.SenderSpec{
			ID: id,
			Config: transport.Config{
				Mode:              "sse",
				HeartbeatInterval: time.Hour, // never fires mid-test
			},
		}, nil)
		if err != nil {
			t.Fatalf("NewSender(%s): %v", id, err)
		}
		return s.(*transport.SSESender)
	}
	senderA := newSender("sender-a")
	senderB := newSender("sender-b")

	// Connect one client to each sender; each connect emits the
	// SSEClients gauge tagged with the owning sender id.
	connect := func(s *transport.SSESender, id string) {
		w := newFakeSSEWriter(nil)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req := httptest.NewRequest("GET", "/transport/http/senders/"+id+"/events", nil).WithContext(ctx)
		done := make(chan struct{})
		go func() {
			s.ServeHTTP(w, req)
			close(done)
		}()
		t.Cleanup(func() {
			cancel()
			wait.RequireClosed(t, done, 2*time.Second)
		})
		wait.Until(t, 2*time.Second, "client connected to "+id, func() bool {
			return s.ClientCount() >= 1
		})
	}
	connect(senderA, "sender-a")
	connect(senderB, "sender-b")

	entries := rec.FindEntries(transport.MetricSSEClients)
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 %s emissions (one per sender), got %d", transport.MetricSSEClients, len(entries))
	}

	// Every SSEClients emission MUST carry a sender_id tag, and both
	// senders' ids must be represented — proof the series did not
	// collapse onto one last-writer-wins series.
	seen := map[string]bool{}
	for _, e := range entries {
		id := tagValue(e.Tags, "sender_id")
		if id == "" {
			t.Fatalf("%s emitted untagged (no sender_id): %+v", transport.MetricSSEClients, e)
		}
		seen[id] = true
	}
	if !seen["sender-a"] || !seen["sender-b"] {
		t.Fatalf("expected distinct sender_id series for sender-a AND sender-b, saw %v", seen)
	}
}

// tagValue returns the value of the first tag with the given key, or "".
func tagValue(tags []shared.Tag, key string) string {
	for _, tag := range tags {
		if tag.Key == key {
			return tag.Value
		}
	}
	return ""
}
