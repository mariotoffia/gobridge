package transport_test

import (
	"bufio"
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
// 1. TestEdge_ReceiverDoubleRunNoPanic
// ---------------------------------------------------------------------------

func TestEdge_ReceiverDoubleRunNoPanic(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "double-run"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	errCh1 := make(chan error, 1)
	go func() {
		errCh1 <- recv.Run(ctx1, func(_ context.Context, d ports.Delivery) error {
			return d.Ack(context.Background())
		})
	}()

	waitReceiverReady(t, recv, 2*time.Second)
	cancel1()

	select {
	case err := <-errCh1:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first Run: expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not return")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	errCh2 := make(chan error, 1)
	go func() {
		errCh2 <- recv.Run(ctx2, func(_ context.Context, d ports.Delivery) error {
			return d.Ack(context.Background())
		})
	}()

	waitReceiverReady(t, recv, 2*time.Second)
	cancel2()

	select {
	case err := <-errCh2:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second Run: expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Run did not return")
	}
}

// ---------------------------------------------------------------------------
// 2. TestEdge_SubjectRequired
// ---------------------------------------------------------------------------

func TestEdge_SubjectRequired(t *testing.T) {
	factory := transport.NewFactory()
	_, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "no-subject"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recv, _ := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "no-subj2"}, nil)
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			return d.Ack(context.Background())
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	rec := postJSON(t, factory.Handler(), "/transport/http/receivers/no-subj2/messages",
		map[string]any{"payload": json.RawMessage(`{}`)}, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "subject is required") {
		t.Fatalf("expected 'subject is required' in body, got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 3. TestEdge_ReservedHeadersStrippedAtIngress
// ---------------------------------------------------------------------------

func TestEdge_ReservedHeadersStrippedAtIngress(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "strip-hdrs"}, nil)
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

	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/strip-hdrs/messages",
			map[string]any{
				"subject": "hdr.strip",
				"payload": json.RawMessage(`{}`),
				"headers": map[string]any{
					"x-bridge.route-id":  "injected-route",
					"x-bridge.tenant-id": "injected-tenant",
					"x-custom-safe":      "keep-me",
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

	if _, ok := env.Headers["x-bridge.route-id"]; ok {
		t.Fatal("reserved header x-bridge.route-id was NOT stripped")
	}
	if _, ok := env.Headers["x-bridge.tenant-id"]; ok {
		t.Fatal("reserved header x-bridge.tenant-id was NOT stripped")
	}
	if v, ok := env.Headers["x-custom-safe"]; !ok || v != "keep-me" {
		t.Fatalf("non-reserved header x-custom-safe missing or wrong: %v", v)
	}
}

// ---------------------------------------------------------------------------
// 4. TestEdge_ForwarderPreservesExpiresAt
// ---------------------------------------------------------------------------

func TestEdge_ForwarderPreservesExpiresAt(t *testing.T) {
	var capturedBody []byte
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer remote.Close()
	fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	peer := &domain.PeerInfo{
		InstanceID: "remote-exp",
		Endpoints:  map[string]string{"http": remote.URL},
	}

	expiresAt := time.Now().Add(10 * time.Minute).Truncate(time.Second).UTC()
	env := &domain.Envelope{
		ID:        "exp-msg-1",
		Subject:   "order.expiring",
		Payload:   []byte(`{"id":"42"}`),
		ExpiresAt: expiresAt,
	}

	if err := fwd.Forward(context.Background(), peer, "route-exp", env); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}

	raw, ok := parsed["expires_at"]
	if !ok {
		t.Fatal("forwarded body missing expires_at field")
	}
	got, ok := raw.(string)
	if !ok {
		t.Fatalf("expires_at is not a string: %T", raw)
	}
	parsedTime, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("expires_at is not valid RFC3339: %v", err)
	}
	if !parsedTime.Equal(expiresAt) {
		t.Fatalf("expires_at mismatch: got %v, want %v", parsedTime, expiresAt)
	}
}

// ---------------------------------------------------------------------------
// 5. TestEdge_ForwarderMissingHTTPEndpoint
// ---------------------------------------------------------------------------

func TestEdge_ForwarderMissingHTTPEndpoint(t *testing.T) {
	fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	peer := &domain.PeerInfo{
		InstanceID: "grpc-only",
		Endpoints:  map[string]string{"grpc": "grpc://remote:50051"},
	}
	env := &domain.Envelope{
		ID:      "no-http-1",
		Subject: "test.no-http",
		Payload: []byte(`{}`),
	}

	err := fwd.Forward(context.Background(), peer, "route-nohttp", env)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, domain.ErrForwardFailed) {
		t.Fatalf("expected ErrForwardFailed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. TestEdge_ForwarderRemoteReturns500
// ---------------------------------------------------------------------------

func TestEdge_ForwarderRemoteReturns500(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()
	fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	peer := &domain.PeerInfo{
		InstanceID: "error-node",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := &domain.Envelope{
		ID:      "err-500",
		Subject: "test.error",
		Payload: []byte(`{}`),
	}

	err := fwd.Forward(context.Background(), peer, "route-500", env)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable (transient) for 500, got %v", err)
	}
	if !domain.IsRecoverableError(err) {
		t.Fatal("expected recoverable error for 500")
	}
}

// ---------------------------------------------------------------------------
// 7. TestEdge_SSEFieldSanitization
// ---------------------------------------------------------------------------

func TestEdge_SSEFieldSanitization(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-sanitize", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-sanitize/events")
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

	env := &domain.Envelope{
		ID:      "evil\ninjection",
		Subject: "sanitize.test",
		Payload: []byte(`{}`),
	}
	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(2 * time.Second)
	done := make(chan []string, 1)

	go func() {
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			lines = append(lines, line)
			if strings.HasPrefix(line, "data: ") {
				done <- lines
				return
			}
		}
		done <- lines
	}()

	var lines []string
	select {
	case lines = <-done:
	case <-deadline:
		t.Fatal("timeout reading SSE events")
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "id: ") {
			idVal := strings.TrimPrefix(line, "id: ")
			if strings.Contains(idVal, "\n") {
				t.Fatalf("SSE id field contains raw newline: %q", idVal)
			}
			if idVal != "evilinjection" {
				t.Fatalf("SSE id: got %q, want %q", idVal, "evilinjection")
			}
			return
		}
	}
	t.Fatal("no id: line found in SSE output")
}

