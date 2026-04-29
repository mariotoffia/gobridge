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
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
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
	factoryB := transport.NewFactory()
	recvB, err := factoryB.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-cluster"}, nil)
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

	// Wait for the HTTP receiver's Run to land its emit callback before
	// the server starts serving. ServeHTTP blocks on this readiness
	// channel internally, so requests arriving before Run has started
	// would observe "receiver not ready".
	waitReceiverReady(t, recvB, 2*time.Second)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — locator points to Bridge B, uses HTTPForwarder.
	locA := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: map[string]string{"http": serverB.URL}},
		local: false,
	}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-cluster"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, "route-cluster")
	var emitCalled atomic.Bool
	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			emitCalled.Store(true)
			return nil
		})
	}()
	waitReceiverReady(t, recvA, 2*time.Second)

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
	if len(envs) != 1 {
		t.Fatalf("expected exactly 1 envelope, got %d", len(envs))
	}
	if envs[0].Subject != "orders.created" {
		t.Fatalf("subject: got %q, want orders.created", envs[0].Subject)
	}
	if string(envs[0].Payload) != `{"order":"123"}` {
		t.Fatalf("payload mismatch: %s", envs[0].Payload)
	}

	cancel()
	wgA.Wait()
	if emitCalled.Load() {
		t.Error("Bridge A emit must not be called when forwarding")
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
	factoryB := transport.NewFactory()
	senderB, err := factoryB.NewSender(ctx, ports.SenderSpec{
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
	factoryA := transport.NewFactory(transport.WithRouteLocator(locA))
	senderA, err := factoryA.NewSender(ctx, ports.SenderSpec{
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
		wait.Until(t, 2*time.Second, "SSE client registered on B", func() bool {
			return senderB.(*transport.SSESender).ClientCount() >= 1
		})

		env := &domain.Envelope{
			ID: "sse-evt-1", Subject: "user.created",
			Payload: []byte(`{"name":"alice"}`),
		}
		if err := senderB.Send(ctx, env); err != nil {
			t.Fatalf("Send: %v", err)
		}

		lineCh := make(chan string, 64)
		var scanWg sync.WaitGroup
		scanWg.Add(1)
		go func() {
			defer scanWg.Done()
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				lineCh <- scanner.Text()
			}
			close(lineCh)
		}()

		var lines []string
		deadline := time.After(2 * time.Second)
	loop:
		for {
			select {
			case l, ok := <-lineCh:
				if !ok {
					break loop
				}
				if l == "" && len(lines) > 0 {
					break loop
				}
				lines = append(lines, l)
			case <-deadline:
				break loop
			}
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "id: sse-evt-1") {
			t.Fatalf("missing SSE id, got:\n%s", joined)
		}
		if !strings.Contains(joined, `"subject":"user.created"`) {
			t.Fatalf("missing subject in SSE data, got:\n%s", joined)
		}
		_ = resp.Body.Close()
		scanWg.Wait()
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
	factoryB := transport.NewFactory(
		transport.WithRouteLocator(locB),
		transport.WithMessageForwarder(fwdB),
	)
	recvB, err := factoryB.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-loop"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver B: %v", err)
	}
	setRouteID(t, recvB, "route-loop")

	var delivered []*domain.Envelope
	var mu sync.Mutex
	var wgB sync.WaitGroup
	wgB.Add(1)
	go func() {
		defer wgB.Done()
		_ = recvB.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			mu.Lock()
			delivered = append(delivered, d.Envelope())
			mu.Unlock()
			return d.Ack(context.Background())
		})
	}()
	waitReceiverReady(t, recvB, 2*time.Second)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — locator says remote(B), forwards via HTTPForwarder.
	locA := &stubLocator{peer: peerB, local: false}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-loop"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, "route-loop")
	var emitCalledA atomic.Bool
	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			emitCalledA.Store(true)
			return nil
		})
	}()
	waitReceiverReady(t, recvA, 2*time.Second)

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
	if len(delivered) != 1 {
		t.Fatalf("expected exactly 1 delivered envelope, got %d", len(delivered))
	}
	if delivered[0].Subject != "loop.test" {
		t.Fatalf("subject: got %q, want loop.test", delivered[0].Subject)
	}
	mu.Unlock()

	cancel()
	wgA.Wait()
	wgB.Wait()
	if emitCalledA.Load() {
		t.Error("Bridge A emit must not be called")
	}
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
	factory := transport.NewFactory(
		transport.WithRouteLocator(loc),
		transport.WithMessageForwarder(fwd),
	)
	recv, err := factory.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-dead"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	setRouteID(t, recv, "route-dead")
	var emitCalled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = recv.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			emitCalled.Store(true)
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

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

	cancel()
	wg.Wait()
	if emitCalled.Load() {
		t.Error("emit must not be called for dead peer")
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
	factoryB := transport.NewFactory()
	recvB, err := factoryB.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-preserve"}, nil)
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
	waitReceiverReady(t, recvB, 2*time.Second)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — forwards to Bridge B.
	locA := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: map[string]string{"http": serverB.URL}},
		local: false,
	}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, ports.ReceiverSpec{ID: "route-preserve"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, "route-preserve")
	var emitCalled atomic.Bool
	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			emitCalled.Store(true)
			return nil
		})
	}()
	waitReceiverReady(t, recvA, 2*time.Second)

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

	envs := senderB.getSent()
	if len(envs) != 1 {
		t.Fatalf("expected exactly 1 envelope, got %d", len(envs))
	}
	got := envs[0]
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

	cancel()
	wgA.Wait()
	if emitCalled.Load() {
		t.Error("Bridge A emit must not be called")
	}
}

