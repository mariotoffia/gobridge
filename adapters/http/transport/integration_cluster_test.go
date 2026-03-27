package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// Helpers (fakeSender, waitFor, directHoldRouteConfig live in
// integration_single_bridge_test.go — same package, shared automatically)
// ---------------------------------------------------------------------------

func setRouteID(t *testing.T, v any, id string) {
	t.Helper()
	if s, ok := v.(interface{ SetRouteID(string) }); ok {
		s.SetRouteID(id)
	} else {
		t.Fatal("value does not support SetRouteID")
	}
}

func httpPostJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// 3.1 — Bridge A forwards a POST to Bridge B via HTTPForwarder.
// ---------------------------------------------------------------------------

// Validates that Bridge A forwards HTTP POST to Bridge B via HTTPForwarder when the route locator says remote.
func TestIntegration_Cluster_ForwardToBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bridge B — no locator, processes locally through a real runtime.
	factoryB := transport.NewBridgeFactory()
	recvB, err := factoryB.NewReceiver(ctx, config.ReceiverDef{ID: "route-cluster"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver B: %v", err)
	}
	senderB := &fakeSender{}

	rtB := runtime.New(runtime.WithInstanceID("bridge-b"))
	if err := rtB.AddRoute(directHoldRouteConfig("route-cluster", nil), recvB, senderB, nil, nil); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctx); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	defer func() { _ = rtB.Stop(context.Background()) }()
	time.Sleep(50 * time.Millisecond)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — locator points to Bridge B, uses HTTPForwarder.
	locA := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: map[string]string{"http": serverB.URL}},
		local: false,
	}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewBridgeFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, config.ReceiverDef{ID: "route-cluster"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, "route-cluster")
	go func() {
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			t.Error("Bridge A emit must not be called when forwarding")
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	serverA := httptest.NewServer(factoryA.Handler())
	defer serverA.Close()

	resp := httpPostJSON(t, serverA.URL+"/transport/http/receivers/route-cluster/messages", map[string]any{
		"subject": "orders.created",
		"payload": json.RawMessage(`{"order":"123"}`),
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "accepted" {
		t.Fatalf("expected status=accepted, got %q", body["status"])
	}

	waitFor(t, 2*time.Second, "Bridge B sender receives 1 message", func() bool {
		return len(senderB.getSent()) > 0
	})

	envs := senderB.getSent()
	if envs[0].Subject != "orders.created" {
		t.Fatalf("subject: got %q, want orders.created", envs[0].Subject)
	}
	if string(envs[0].Payload) != `{"order":"123"}` {
		t.Fatalf("payload mismatch: %s", envs[0].Payload)
	}
}

// ---------------------------------------------------------------------------
// 3.2 — SSE client hitting Bridge A gets 307 redirect to Bridge B.
// ---------------------------------------------------------------------------

// Validates that SSE client connecting to Bridge A gets a 307 redirect to Bridge B when the route is remote.
func TestIntegration_Cluster_SSERedirect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bridge B — local SSE sender.
	factoryB := transport.NewBridgeFactory()
	senderB, err := factoryB.NewSender(ctx, config.SenderDef{
		ID: "sse-cluster", Options: map[string]any{"mode": "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender B: %v", err)
	}
	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — locator says remote → redirect.
	locA := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: map[string]string{"http": serverB.URL}},
		local: false,
	}
	factoryA := transport.NewBridgeFactory(transport.WithRouteLocator(locA))
	senderA, err := factoryA.NewSender(ctx, config.SenderDef{
		ID: "sse-cluster", Options: map[string]any{"mode": "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender A: %v", err)
	}
	setRouteID(t, senderA, "route-sse")
	serverA := httptest.NewServer(factoryA.Handler())
	defer serverA.Close()

	t.Run("redirect_307", func(t *testing.T) {
		noFollow := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := noFollow.Get(serverA.URL + "/transport/http/senders/sse-cluster/events")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("expected 307, got %d", resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		want := serverB.URL + "/transport/http/senders/sse-cluster/events"
		if loc != want {
			t.Fatalf("Location: got %q, want %q", loc, want)
		}
	})

	t.Run("follow_and_receive", func(t *testing.T) {
		resp, err := http.Get(serverA.URL + "/transport/http/senders/sse-cluster/events")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 after redirect, got %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)

		env := &domain.Envelope{
			ID: "sse-evt-1", Subject: "user.created",
			Payload: []byte(`{"name":"alice"}`),
		}
		if err := senderB.Send(ctx, env); err != nil {
			t.Fatalf("Send: %v", err)
		}

		scanner := bufio.NewScanner(resp.Body)
		var lines []string
		deadline := time.After(2 * time.Second)
		done := false
		for !done {
			ch := make(chan string, 1)
			go func() {
				if scanner.Scan() {
					ch <- scanner.Text()
				} else {
					ch <- ""
				}
			}()
			select {
			case l := <-ch:
				if l == "" && len(lines) > 0 {
					done = true
				} else {
					lines = append(lines, l)
				}
			case <-deadline:
				done = true
			}
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "id: sse-evt-1") {
			t.Fatalf("missing SSE id, got:\n%s", joined)
		}
		if !strings.Contains(joined, `"subject":"user.created"`) {
			t.Fatalf("missing subject in SSE data, got:\n%s", joined)
		}
	})
}

// ---------------------------------------------------------------------------
// 3.3 — X-Bridge-Forwarded prevents re-forwarding (loop prevention).
// ---------------------------------------------------------------------------

// Verifies that X-Bridge-Forwarded header prevents infinite forwarding loops between bridges.
func TestIntegration_Cluster_ForwardLoopPrevention(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerA := &domain.PeerInfo{InstanceID: "bridge-a", Endpoints: make(map[string]string)}
	peerB := &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: make(map[string]string)}

	// Bridge B — locator says remote(A), but forwarded flag bypasses that.
	locB := &stubLocator{peer: peerA, local: false}
	fwdB := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryB := transport.NewBridgeFactory(
		transport.WithRouteLocator(locB),
		transport.WithMessageForwarder(fwdB),
	)
	recvB, err := factoryB.NewReceiver(ctx, config.ReceiverDef{ID: "route-loop"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver B: %v", err)
	}
	setRouteID(t, recvB, "route-loop")

	var delivered []*domain.Envelope
	var mu sync.Mutex
	go func() {
		_ = recvB.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			mu.Lock()
			delivered = append(delivered, d.Envelope())
			mu.Unlock()
			return d.Ack(context.Background())
		})
	}()
	time.Sleep(50 * time.Millisecond)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — locator says remote(B), forwards via HTTPForwarder.
	locA := &stubLocator{peer: peerB, local: false}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewBridgeFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, config.ReceiverDef{ID: "route-loop"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, "route-loop")
	go func() {
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			t.Error("Bridge A emit must not be called")
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	serverA := httptest.NewServer(factoryA.Handler())
	defer serverA.Close()

	// Wire peer URLs now that both servers are up.
	peerA.Endpoints["http"] = serverA.URL
	peerB.Endpoints["http"] = serverB.URL

	resp := httpPostJSON(t, serverA.URL+"/transport/http/receivers/route-loop/messages", map[string]any{
		"subject": "loop.test",
		"payload": json.RawMessage(`{"key":"val"}`),
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	waitFor(t, 2*time.Second, "Bridge B processes locally", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delivered) > 0
	})

	mu.Lock()
	if delivered[0].Subject != "loop.test" {
		t.Fatalf("subject: got %q, want loop.test", delivered[0].Subject)
	}
	mu.Unlock()
}

