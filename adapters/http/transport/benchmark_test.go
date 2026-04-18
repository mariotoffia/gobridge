package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// BenchmarkReceiver_PostAck — HTTP POST through receiver to Ack.
// ---------------------------------------------------------------------------

func BenchmarkReceiver_PostAck(b *testing.B) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "bench-ack"}, nil)
	if err != nil {
		b.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			return d.Ack(context.Background())
		})
	}()
	<-recv.(ports.ReceiverStartedSignaler).Started()

	handler := factory.Handler()
	body, _ := json.Marshal(map[string]any{
		"subject": "bench.test",
		"payload": json.RawMessage(`{"key":"value"}`),
		"id":      "bench-msg",
	})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/transport/http/receivers/bench-ack/messages",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("expected 200, got %d", rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// BenchmarkSSE_Broadcast — broadcast to N SSE clients.
// ---------------------------------------------------------------------------

func BenchmarkSSE_Broadcast(b *testing.B) {
	for _, numClients := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("clients_%d", numClients), func(b *testing.B) {
			factory := transport.NewBridgeFactory()
			sender, err := factory.NewSender(context.Background(), config.SenderDef{
				ID:      fmt.Sprintf("bench-sse-%d", numClients),
				Options: map[string]any{"mode": "sse"},
			}, nil)
			if err != nil {
				b.Fatalf("NewSender: %v", err)
			}

			ts := httptest.NewServer(factory.Handler())
			defer ts.Close()

			path := fmt.Sprintf("/transport/http/senders/bench-sse-%d/events", numClients)

			// Connect N SSE clients.
			clients := make([]*http.Response, numClients)
			for i := 0; i < numClients; i++ {
				resp, err := http.Get(ts.URL + path)
				if err != nil {
					b.Fatalf("client %d: %v", i, err)
				}
				if resp.StatusCode != http.StatusOK {
					b.Fatalf("client %d: expected 200, got %d", i, resp.StatusCode)
				}
				clients[i] = resp
			}
			defer func() {
				for _, c := range clients {
					_ = c.Body.Close()
				}
			}()
			sseSender := sender.(*transport.SSESender)
			deadline := time.Now().Add(2 * time.Second)
			for sseSender.ClientCount() < numClients && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond) // OTHER: polling for SSE client registration in benchmark
			}

			env := &domain.Envelope{
				ID:      "bench-evt",
				Subject: "bench.broadcast",
				Payload: []byte(`{"data":"test"}`),
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				if err := sender.Send(context.Background(), env); err != nil {
					b.Fatalf("Send: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BenchmarkForwarder_Forward — cluster forwarding round-trip.
// ---------------------------------------------------------------------------

func BenchmarkForwarder_Forward(b *testing.B) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer remote.Close()

	fwd := transport.NewHTTPForwarder("/transport/http", 30*time.Second)
	peer := &domain.PeerInfo{
		InstanceID: "bench-remote",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := &domain.Envelope{
		ID:      "bench-fwd",
		Subject: "bench.forward",
		Payload: []byte(`{"order":"123"}`),
		Headers: map[string]any{"x-tenant": "acme"},
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := fwd.Forward(context.Background(), peer, "route-bench", env); err != nil {
			b.Fatalf("Forward: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// BenchmarkReceiver_PostAck_Parallel — concurrent POST processing.
// ---------------------------------------------------------------------------

func BenchmarkReceiver_PostAck_Parallel(b *testing.B) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "bench-par"}, nil)
	if err != nil {
		b.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			return d.Ack(context.Background())
		})
	}()
	<-recv.(ports.ReceiverStartedSignaler).Started()

	handler := factory.Handler()
	body, _ := json.Marshal(map[string]any{
		"subject": "bench.parallel",
		"payload": json.RawMessage(`{"key":"value"}`),
	})

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("POST", "/transport/http/receivers/bench-par/messages",
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("expected 200, got %d", rec.Code)
			}
		}
	})
}
