// Tests covering the subject/address separation contract for the
// HTTP/SSE adapter (T10):
//
//   - The HTTP POST ingress half is unchanged: body.Subject is the
//     logical Envelope.Subject and there is no transport-address
//     synthesis from URL/path.
//   - The SSE egress half is bound to a single logical identity (the
//     sender spec ID, optionally overridden by SetRouteID).
//     ports.OutboundMessage.Address must be empty or equal to that
//     identity. A nil envelope is rejected with shared.ErrInvalidPayload
//     and a mismatched address is rejected with shared.ErrInvalidTopic
//     before any marshal, fan-out, or metric emission. Per-message
//     dynamic SSE channel routing is explicitly out of scope.
//   - The logical Envelope.Subject flows verbatim end-to-end from HTTP
//     ingress through SSE egress.
package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingMetrics counts every emission so tests can assert that no
// transport metric is emitted on validation failures.
type recordingMetrics struct {
	mu     sync.Mutex
	timers int
	counts int
	gauges int
}

func (r *recordingMetrics) Counter(_ string, _ int64, _ ...shared.Tag) {
	r.mu.Lock()
	r.counts++
	r.mu.Unlock()
}

func (r *recordingMetrics) Gauge(_ string, _ float64, _ ...shared.Tag) {
	r.mu.Lock()
	r.gauges++
	r.mu.Unlock()
}

func (r *recordingMetrics) Histogram(_ string, _ float64, _ ...shared.Tag) {}

func (r *recordingMetrics) Timer(_ string, _ time.Duration, _ ...shared.Tag) {
	r.mu.Lock()
	r.timers++
	r.mu.Unlock()
}

func (r *recordingMetrics) Flush(_ context.Context) error { return nil }
func (r *recordingMetrics) Close(_ context.Context) error { return nil }

func (r *recordingMetrics) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Gauges are emitted only by ServeHTTP (client connect/disconnect),
	// never by Send, so they do not count toward Send-path emission.
	return r.timers + r.counts
}

// newSSESenderForTest constructs an SSESender bound to the given
// sender spec ID via the public factory and returns the concrete type
// alongside the recording metrics exporter.
func newSSESenderForTest(t *testing.T, id string) (*transport.SSESender, *recordingMetrics) {
	t.Helper()
	rec := &recordingMetrics{}
	factory := transport.NewFactory(transport.WithFactoryMetrics(rec))
	s, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     id,
		Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s.(*transport.SSESender), rec
}

// ---------------------------------------------------------------------------
// SSESender — Address validation matrix
// ---------------------------------------------------------------------------

// Verifies a nil envelope is rejected with shared.ErrInvalidPayload
// before any marshal, fan-out, or metric emission.
func TestSSESender_Send_NilEnvelope(t *testing.T) {
	sender, rec := newSSESenderForTest(t, "sse-nil")

	err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: nil})
	if err == nil {
		t.Fatal("Send must fail for nil envelope")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidPayload) {
		t.Fatalf("err = %v, want ErrInvalidPayload", err)
	}
	if sender.ClientCount() != 0 {
		t.Fatalf("expected 0 clients, got %d", sender.ClientCount())
	}
	if rec.total() != 0 {
		t.Fatalf("no metric must be emitted on nil envelope, got %d", rec.total())
	}
}

// Verifies a non-empty Address that does not match the configured
// identity is rejected with shared.ErrInvalidTopic, no event is
// broadcast, and no transport metric is emitted.
func TestSSESender_Send_RejectsMismatchedAddress(t *testing.T) {
	sender, rec := newSSESenderForTest(t, "sse-cfg")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte(`{}`)})
	err := sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: env,
		Address:  "other-route",
	})
	if err == nil {
		t.Fatal("Send must fail when Address mismatches the configured identity")
	}
	var be *shared.BridgeError
	if !errors.As(err, &be) || !errors.Is(be, shared.ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
	if rec.total() != 0 {
		t.Fatalf("no metric must be emitted on address mismatch, got %d emissions", rec.total())
	}
}

// Sanity guard: the address-mismatch error message includes both the
// requested address and the configured identity so on-call has a
// usable diagnostic without rerunning with trace logs.
func TestSSESender_Send_MismatchErrorMessageContainsBothAddresses(t *testing.T) {
	sender, _ := newSSESenderForTest(t, "sse-cfg")

	err := sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: &messaging.Envelope{ID: "e1", Payload: []byte(`{}`)},
		Address:  "other-route",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "other-route") || !strings.Contains(msg, "sse-cfg") {
		t.Fatalf("error %q must contain both requested and configured addresses", msg)
	}
}

// Verifies an empty Address means "use the configured identity": a
// connected client receives the broadcast event with Envelope.Subject
// preserved verbatim.
func TestSSESender_Send_EmptyAddressUsesConfiguredIdentity(t *testing.T) {
	sender, _ := newSSESenderForTest(t, "sse-empty")
	subject, body := runSSESendAndCapture(t, sender, ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "evt-empty",
			Subject: "user.signup",
			Payload: []byte(`{"user":"alice"}`),
		}),
	})
	if subject != "user.signup" {
		t.Fatalf("subject = %q, want user.signup; full body:\n%s", subject, body)
	}
}