// ---------------------------------------------------------------------------
// 3.4 — Forwarding to an unreachable peer returns 502.
// ---------------------------------------------------------------------------

// Verifies that forwarding to an unavailable peer returns 502 Bad Gateway.
func TestIntegration_Cluster_ForwardToDeadPeer(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	loc := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "dead", Endpoints: map[string]string{"http": deadURL}},
		local: false,
	}
	fwd := transport.NewHTTPForwarder("/transport/http", 2*time.Second)
	factory := transport.NewBridgeFactory(
		transport.WithRouteLocator(loc),
		transport.WithMessageForwarder(fwd),
	)
	recv, err := factory.NewReceiver(ctx, config.ReceiverDef{ID: "route-dead"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	setRouteID(t, recv, "route-dead")
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			t.Error("emit must not be called for dead peer")
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	server := httptest.NewServer(factory.Handler())
	defer server.Close()

	resp := httpPostJSON(t, server.URL+"/transport/http/receivers/route-dead/messages", map[string]any{
		"subject": "dead.test",
		"payload": json.RawMessage(`{}`),
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 3.5 — Forwarding preserves the full envelope including custom headers.
// ---------------------------------------------------------------------------

// Validates that rich envelope data (ID, subject, payload, custom headers) survives the forward round-trip intact.
func TestIntegration_Cluster_ForwardPreservesEnvelope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bridge B — processes locally through a real runtime.
	factoryB := transport.NewBridgeFactory()
	recvB, err := factoryB.NewReceiver(ctx, config.ReceiverDef{ID: "route-preserve"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver B: %v", err)
	}
	senderB := &fakeSender{}

	rtB := runtime.New(runtime.WithInstanceID("bridge-b"))
	if err := rtB.AddRoute(directHoldRouteConfig("route-preserve", nil), recvB, senderB, nil, nil); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctx); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	defer func() { _ = rtB.Stop(context.Background()) }()
	time.Sleep(50 * time.Millisecond)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — forwards to Bridge B.
	locA := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: map[string]string{"http": serverB.URL}},
		local: false,
	}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewBridgeFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, config.ReceiverDef{ID: "route-preserve"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, "route-preserve")
	go func() {
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			t.Error("Bridge A emit must not be called")
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	serverA := httptest.NewServer(factoryA.Handler())
	defer serverA.Close()

	resp := httpPostJSON(t, serverA.URL+"/transport/http/receivers/route-preserve/messages", map[string]any{
		"id":      "env-rich-001",
		"subject": "billing.invoice.created",
		"payload": json.RawMessage(`{"invoice":{"id":"inv-42","items":[{"sku":"A","qty":2}]}}`),
		"headers": map[string]any{
			"x-tenant":   "acme",
			"x-priority": "high",
		},
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	waitFor(t, 2*time.Second, "Bridge B sender receives 1 message", func() bool {
		return len(senderB.getSent()) > 0
	})

	got := senderB.getSent()[0]
	if got.ID != "env-rich-001" {
		t.Fatalf("ID: got %q, want env-rich-001", got.ID)
	}
	if got.Subject != "billing.invoice.created" {
		t.Fatalf("subject: got %q, want billing.invoice.created", got.Subject)
	}
	wantPayload := `{"invoice":{"id":"inv-42","items":[{"sku":"A","qty":2}]}}`
	if string(got.Payload) != wantPayload {
		t.Fatalf("payload:\n got %s\nwant %s", got.Payload, wantPayload)
	}
	if got.Headers["x-tenant"] != "acme" {
		t.Fatalf("x-tenant: got %v, want acme", got.Headers["x-tenant"])
	}
	if got.Headers["x-priority"] != "high" {
		t.Fatalf("x-priority: got %v, want high", got.Headers["x-priority"])
	}
}