// ---------------------------------------------------------------------------
// 8. TestEdge_LocatorErrorReturns502
// ---------------------------------------------------------------------------

func TestEdge_LocatorErrorReturns502(t *testing.T) {
	loc := &stubLocator{err: errors.New("lease service unavailable")}
	factory := transport.NewFactory(transport.WithRouteLocator(loc))
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "loc-err"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	setRouteID(t, recv, "route-loc-err")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			t.Error("emit should not be called on locator error")
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	rec := postJSON(t, factory.Handler(), "/transport/http/receivers/loc-err/messages",
		map[string]any{"subject": "test", "payload": json.RawMessage(`{}`)}, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "route location failed") {
		t.Fatalf("expected generic error, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 9. TestEdge_SendWithNoClients
// ---------------------------------------------------------------------------

func TestEdge_SendWithNoClients(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-empty", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := &domain.Envelope{
		ID:      "orphan-1",
		Subject: "no.listeners",
		Payload: []byte(`{}`),
	}
	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send with no clients should succeed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 10. TestEdge_ErrorResponsesAreGeneric
// ---------------------------------------------------------------------------

func TestEdge_ErrorResponsesAreGeneric(t *testing.T) {
	loc := &stubLocator{
		err: errors.New("DynamoDB connection refused: host=10.0.1.5:8000"),
	}
	factory := transport.NewFactory(transport.WithRouteLocator(loc))
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "generic-err"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	setRouteID(t, recv, "route-generic-err")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			t.Error("emit should not be called on locator error")
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	rec := postJSON(t, factory.Handler(), "/transport/http/receivers/generic-err/messages",
		map[string]any{"subject": "test", "payload": json.RawMessage(`{}`)}, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "DynamoDB") {
		t.Fatalf("response leaks internal detail 'DynamoDB': %s", body)
	}
	if strings.Contains(body, "10.0.1.5") {
		t.Fatalf("response leaks internal IP '10.0.1.5': %s", body)
	}
	if !strings.Contains(body, "route location failed") {
		t.Fatalf("expected generic message 'route location failed', got: %s", body)
	}
}
