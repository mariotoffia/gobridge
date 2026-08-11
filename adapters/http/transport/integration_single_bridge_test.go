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
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type fakeSender struct {
	mu   sync.Mutex
	sent []*messaging.Envelope
}

func (s *fakeSender) Send(_ context.Context, msg ports.OutboundMessage) error {
	env := msg.Envelope
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, env.Clone())
	return nil
}

func (s *fakeSender) getSent() []*messaging.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*messaging.Envelope, len(s.sent))
	copy(cp, s.sent)
	return cp
}

func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond) // OTHER: internal polling interval in test helper
	}
	t.Fatalf("timeout waiting for %s", desc)
}

// waitReceiverReady blocks until recv signals it is live, or fails the test
// after timeout. No-ops for receivers that don't implement the signaler.
func waitReceiverReady(t *testing.T, recv any, timeout time.Duration) {
	t.Helper()
	s, ok := recv.(ports.ReceiverStartedSignaler)
	if !ok {
		return
	}
	select {
	case <-s.Started():
	case <-time.After(timeout):
		t.Fatalf("receiver did not become ready within %s", timeout)
	}
}

type filterProcessor struct {
	dropFn func(*messaging.Envelope) bool
}

func (p *filterProcessor) Name() string { return "test-filter" }

func (p *filterProcessor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	if p.dropFn(env) {
		return shared.ErrMessageFiltered
	}
	return next(ctx, env)
}

func directHoldRouteConfig(id string, procs []ports.Processor) runtime.RouteConfig {
	return runtime.RouteConfig{
		ID: id,
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			DispatchMode:       routing.DispatchSingle,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
		},
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
		Processors: procs,
	}
}

// ---------------------------------------------------------------------------
// 1.1 TestIntegration_HTTPPost_RuntimePipeline_FakeSender
// ---------------------------------------------------------------------------

// Validates that an HTTP POST flows through the full runtime pipeline and
// arrives at the configured sender with correct subject, payload, and headers.
func TestIntegration_HTTPPost_RuntimePipeline_FakeSender(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "pipe"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	sender := &fakeSender{}
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	if err := rt.AddRoute(directHoldRouteConfig("pipe-route", nil), recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"subject": "order.created",
		"payload": json.RawMessage(`{"id":"42"}`),
	})
	resp, err := http.Post(ts.URL+"/transport/http/receivers/pipe/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	waitFor(t, 2*time.Second, "sender receives 1 message", func() bool {
		return len(sender.getSent()) >= 1
	})
	env := sender.getSent()[0]
	if env.Subject() != "order.created" {
		t.Fatalf("subject: got %q, want order.created", env.Subject())
	}
	if !strings.Contains(string(env.Payload()), `"id":"42"`) {
		t.Fatalf("payload mismatch: %s", env.Payload())
	}
	if _, ok := env.Headers()[messaging.HeaderRouteID]; !ok {
		t.Fatal("missing x-bridge.route-id header")
	}
	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; !ok {
		t.Fatal("missing x-bridge.correlation-id header")
	}
}

// ---------------------------------------------------------------------------
// 1.2 TestIntegration_HTTPPost_FilterDrop_NoSend
// ---------------------------------------------------------------------------

// Validates that a processor can filter messages and that filtered messages
// are acked without forwarding to the sender.
func TestIntegration_HTTPPost_FilterDrop_NoSend(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "filter"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	sender := &fakeSender{}
	filter := &filterProcessor{dropFn: func(env *messaging.Envelope) bool {
		return strings.HasPrefix(env.Subject(), "spam.")
	}}
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	if err := rt.AddRoute(directHoldRouteConfig("filter-route", []ports.Processor{filter}), recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	post := func(subject string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"subject": subject, "payload": json.RawMessage(`{}`)})
		resp, err := http.Post(ts.URL+"/transport/http/receivers/filter/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", subject, err)
		}
		return resp
	}
	resp := post("spam.phish")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered POST: expected 200, got %d", resp.StatusCode)
	}
	resp = post("order.created")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid POST: expected 200, got %d", resp.StatusCode)
	}
	waitFor(t, 2*time.Second, "sender has exactly 1 message (spam filtered)", func() bool {
		return len(sender.getSent()) == 1
	})
	env := sender.getSent()[0]
	if env.Subject() != "order.created" {
		t.Fatalf("subject: got %q, want order.created", env.Subject())
	}
}

// ---------------------------------------------------------------------------
// 1.3 TestIntegration_SSEClient_ReceivesMultipleEvents
// ---------------------------------------------------------------------------

// Validates that an SSE sender broadcasts multiple envelopes to a connected
// HTTP client and that disconnection does not cause sender errors.
func TestIntegration_SSEClient_ReceivesMultipleEvents(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-multi", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-multi/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE connect: expected 200, got %d", resp.StatusCode)
	}

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.(*transport.SSESender).ClientCount() >= 1
	})

	subjects := []string{"evt.one", "evt.two", "evt.three"}
	for i, subj := range subjects {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: subj, Subject: subj, Payload: []byte(`{}`)})
		if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	var collected []string
	deadline := time.After(3 * time.Second)
	done := make(chan struct{})

	go func() {
		defer close(done)
		// No SSE "id:" field is emitted (at-most-once, see doc.go);
		// collect envelope IDs from the JSON data payload instead.
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var evt struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
					continue
				}
				collected = append(collected, evt.ID)
				if len(collected) >= 3 {
					return
				}
			}
		}
	}()

	select {
	case <-done:
	case <-deadline:
		t.Fatal("timeout reading SSE events")
	}

	for _, subj := range subjects {
		found := false
		for _, id := range collected {
			if id == subj {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing SSE event id %q in %v", subj, collected)
		}
	}

}

