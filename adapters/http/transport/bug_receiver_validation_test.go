package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// BUG M1: HTTP Receiver no Content-Type validation
// ---------------------------------------------------------------------------

func TestBugReceiver_RejectsNonJSONContentType(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "ct-test"}, nil)
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

	body, _ := json.Marshal(map[string]any{
		"subject": "test.ct",
		"payload": json.RawMessage(`{}`),
	})

	// POST with Content-Type: text/plain should be rejected with 415.
	req := httptest.NewRequest("POST", "/transport/http/receivers/ct-test/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	factory.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for text/plain Content-Type, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Content-Type") {
		t.Fatalf("expected error message mentioning Content-Type, got: %s", rec.Body.String())
	}
}

func TestBugReceiver_AcceptsJSONContentType(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "ct-json"}, nil)
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
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/ct-json/messages", map[string]any{
			"subject": "test.ct.json",
			"payload": json.RawMessage(`{}`),
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
			t.Fatalf("expected 200 for application/json, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}
}

func TestBugReceiver_AcceptsMissingContentType(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "ct-empty"}, nil)
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

	body, _ := json.Marshal(map[string]any{
		"subject": "test.ct.empty",
		"payload": json.RawMessage(`{}`),
	})

	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		// POST without Content-Type header.
		req := httptest.NewRequest("POST", "/transport/http/receivers/ct-empty/messages", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		factory.Handler().ServeHTTP(rec, req)
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
			t.Fatalf("expected 200 for missing Content-Type, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}
}

func TestBugReceiver_AcceptsJSONWithCharset(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "ct-charset"}, nil)
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

	body, _ := json.Marshal(map[string]any{
		"subject": "test.ct.charset",
		"payload": json.RawMessage(`{}`),
	})

	type httpResult struct {
		rec *httptest.ResponseRecorder
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		// POST with Content-Type: application/json; charset=utf-8 should work.
		req := httptest.NewRequest("POST", "/transport/http/receivers/ct-charset/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		rec := httptest.NewRecorder()
		factory.Handler().ServeHTTP(rec, req)
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
			t.Fatalf("expected 200 for application/json; charset=utf-8, got %d: %s", res.rec.Code, res.rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP response")
	}
}

// ---------------------------------------------------------------------------
// BUG M2: HTTP Receiver empty Envelope.ID
// ---------------------------------------------------------------------------

func TestBugReceiver_GeneratesIDWhenEmpty(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "id-gen"}, nil)
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
		// POST without an "id" field.
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/id-gen/messages", map[string]any{
			"subject": "test.id.gen",
			"payload": json.RawMessage(`{"v":1}`),
		}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	var gotID string
	select {
	case d := <-deliveryCh:
		gotID = d.Envelope().ID
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

	if gotID == "" {
		t.Fatal("expected non-empty envelope ID when no id provided in request, got empty")
	}
	if !strings.HasPrefix(gotID, "http-") {
		t.Fatalf("expected generated ID to start with 'http-', got %q", gotID)
	}
}

func TestBugReceiver_PreservesExplicitID(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "id-keep"}, nil)
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
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/id-keep/messages", map[string]any{
			"subject": "test.id.keep",
			"payload": json.RawMessage(`{}`),
			"id":      "my-explicit-id-42",
		}, nil)
		resultCh <- httpResult{rec: rec}
	}()

	var gotID string
	select {
	case d := <-deliveryCh:
		gotID = d.Envelope().ID
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

	if gotID != "my-explicit-id-42" {
		t.Fatalf("expected explicit ID 'my-explicit-id-42', got %q", gotID)
	}
}

func TestBugReceiver_GeneratedIDsAreUnique(t *testing.T) {
	factory := transport.NewBridgeFactory()
	recv, err := factory.NewReceiver(context.Background(), config.ReceiverDef{ID: "id-unique"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deliveryCh := make(chan ports.Delivery, 10)

	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			deliveryCh <- d
			return nil
		})
	}()

	waitReceiverReady(t, recv, 2*time.Second)

	const count = 5
	for i := 0; i < count; i++ {
		go func() {
			postJSON(t, factory.Handler(), "/transport/http/receivers/id-unique/messages", map[string]any{
				"subject": "test.unique",
				"payload": json.RawMessage(`{}`),
			}, nil)
		}()
	}

	seen := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		select {
		case d := <-deliveryCh:
			id := d.Envelope().ID
			if id == "" {
				t.Fatal("got empty ID")
			}
			if seen[id] {
				t.Fatalf("duplicate generated ID: %q", id)
			}
			seen[id] = true
			_ = d.Ack(context.Background())
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for delivery %d/%d", i+1, count)
		}
	}
}