// ---------------------------------------------------------------------------
// 3.6 — Forwarding with divergent receiver ID vs route ID.
// ---------------------------------------------------------------------------

// Exposes a bug where the forwarder constructs the remote URL using the route
// ID instead of the receiver ID. When the two differ, Bridge B never receives
// the forwarded message because the factory mounts the handler at
// /receivers/{receiverID}/messages but the forwarder targets
// /receivers/{routeID}/messages — resulting in a 404 on the remote peer.
func TestIntegration_Cluster_ForwardDivergentReceiverID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		receiverID = "recv-orders"
		routeID    = "route-alpha"
	)

	// Bridge B — receiver registered under receiverID, runtime route under routeID.
	factoryB := transport.NewFactory()
	recvB, err := factoryB.NewReceiver(ctx, ports.ReceiverSpec{ID: receiverID}, nil)
	if err != nil {
		t.Fatalf("NewReceiver B: %v", err)
	}
	senderB := &fakeSender{}

	rtB := runtime.New(runtime.WithInstanceID("bridge-b"))
	if err := rtB.AddRoute(directHoldRouteConfig(routeID, nil), recvB, senderB, nil, nil); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctx); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	defer func() { _ = rtB.Stop(context.Background()) }()
	waitReceiverReady(t, recvB, 2*time.Second)

	serverB := httptest.NewServer(factoryB.Handler())
	defer serverB.Close()

	// Bridge A — locator says remote(B), forwards via HTTPForwarder.
	locA := &stubLocator{
		peer:  &domain.PeerInfo{InstanceID: "bridge-b", Endpoints: map[string]string{"http": serverB.URL}},
		local: false,
	}
	fwdA := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	factoryA := transport.NewFactory(
		transport.WithRouteLocator(locA),
		transport.WithMessageForwarder(fwdA),
	)
	recvA, err := factoryA.NewReceiver(ctx, ports.ReceiverSpec{ID: receiverID}, nil)
	if err != nil {
		t.Fatalf("NewReceiver A: %v", err)
	}
	setRouteID(t, recvA, routeID)
	var emitCalled atomic.Bool
	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		_ = recvA.Run(ctx, func(_ context.Context, _ ports.Delivery) error {
			emitCalled.Store(true)
			return nil
		})
	}()
	waitReceiverReady(t, recvA, 2*time.Second)

	serverA := httptest.NewServer(factoryA.Handler())
	defer serverA.Close()

	resp := httpPostJSON(t, serverA.URL+"/transport/http/receivers/"+receiverID+"/messages", map[string]any{
		"subject": "orders.created",
		"payload": json.RawMessage(`{"order":"divergent-1"}`),
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	waitFor(t, 2*time.Second, "Bridge B sender receives 1 message", func() bool {
		return len(senderB.getSent()) > 0
	})

	envs := senderB.getSent()
	if len(envs) != 1 {
		t.Fatalf("expected exactly 1 envelope, got %d", len(envs))
	}
	if envs[0].Subject != "orders.created" {
		t.Fatalf("subject: got %q, want orders.created", envs[0].Subject)
	}
	if string(envs[0].Payload) != `{"order":"divergent-1"}` {
		t.Fatalf("payload mismatch: %s", envs[0].Payload)
	}

	cancel()
	wgA.Wait()
	if emitCalled.Load() {
		t.Error("Bridge A emit must not be called when forwarding")
	}
}