// ---------------------------------------------------------------------------
// 1.4 TestIntegration_HTTPPost_APIKeyAuth
// ---------------------------------------------------------------------------

// Validates API key authentication for both X-API-Key and Bearer header styles,
// and rejects requests with missing or wrong keys.
func TestIntegration_HTTPPost_APIKeyAuth(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"valid_x_api_key", map[string]string{"X-API-Key": "secret-abcdefghij"}, http.StatusOK},
		{"valid_bearer", map[string]string{"Authorization": "Bearer secret-abcdefghij"}, http.StatusOK},
		{"missing_key", nil, http.StatusUnauthorized},
		{"wrong_key", map[string]string{"X-API-Key": "wrong"}, http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := transport.NewFactory()
			recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
				ID: "auth-" + tc.name, Config: transport.Config{APIKey: shared.NewSecret("secret-abcdefghij")},
			}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
					_ = d.Ack(context.Background())
					return nil
				})
			}()
			waitReceiverReady(t, recv, 2*time.Second)

			rec := postJSON(t, factory.Handler(), "/transport/http/receivers/auth-"+tc.name+"/messages",
				map[string]any{"subject": "test", "payload": json.RawMessage(`{}`)}, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("expected %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 1.5 TestIntegration_HTTPPost_BodyTooLarge
// ---------------------------------------------------------------------------

// Validates that requests exceeding max_body_size are rejected with 413.
func TestIntegration_HTTPPost_BodyTooLarge(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "big", Config: transport.Config{MaxBodySize: 256},
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			_ = d.Ack(context.Background())
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	bigPayload := strings.Repeat("x", 1024)
	body, _ := json.Marshal(map[string]any{"subject": "big", "payload": bigPayload})
	req := httptest.NewRequest("POST", "/transport/http/receivers/big/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	factory.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 1.6 TestIntegration_HTTPPost_InvalidJSON
// ---------------------------------------------------------------------------

// Validates that malformed request bodies are rejected with 400.
func TestIntegration_HTTPPost_InvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated_json", `{"subject":"x"`},
		{"non_json", `not json at all`},
		{"empty_body", ``},
	}

	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "badjson"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			_ = d.Ack(context.Background())
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/transport/http/receivers/badjson/messages",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			factory.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 1.7 TestIntegration_HTTPPost_HeaderProcessing
// ---------------------------------------------------------------------------

// Verifies that the runtime overwrites injected x-bridge.route-id, preserves
// custom headers, and auto-injects correlation-id.
func TestIntegration_HTTPPost_HeaderProcessing(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "hdrs"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	sender := &fakeSender{}
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	if err := rt.AddRoute(directHoldRouteConfig("hdr-route", nil), recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"subject": "hdr.test",
		"payload": json.RawMessage(`{}`),
		"headers": map[string]any{
			"x-bridge.route-id": "should-be-overwritten",
			"custom-header":     "keep-me",
		},
	})
	resp, err := http.Post(ts.URL+"/transport/http/receivers/hdrs/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()

	waitFor(t, 2*time.Second, "sender receives 1 message", func() bool {
		return len(sender.getSent()) >= 1
	})

	env := sender.getSent()[0]

	routeID, ok := messaging.GetHeaderString(env.Headers(), messaging.HeaderRouteID)
	if !ok || routeID != "hdr-route" {
		t.Fatalf("route-id: got %q, want hdr-route", routeID)
	}

	custom, ok := env.Headers()["custom-header"]
	if !ok || custom != "keep-me" {
		t.Fatalf("custom-header: got %v, want keep-me", custom)
	}

	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; !ok {
		t.Fatal("missing auto-injected correlation-id")
	}
}

// ---------------------------------------------------------------------------
// 1.8 TestIntegration_HTTPPost_ReceiverNotReady
// ---------------------------------------------------------------------------

// Verifies that posting to a receiver before Run is called returns 503
// IMMEDIATELY: readiness is a non-blocking check, so the handler
// must return without any client-side cancellation. The request carries a
// background context with NO timeout — without the fix ServeHTTP would
// block on readiness until the (never-cancelled) context is done and the
// bounded wait below would fail.
func TestIntegration_HTTPPost_ReceiverNotReady(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "notready"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"subject": "test", "payload": json.RawMessage(`{}`)})
	req := httptest.NewRequest("POST", "/transport/http/receivers/notready/messages",
		bytes.NewReader(body)).WithContext(context.Background())
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		factory.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	// A block here is the regression: readiness lag pinning the
	// handler goroutine until the client gives up.
	wait.RequireClosed(t, done, 2*time.Second)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}
