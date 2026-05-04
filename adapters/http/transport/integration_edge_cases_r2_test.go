package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// 1. TestEdgeR2_EmitReturnsError
// ---------------------------------------------------------------------------

func TestEdgeR2_EmitReturnsError(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "emit-err-r2"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			return errors.New("queue full")
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	rec := postJSON(t, factory.Handler(), "/transport/http/receivers/emit-err-r2/messages",
		map[string]any{
			"subject": "test.emit-err",
			"payload": json.RawMessage(`{"key":"val"}`),
		}, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "processing failed") {
		t.Fatalf("expected 'processing failed' in body, got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 2. TestEdgeR2_RequestTimeout504
// ---------------------------------------------------------------------------

func TestEdgeR2_RequestTimeout504(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "timeout-r2"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			return nil // don't Ack — handler blocks on del.done
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer reqCancel()

	data, _ := json.Marshal(map[string]any{
		"subject": "test.timeout",
		"payload": json.RawMessage(`{}`),
	})

	req := httptest.NewRequest("POST", "/transport/http/receivers/timeout-r2/messages", bytes.NewReader(data))
	req = req.WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	factory.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request timeout") {
		t.Fatalf("expected 'request timeout' in body, got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 3. TestEdgeR2_SSEAuthRequired
// ---------------------------------------------------------------------------

func TestEdgeR2_SSEAuthRequired(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:      "sse-auth-r2",
		Options: map[string]any{"mode": "sse", "api_key": "sse-secret"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	sseURL := ts.URL + "/transport/http/senders/sse-auth-r2/events"

	resp, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("GET without key: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API key, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", sseURL, nil)
	req.Header.Set("X-API-Key", "sse-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with key: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with correct API key, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 4. TestEdgeR2_SSEMaxClients
// ---------------------------------------------------------------------------

func TestEdgeR2_SSEMaxClients(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:      "sse-maxcli-r2",
		Options: map[string]any{"mode": "sse", "max_clients": 2},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	sseURL := ts.URL + "/transport/http/senders/sse-maxcli-r2/events"

	resp1, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("client 1: %v", err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("client 1: expected 200, got %d", resp1.StatusCode)
	}

	resp2, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("client 2: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("client 2: expected 200, got %d", resp2.StatusCode)
	}

	wait.Until(t, 2*time.Second, "2 SSE clients registered", func() bool {
		return sender.(*transport.SSESender).ClientCount() >= 2
	})

	resp3, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("client 3: %v", err)
	}
	defer func() { _ = resp3.Body.Close() }()

	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("client 3: expected 503, got %d", resp3.StatusCode)
	}
	body3, err := io.ReadAll(resp3.Body)
	if err != nil {
		t.Fatalf("read client 3 body: %v", err)
	}
	if !strings.Contains(string(body3), "connection limit reached") {
		t.Fatalf("expected 'connection limit reached' in response, got: %s", body3)
	}
}

// ---------------------------------------------------------------------------
// 5. TestEdgeR2_ForwarderClusterKey
// ---------------------------------------------------------------------------

func TestEdgeR2_ForwarderClusterKey(t *testing.T) {
	var capturedAPIKey string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAPIKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer remote.Close()

	fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second, "cluster-secret")

	peer := &domain.PeerInfo{
		InstanceID: "remote-key",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := &domain.Envelope{
		ID:      "fwd-key-1",
		Subject: "test.cluster-key",
		Payload: []byte(`{}`),
	}

	if err := fwd.Forward(context.Background(), peer, "route-key", env); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if capturedAPIKey != "cluster-secret" {
		t.Fatalf("expected X-API-Key=cluster-secret, got %q", capturedAPIKey)
	}
}

// ---------------------------------------------------------------------------
// 6. TestEdgeR2_UnsupportedSenderMode
// ---------------------------------------------------------------------------

func TestEdgeR2_UnsupportedSenderMode(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:      "ws-sender-r2",
		Options: map[string]any{"mode": "websocket"},
	}, nil)
	if err == nil {
		t.Fatal("expected error for unsupported sender mode, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported sender mode") {
		t.Fatalf("expected 'unsupported sender mode' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 7. TestEdgeR2_CaseInsensitiveHeaderStripping
// ---------------------------------------------------------------------------

func TestEdgeR2_CaseInsensitiveHeaderStripping(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "case-strip-r2"}, nil)
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
			"/transport/http/receivers/case-strip-r2/messages",
			map[string]any{
				"subject": "test.case-strip",
				"payload": json.RawMessage(`{}`),
				"headers": map[string]any{
					"X-Bridge.tenant-id": "evil",
					"X-BRIDGE.route-id":  "injected",
					"custom":             "keep",
				},
			}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	var env *domain.Envelope
	select {
	case d := <-deliveryCh:
		env = d.Envelope()
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	<-resultCh

	for k := range env.Headers {
		if strings.HasPrefix(strings.ToLower(k), "x-bridge.") {
			t.Fatalf("reserved header not stripped: %q", k)
		}
	}

	if v, ok := env.Headers["custom"]; !ok || v != "keep" {
		t.Fatalf("custom header missing or wrong: %v", v)
	}
}

// ---------------------------------------------------------------------------
// 8. TestEdgeR2_SSENoConnectionHeader
// ---------------------------------------------------------------------------

func TestEdgeR2_SSENoConnectionHeader(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:      "sse-conn-r2",
		Options: map[string]any{"mode": "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-conn-r2/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if conn := resp.Header.Get("Connection"); conn != "" {
		t.Fatalf("expected no Connection header, got %q", conn)
	}
}
