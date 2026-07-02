package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// Mock helpers
// ---------------------------------------------------------------------------

type stubLocator struct {
	peer  *persistence.PeerInfo
	local bool
	err   error
}

func (s *stubLocator) Locate(_ context.Context, _ string) (*persistence.PeerInfo, bool, error) {
	return s.peer, s.local, s.err
}

type recordingForwarder struct {
	mu       sync.Mutex
	calls    []forwardCall
	returnFn func() error
}

type forwardCall struct {
	Peer       *persistence.PeerInfo
	ReceiverID string
	Env        *messaging.Envelope
}

func (f *recordingForwarder) Forward(_ context.Context, peer *persistence.PeerInfo, receiverID string, env *messaging.Envelope) error {
	f.mu.Lock()
	f.calls = append(f.calls, forwardCall{Peer: peer, ReceiverID: receiverID, Env: env})
	f.mu.Unlock()
	if f.returnFn != nil {
		return f.returnFn()
	}
	return nil
}

func (f *recordingForwarder) getCalls() []forwardCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]forwardCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// postJSON sends a POST request with a JSON ingress body.
func postJSON(t *testing.T, handler http.Handler, url string, body map[string]any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// 1. TestHTTPDelivery_AckSignalsCompletion
// ---------------------------------------------------------------------------

func TestHTTPDelivery_AckSignalsCompletion(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "ack-test"}, nil)
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

	// ServeHTTP blocks until Ack/Retry, so POST in a goroutine.
	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/ack-test/messages", map[string]any{
			"subject": "test.ack",
			"payload": json.RawMessage(`{"ok":true}`),
		}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	select {
	case d := <-deliveryCh:
		if err := d.Ack(context.Background()); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	select {
	case res := <-resultCh:
		if res.rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(res.rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp["status"] != "accepted" {
			t.Fatalf("expected status=accepted, got %q", resp["status"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}
}

// ---------------------------------------------------------------------------
// 2. TestHTTPDelivery_RetrySignalsError
// ---------------------------------------------------------------------------

func TestHTTPDelivery_RetrySignalsError(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "retry-test"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	retryErr := errors.New("processing failed")
	deliveryCh := make(chan ports.Delivery, 1)

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			deliveryCh <- d
			return nil
		})
	}()

	waitReceiverReady(t, recv, 2*time.Second)

	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/retry-test/messages", map[string]any{
			"subject": "test.retry",
			"payload": json.RawMessage(`{}`),
		}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	select {
	case d := <-deliveryCh:
		if err := d.Retry(context.Background(), 0, retryErr); err != nil {
			t.Fatalf("Retry: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	select {
	case res := <-resultCh:
		if res.rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
		if !strings.Contains(res.rec.Body.String(), "processing failed") {
			t.Fatalf("expected error message in body, got: %s", res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}
}

// ---------------------------------------------------------------------------
// 3. TestHTTPDelivery_ExtendReturnsNotSupported
// ---------------------------------------------------------------------------

func TestHTTPDelivery_ExtendReturnsNotSupported(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "extend-test"}, nil)
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

	go func() {
		data, _ := json.Marshal(map[string]any{
			"subject": "test.extend",
			"payload": json.RawMessage(`{}`),
		})
		req := httptest.NewRequest("POST", "/transport/http/receivers/extend-test/messages", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		factory.Handler().ServeHTTP(rec, req)
	}()

	select {
	case d := <-deliveryCh:
		err := d.Extend(context.Background(), time.Now().Add(time.Minute))
		if !errors.Is(err, shared.ErrNotSupported) {
			t.Fatalf("expected ErrNotSupported, got %v", err)
		}
		// Ack so the HTTP handler can finish.
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

// ---------------------------------------------------------------------------
// 4. TestReceiver_LocalProcessing
// ---------------------------------------------------------------------------

func TestReceiver_LocalProcessing(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "local"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gotEnvelope *messaging.Envelope
	deliveryCh := make(chan ports.Delivery, 1)

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			gotEnvelope = d.Envelope()
			deliveryCh <- d
			return nil
		})
	}()

	waitReceiverReady(t, recv, 2*time.Second)

	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/local/messages", map[string]any{
			"subject": "orders.created",
			"payload": json.RawMessage(`{"order_id":"123"}`),
			"id":      "msg-001",
		}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	select {
	case d := <-deliveryCh:
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

	if gotEnvelope == nil {
		t.Fatal("emit was never called")
	}
	if gotEnvelope.Subject() != "orders.created" {
		t.Fatalf("expected subject orders.created, got %q", gotEnvelope.Subject())
	}
	if gotEnvelope.ID() != "msg-001" {
		t.Fatalf("expected ID msg-001, got %q", gotEnvelope.ID())
	}
}

// ---------------------------------------------------------------------------
// 5. TestReceiver_ClusterForward
// ---------------------------------------------------------------------------

func TestReceiver_ClusterForward(t *testing.T) {
	remotePeer := &persistence.PeerInfo{
		InstanceID: "remote-1",
		Endpoints:  map[string]string{"http": "http://remote:9090"},
	}
	locator := &stubLocator{peer: remotePeer, local: false}
	fwd := &recordingForwarder{}

	factory := transport.NewFactory(
		transport.WithRouteLocator(locator),
		transport.WithMessageForwarder(fwd),
	)

	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "cluster"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	// The receiver needs a routeID for cluster logic to kick in.
	if setter, ok := recv.(ports.RouteIDSetter); ok {
		setter.SetRouteID("route-A")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			t.Error("emit should not be called when forwarding")
			return nil
		})
	}()

	waitReceiverReady(t, recv, 2*time.Second)

	rec := postJSON(t, factory.Handler(), "/transport/http/receivers/cluster/messages", map[string]any{
		"subject": "test.forward",
		"payload": json.RawMessage(`{"x":1}`),
	}, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Fatalf("expected status=accepted, got %q", resp["status"])
	}

	calls := fwd.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 forward call, got %d", len(calls))
	}
	if calls[0].ReceiverID != "cluster" {
		t.Fatalf("expected receiver ID cluster (used for URL path), got %q", calls[0].ReceiverID)
	}
}

