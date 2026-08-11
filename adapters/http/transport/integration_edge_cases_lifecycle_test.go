package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"

	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// TestEdge_DuplicateReceiverID_ReturnsError
// ---------------------------------------------------------------------------

func TestEdge_DuplicateReceiverID_ReturnsError(t *testing.T) {
	factory := transport.NewFactory()

	_, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "dup"}, nil)
	if err != nil {
		t.Fatalf("first NewReceiver: %v", err)
	}

	_, err = factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "dup"}, nil)
	if err == nil {
		t.Fatal("expected error for duplicate receiver ID, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected 'duplicate' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// C1b. TestEdge_DuplicateSenderID_ReturnsError
// ---------------------------------------------------------------------------

func TestEdge_DuplicateSenderID_ReturnsError(t *testing.T) {
	factory := transport.NewFactory()

	_, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "dup-sse", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("first NewSender: %v", err)
	}

	_, err = factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "dup-sse", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err == nil {
		t.Fatal("expected error for duplicate sender ID, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected 'duplicate' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestEdge_InvalidExpiresAtReturns400
// ---------------------------------------------------------------------------

func TestEdge_InvalidExpiresAtReturns400(t *testing.T) {
	cases := []struct {
		name      string
		expiresAt string
	}{
		{"plain_text", "tomorrow"},
		{"wrong_format", "2026-13-01T00:00:00Z"},
		{"unix_timestamp", "1700000000"},
		{"partial_date", "2026-03-28"},
	}

	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "bad-exp"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			return d.Ack(context.Background())
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, factory.Handler(),
				"/transport/http/receivers/bad-exp/messages",
				map[string]any{
					"subject":    "test",
					"payload":    json.RawMessage(`{}`),
					"expires_at": tc.expiresAt,
				}, nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "expires_at") {
				t.Fatalf("expected 'expires_at' in error, got: %s", rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C2b. TestEdge_ValidExpiresAtAccepted
// ---------------------------------------------------------------------------

func TestEdge_ValidExpiresAtAccepted(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "good-exp"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deliveryCh := make(chan ports.Delivery, 1)
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			deliveryCh <- d
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	type httpResult struct{ rec *httptest.ResponseRecorder }
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(),
			"/transport/http/receivers/good-exp/messages",
			map[string]any{
				"subject":    "test",
				"payload":    json.RawMessage(`{}`),
				"expires_at": "2026-12-31T23:59:59Z",
			}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	select {
	case d := <-deliveryCh:
		env := d.Envelope()
		if env.ExpiresAt().IsZero() {
			t.Fatal("expected non-zero ExpiresAt")
		}
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	select {
	case res := <-resultCh:
		if res.rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}
}

// ---------------------------------------------------------------------------
// TestEdge_SSEHeartbeatDelivered
// ---------------------------------------------------------------------------

func TestEdge_SSEHeartbeatDelivered(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-hb",
		Config: transport.Config{
			Mode:              "sse",
			HeartbeatInterval: 200 * time.Millisecond,
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-hb/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Wait long enough for at least one heartbeat (200ms interval).
	buf := make([]byte, 4096)
	deadline := time.After(2 * time.Second)
	var collected []byte

	for {
		doneCh := make(chan int, 1)
		go func() {
			n, _ := resp.Body.Read(buf)
			doneCh <- n
		}()

		select {
		case n := <-doneCh:
			collected = append(collected, buf[:n]...)
			if strings.Contains(string(collected), ": heartbeat") {
				return // success
			}
		case <-deadline:
			t.Fatalf("timeout waiting for heartbeat, got:\n%s", collected)
		}
	}
}

// ---------------------------------------------------------------------------
// TestEdge_ConcurrentPOSTProcessing
// ---------------------------------------------------------------------------

func TestEdge_ConcurrentPOSTProcessing(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "concurrent"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed atomic.Int64
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			processed.Add(1)
			return d.Ack(context.Background())
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"subject": "concurrent.test",
				"payload": json.RawMessage(`{}`),
				"id":      fmt.Sprintf("msg-%d", idx),
			})
			resp, err := http.Post(
				ts.URL+"/transport/http/receivers/concurrent/messages",
				"application/json",
				bytes.NewReader(body),
			)
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- errors.New(resp.Status)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent POST error: %v", err)
	}

	waitFor(t, 2*time.Second, "all messages processed", func() bool {
		return processed.Load() >= n
	})
}

// ---------------------------------------------------------------------------
// TestEdge_CustomPathPrefix
// ---------------------------------------------------------------------------

func TestEdge_CustomPathPrefix(t *testing.T) {
	factory := transport.NewFactory(transport.WithPathPrefix("/custom/api"))

	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "pfx"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deliveryCh := make(chan ports.Delivery, 1)
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			deliveryCh <- d
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	type httpResult struct{ rec *httptest.ResponseRecorder }
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(), "/custom/api/receivers/pfx/messages",
			map[string]any{"subject": "pfx.test", "payload": json.RawMessage(`{}`)}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	select {
	case d := <-deliveryCh:
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery on custom path")
	}

	select {
	case res := <-resultCh:
		if res.rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}

	if factory.PathPrefix() != "/custom/api" {
		t.Fatalf("PathPrefix: got %q, want /custom/api", factory.PathPrefix())
	}
}

// ---------------------------------------------------------------------------
// TestEdge_ForwarderContextCancelled
// ---------------------------------------------------------------------------

func TestEdge_ForwarderContextCancelled(t *testing.T) {
	// Slow remote that takes longer than context allows.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second) // OTHER: simulates slow remote for context cancellation test
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()
	fwd := transport.NewHTTPForwarder("/transport/http", 10*time.Second)

	peer := &persistence.PeerInfo{
		InstanceID: "slow-peer",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "ctx-cancel-1",
		Subject: "test.cancel",
		Payload: []byte(`{}`),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := fwd.Forward(ctx, peer, "route-cancel", env)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, shared.ErrForwardFailed) {
		t.Fatalf("expected ErrForwardFailed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestEdge_NilPayload
// ---------------------------------------------------------------------------

func TestEdge_NilPayload(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "nil-payload"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deliveryCh := make(chan ports.Delivery, 1)
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			deliveryCh <- d
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"null_payload", map[string]any{"subject": "test", "payload": nil}},
		{"no_payload_field", map[string]any{"subject": "test"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type httpResult struct{ rec *httptest.ResponseRecorder }
			resultCh := make(chan httpResult, 1)
			go func() {
				rec := postJSON(t, factory.Handler(),
					"/transport/http/receivers/nil-payload/messages",
					tc.body, nil)
				resultCh <- httpResult{rec: rec}
			}()

			select {
			case d := <-deliveryCh:
				env := d.Envelope()
				if env.Subject() != "test" {
					t.Fatalf("subject: got %q, want test", env.Subject())
				}
				_ = d.Ack(context.Background())
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for delivery")
			}

			select {
			case res := <-resultCh:
				if res.rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", res.rec.Code, res.rec.Body.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for HTTP response")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestEdge_RemoteRouteNoForwarderReturns502
// ---------------------------------------------------------------------------

func TestEdge_RemoteRouteNoForwarderReturns502(t *testing.T) {
	remotePeer := &persistence.PeerInfo{
		InstanceID: "remote-nofwd",
		Endpoints:  map[string]string{"http": "http://remote:9090"},
	}
	locator := &stubLocator{peer: remotePeer, local: false}

	// Factory with locator but NO forwarder.
	factory := transport.NewFactory(
		transport.WithRouteLocator(locator),
	)

	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "nofwd"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	if setter, ok := recv.(ports.RouteIDSetter); ok {
		setter.SetRouteID("route-nofwd")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			t.Error("emit should not be called when forwarder is nil and route is remote")
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	rec := postJSON(t, factory.Handler(), "/transport/http/receivers/nofwd/messages",
		map[string]any{"subject": "test", "payload": json.RawMessage(`{}`)}, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no forwarder configured") {
		t.Fatalf("expected 'no forwarder configured' in body, got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// TestEdge_SSEResponseHeaders
// ---------------------------------------------------------------------------

func TestEdge_SSEResponseHeaders(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-headers", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-headers/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("Content-Type: got %q, want text/event-stream", ct)
	}

	cc := resp.Header.Get("Cache-Control")
	if cc != "no-cache" {
		t.Fatalf("Cache-Control: got %q, want no-cache", cc)
	}

	xcto := resp.Header.Get("X-Content-Type-Options")
	if xcto != "nosniff" {
		t.Fatalf("X-Content-Type-Options: got %q, want nosniff", xcto)
	}
}

// ---------------------------------------------------------------------------
// TestEdge_SSESendContextCancelled
// ---------------------------------------------------------------------------

func TestEdge_SSESendContextCancelled(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-ctx", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "ctx-1",
		Subject: "test.ctx",
		Payload: []byte(`{}`),
	})

	err = sender.Send(ctx, ports.OutboundMessage{Envelope: env})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