// Verifies a non-empty Address equal to the configured identity is
// accepted and the event is broadcast with Envelope.Subject preserved
// verbatim.
func TestSSESender_Send_AcceptsMatchingAddress(t *testing.T) {
	sender, _ := newSSESenderForTest(t, "sse-match")
	subject, body := runSSESendAndCapture(t, sender, ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "evt-match",
			Subject: "user.signup",
			Payload: []byte(`{"user":"bob"}`),
		}),
		Address: "sse-match",
	})
	if subject != "user.signup" {
		t.Fatalf("subject = %q, want user.signup; full body:\n%s", subject, body)
	}
}

// Verifies SetRouteID overrides the spec ID as the configured
// identity for Address validation.
func TestSSESender_Send_SetRouteIDOverridesIdentity(t *testing.T) {
	sender, _ := newSSESenderForTest(t, "sse-spec-id")
	sender.SetRouteID("route-123")

	// A msg.Address equal to the now-overridden identity must succeed.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e1", Subject: "evt.x", Payload: []byte(`{}`)})
	if err := sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: env,
		Address:  "route-123",
	}); err != nil {
		t.Fatalf("Send with matching routeID address: %v", err)
	}

	// The original spec ID must now be rejected as a mismatch.
	err := sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: env,
		Address:  "sse-spec-id",
	})
	if err == nil {
		t.Fatal("Send with stale spec-id Address must be rejected after SetRouteID")
	}
	if !errors.Is(err, shared.ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP ingress → SSE egress: end-to-end subject preservation
// ---------------------------------------------------------------------------

// Verifies the logical subject submitted via HTTP POST ingress is
// delivered verbatim to SSE clients of a sender bound to the same
// route, with no transport-address synthesis from URL/path.
func TestHTTPIngressToSSEEgress_PreservesSubject(t *testing.T) {
	factory := transport.NewFactory()

	// Build receiver and sender, wire them through a shared route ID.
	const routeID = "rt-1"
	const senderID = "sse-e2e"

	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "http-ingress",
		Config: transport.Config{Mode: "http"},
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	recv.(*transport.Receiver).SetRouteID(routeID)

	sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     senderID,
		Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	sse := sender.(*transport.SSESender)

	// Run receiver in background so it accepts requests.
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	emitDone := make(chan *messaging.Envelope, 1)
	go func() {
		_ = recv.Run(rctx, func(_ context.Context, d ports.Delivery) error {
			env := d.Envelope()
			emitDone <- env
			_ = d.Ack(context.Background())
			// Forward the same envelope to SSE egress.
			_ = sse.Send(context.Background(), ports.OutboundMessage{Envelope: env})
			return nil
		})
	}()
	<-recv.(*transport.Receiver).Started()

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	// Connect an SSE client and wait for it to register.
	resp, err := http.Get(ts.URL + "/transport/http/senders/" + senderID + "/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from SSE endpoint, got %d", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for sse.ClientCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("SSE client did not register in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// POST an ingress request with a known subject.
	reqBody := []byte(`{"subject":"order.created","payload":{"id":42}}`)
	postResp, err := http.Post(ts.URL+"/transport/http/receivers/http-ingress/messages",
		"application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("ingress status = %d", postResp.StatusCode)
	}

	// Confirm the receiver saw the same logical subject.
	select {
	case env := <-emitDone:
		if env.Subject() != "order.created" {
			t.Fatalf("ingress envelope subject = %q, want order.created", env.Subject())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit did not fire")
	}

	// Read SSE event stream and assert the subject round-tripped.
	subject, body := readOneSSEEvent(t, resp, 2*time.Second)
	if subject != "order.created" {
		t.Fatalf("SSE subject = %q, want order.created; body:\n%s", subject, body)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// runSSESendAndCapture connects a single SSE client to the given
// sender, dispatches msg, reads one SSE event from the wire, and
// returns the decoded subject plus the raw event body (for test
// diagnostics).
func runSSESendAndCapture(t *testing.T, sender *transport.SSESender, msg ports.OutboundMessage) (string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/sse", sender)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/sse")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for sender.ClientCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("SSE client did not register in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	return readOneSSEEvent(t, resp, 2*time.Second)
}

// readOneSSEEvent reads SSE-formatted lines until a blank separator
// is seen and returns the subject extracted from the JSON data line
// alongside the joined event body for diagnostics.
func readOneSSEEvent(t *testing.T, resp *http.Response, timeout time.Duration) (string, string) {
	t.Helper()
	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	deadline := time.After(timeout)
	for {
		lineCh := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				lineCh <- scanner.Text()
				return
			}
			lineCh <- ""
		}()
		select {
		case line := <-lineCh:
			if line == "" && len(lines) > 0 {
				body := strings.Join(lines, "\n")
				return extractSubject(body), body
			}
			if line != "" {
				lines = append(lines, line)
			}
		case <-deadline:
			body := strings.Join(lines, "\n")
			t.Fatalf("timed out reading SSE event; got:\n%s", body)
			return "", body
		}
	}
}

// extractSubject finds the JSON "subject" field in the SSE data line
// without depending on a JSON decoder ordering.
func extractSubject(body string) string {
	const key = `"subject":"`
	idx := strings.Index(body, key)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(key):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}