// ---------------------------------------------------------------------------
// 6. TestReceiver_ForwardedRequestNotReforwarded
// ---------------------------------------------------------------------------

// A request that is genuinely from a peer bridge — proven by the
// internal forward token — is trusted as already-forwarded and processed
// locally instead of being re-forwarded (loop prevention). A bare
// X-Bridge-Forwarded marker WITHOUT the token is covered by the
// spoofing tests in prodready_security_test.go.
func TestReceiver_ForwardedRequestNotReforwarded(t *testing.T) {
	const forwardToken = "peer-forward-token-secret-1"
	remotePeer := &persistence.PeerInfo{
		InstanceID: "remote-1",
		Endpoints:  map[string]string{"http": "http://remote:9090"},
	}
	locator := &stubLocator{peer: remotePeer, local: false}
	fwd := &recordingForwarder{}

	factory := transport.NewFactory(
		transport.WithRouteLocator(locator),
		transport.WithMessageForwarder(fwd),
		transport.WithForwardToken(forwardToken),
	)

	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "noreforward"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	if setter, ok := recv.(ports.RouteIDSetter); ok {
		setter.SetRouteID("route-B")
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

	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/noreforward/messages", map[string]any{
			"subject": "test.forwarded",
			"payload": json.RawMessage(`{}`),
		}, map[string]string{
			"X-Bridge-Forwarded":     "true",
			"X-Bridge-Forward-Token": forwardToken,
		})
		resultCh <- httpResult{rec: rec}
	}()

	select {
	case d := <-deliveryCh:
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local delivery")
	}

	select {
	case res := <-resultCh:
		if res.rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}

	calls := fwd.getCalls()
	if len(calls) != 0 {
		t.Fatalf("expected 0 forward calls (already forwarded), got %d", len(calls))
	}
}

// ---------------------------------------------------------------------------
// 7. TestSSESender_BroadcastToClients
// ---------------------------------------------------------------------------

