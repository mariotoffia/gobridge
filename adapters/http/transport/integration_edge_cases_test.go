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
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"

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

	var env *messaging.Envelope
	select {
	case d := <-deliveryCh:
		env = d.Envelope()
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	<-resultCh

	if _, ok := env.Headers()["x-bridge.route-id"]; ok {
		t.Fatal("reserved header x-bridge.route-id was NOT stripped")
	}
	if _, ok := env.Headers()["x-bridge.tenant-id"]; ok {
		t.Fatal("reserved header x-bridge.tenant-id was NOT stripped")
	}
	if v, ok := env.Headers()["x-custom-safe"]; !ok || v != "keep-me" {
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
	peer := &persistence.PeerInfo{
		InstanceID: "remote-exp",
		Endpoints:  map[string]string{"http": remote.URL},
	}

	expiresAt := time.Now().Add(10 * time.Minute).Truncate(time.Second).UTC()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        "exp-msg-1",
		Subject:   "order.expiring",
		Payload:   []byte(`{"id":"42"}`),
		ExpiresAt: expiresAt,
	})

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
	peer := &persistence.PeerInfo{
		InstanceID: "grpc-only",
		Endpoints:  map[string]string{"grpc": "grpc://remote:50051"},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "no-http-1",
		Subject: "test.no-http",
		Payload: []byte(`{}`),
	})

	err := fwd.Forward(context.Background(), peer, "route-nohttp", env)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, shared.ErrForwardFailed) {
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
	peer := &persistence.PeerInfo{
		InstanceID: "error-node",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "err-500",
		Subject: "test.error",
		Payload: []byte(`{}`),
	})

	err := fwd.Forward(context.Background(), peer, "route-500", env)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable (transient) for 500, got %v", err)
	}
	if !shared.IsRecoverableError(err) {
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

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "evil\ninjection",
		Subject: "sanitize.test",
		Payload: []byte(`{}`),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
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
		// No "id:" framing field may be emitted at all: it would both
		// reopen the newline-injection vector this test guards and imply
		// Last-Event-ID resumability the sender does not provide.
		if strings.HasPrefix(line, "id: ") {
			t.Fatalf("unexpected SSE id: field emitted: %q", line)
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			// The hostile envelope ID must arrive JSON-escaped inside the
			// data payload — never as a raw newline that could forge an
			// extra SSE field or frame boundary.
			var evt struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
				t.Fatalf("SSE data payload is not valid JSON (framing injection?): %v", err)
			}
			if evt.ID != "evil\ninjection" {
				t.Fatalf("envelope id mangled in data payload: got %q", evt.ID)
			}
			return
		}
	}
	t.Fatal("no data: line found in SSE output")
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

// Safe-by-default: a broadcast with no connected subscribers is a
// zero delivery, so Send returns a TRANSIENT (Unavailable-class) error and
// the route runner does NOT ack the source. Accepting the loss requires the
// explicit at_most_once_accept_loss opt-in (asserted separately below).
func TestEdge_SendWithNoClients(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-empty", Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "orphan-1",
		Subject: "no.listeners",
		Payload: []byte(`{}`),
	})
	err = sender.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if err == nil {
		t.Fatal("Send with no clients must return a transient error by default, got nil")
	}
	if !shared.IsRecoverableError(err) {
		t.Fatalf("zero-delivery error must be transient/recoverable, got %v", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("zero-delivery error must be Unavailable-class, got %v", err)
	}
}

// Explicit opt-out: with at_most_once_accept_loss:true the same no-client
// broadcast is accepted (nil) so the source is acked despite reaching nobody.
func TestEdge_SendWithNoClients_AcceptLossOptOut(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "sse-empty-loss", Config: transport.Config{Mode: "sse", AtMostOnceAcceptLoss: true},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "orphan-2",
		Subject: "no.listeners",
		Payload: []byte(`{}`),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send with no clients under accept-loss must succeed, got: %v", err)
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

// TestEdge_IdempotencyKeyHeaderStampedAtIngress confirms the receiver
// maps the standard, NON-reserved Idempotency-Key request header (plus
// the X-Dedup-Id / X-Ordering-Key companions) onto EnvelopeInput's
// first-class fields, so an external HTTP producer can supply
// idempotency without using the anti-spoofed x-bridge.* namespace — the
// delivered envelope carries the reserved headers stamped on the trusted
// side of the ingress strip.
func TestEdge_IdempotencyKeyHeaderStampedAtIngress(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "idem-hdr"}, nil)
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
		rec := postJSON(t, factory.Handler(), "/transport/http/receivers/idem-hdr/messages",
			map[string]any{
				"subject": "order.created",
				"payload": json.RawMessage(`{}`),
			}, map[string]string{
				"Idempotency-Key": "abc",
				"X-Dedup-Id":      "dedup-9",
				"X-Ordering-Key":  "order-9",
			})
		resultCh <- httpResult{rec: rec}
	}()

	var env *messaging.Envelope
	select {
	case d := <-deliveryCh:
		env = d.Envelope()
		_ = d.Ack(context.Background())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}
	<-resultCh

	if v, _ := env.Header(messaging.HeaderIdempotencyKey); v != "abc" {
		t.Fatalf("HeaderIdempotencyKey = %v, want abc", v)
	}
	if v, _ := env.Header(messaging.HeaderDeduplicationID); v != "dedup-9" {
		t.Fatalf("HeaderDeduplicationID = %v, want dedup-9", v)
	}
	if v, _ := env.Header(messaging.HeaderOrderingKey); v != "order-9" {
		t.Fatalf("HeaderOrderingKey = %v, want order-9", v)
	}
}