func TestSSESender_BroadcastToClients(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     "sse-broadcast",
		Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	// Connect an SSE client.
	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-broadcast/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from SSE endpoint, got %d", resp.StatusCode)
	}

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.(*transport.SSESender).ClientCount() >= 1
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "evt-1",
		Subject: "user.signup",
		Payload: []byte(`{"user":"alice"}`),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Read the SSE event from the client connection.
	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	deadline := time.After(2 * time.Second)
	done := false

	for !done {
		lineCh := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
			} else {
				lineCh <- ""
			}
		}()

		select {
		case line := <-lineCh:
			if line == "" && len(lines) > 0 {
				done = true
			} else {
				lines = append(lines, line)
			}
		case <-deadline:
			done = true
		}
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "id: evt-1") {
		t.Fatalf("expected SSE event with id evt-1, got:\n%s", joined)
	}
	if !strings.Contains(joined, "event: message") {
		t.Fatalf("expected SSE event type message, got:\n%s", joined)
	}
	if !strings.Contains(joined, `"subject":"user.signup"`) {
		t.Fatalf("expected subject in SSE data, got:\n%s", joined)
	}
}

// ---------------------------------------------------------------------------
// 8. TestSSESender_RedirectWhenRemote
// ---------------------------------------------------------------------------

func TestSSESender_RedirectWhenRemote(t *testing.T) {
	remotePeer := &persistence.PeerInfo{
		InstanceID: "remote-sse",
		Endpoints:  map[string]string{"http": "http://remote:8080"},
	}
	locator := &stubLocator{peer: remotePeer, local: false}

	factory := transport.NewFactory(
		transport.WithRouteLocator(locator),
	)

	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     "sse-redirect",
		Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	if setter, ok := sender.(ports.RouteIDSetter); ok {
		setter.SetRouteID("route-C")
	}

	// Use httptest.NewServer so we get a real request through the mux,
	// but disable redirect following to capture the 307.
	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(ts.URL + "/transport/http/senders/sse-redirect/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307, got %d", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	expected := "http://remote:8080/transport/http/senders/sse-redirect/events"
	if loc != expected {
		t.Fatalf("expected Location %q, got %q", expected, loc)
	}
}

// ---------------------------------------------------------------------------
// 9. TestHTTPForwarder_ForwardSuccess
// ---------------------------------------------------------------------------

func TestHTTPForwarder_ForwardSuccess(t *testing.T) {
	var receivedBody []byte
	var receivedForwarded string

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedForwarded = r.Header.Get("X-Bridge-Forwarded")
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer remote.Close()
	fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)

	peer := &persistence.PeerInfo{
		InstanceID: "remote-node",
		Endpoints:  map[string]string{"http": remote.URL},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "fwd-msg-1",
		Subject: "orders.shipped",
		Payload: []byte(`{"order":"456"}`),
	})

	if err := fwd.Forward(context.Background(), peer, "route-X", env); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if receivedForwarded != "true" {
		t.Fatalf("expected X-Bridge-Forwarded=true, got %q", receivedForwarded)
	}

	var reqBody map[string]json.RawMessage
	if err := json.Unmarshal(receivedBody, &reqBody); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	var subject string
	_ = json.Unmarshal(reqBody["subject"], &subject)
	if subject != "orders.shipped" {
		t.Fatalf("expected subject orders.shipped, got %q", subject)
	}
}

// ---------------------------------------------------------------------------
// 10. TestFactory_CreatesReceiversAndSenders
// ---------------------------------------------------------------------------

func TestFactory_CreatesReceiversAndSenders(t *testing.T) {
	factory := transport.NewFactory()

	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "recv-1",
		Config: transport.Config{},
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	if recv == nil {
		t.Fatal("NewReceiver returned nil")
	}

	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     "sender-1",
		Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if sender == nil {
		t.Fatal("NewSender returned nil")
	}

	caps := factory.Capabilities()
	found := false
	for _, c := range caps {
		if c == ports.CapHTTPEndpoint {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected CapHTTPEndpoint in capabilities, got %v", caps)
	}

	if factory.PathPrefix() != "/transport/http" {
		t.Fatalf("expected /transport/http, got %q", factory.PathPrefix())
	}

	if factory.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}
