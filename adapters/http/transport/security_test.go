package transport_test

// Adversarial / production-readiness tests for the HTTP transport
// security contracts hardened in the prod-ready remediation:
//
//   - X-Bridge-Forwarded spoofing is rejected: a bare (or wrongly
//     tokened) marker no longer forces local processing on a non-owner
//     node; only a request proving the shared internal forward token is
//     trusted as already-forwarded.
//   - Trailing data after the single JSON body is rejected instead of
//     silently ignored.
//   - An over-cap body maps to 413 (not 400).
//   - 401 responses advertise a WWW-Authenticate Bearer challenge.
//   - An inline api_key shorter than the enforced minimum is rejected at
//     decode time.
//   - INTERNAL-ONLY reserved headers are stripped before an SSE frame
//     reaches a (possibly external) subscriber.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// startReceiver runs recv (built from spec) on factory with an
// auto-acking emit so a locally-processed request returns 200 without an
// external Ack goroutine. It returns a counter of emit invocations so a
// test can distinguish local processing from a cluster forward.
func startReceiver(t *testing.T, factory *transport.Factory, spec ports.ReceiverSpec, routeID string) *atomic.Int64 {
	t.Helper()
	recv, err := factory.NewReceiver(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	if routeID != "" {
		setRouteID(t, recv, routeID)
	}
	var emitCount atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = recv.Run(ctx, func(_ context.Context, d ports.Delivery) error {
			emitCount.Add(1)
			_ = d.Ack(context.Background())
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)
	return &emitCount
}

// postRaw posts an arbitrary (possibly malformed) body so tests can
// exercise the decoder directly — postJSON marshals a map and cannot
// produce trailing tokens.
func postRaw(t *testing.T, h http.Handler, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// X-Bridge-Forwarded spoofing
// ---------------------------------------------------------------------------

// The X-Bridge-Forwarded marker must satisfy two orthogonal contracts at
// once, and neither may depend on a forward token being configured:
//
//   - Spoof-safe: a client-controlled marker must never force local
//     processing on a node that does not own the route.
//   - Loop-safe: a marker on a route this node does not own must never be
//     re-forwarded (an A->B->A storm under routing disagreement).
//
// The node therefore refuses (508) an *untrusted* marker on a non-owned
// route — it neither processes it locally (spoof defence) nor re-forwards
// it (loop defence). A marker is trusted only when it proves the shared
// internal forward token, in which case it is processed locally to
// terminate the chain. An untrusted marker on a route this node *does*
// own is processed normally, so an untokened cluster still delivers on
// the first hop.
func TestReceiver_ForwardedHeaderSpoofing(t *testing.T) {
	const serverToken = "internal-forward-token-secret"

	cases := []struct {
		name           string
		sendMarker     bool
		configureToken bool
		sendToken      string
		locatorLocal   bool
		wantCode       int
		wantForwards   int
		wantEmits      int64
	}{
		// Baseline: no marker on a remote route forwards to the owner — the
		// loop guard must not block a legitimate first hop.
		{"no_marker_remote_forwards", false, false, "", false, http.StatusOK, 1, 0},
		// Untrusted marker on a route we do NOT own: refuse (508), neither
		// process nor re-forward — independent of token configuration.
		{"untrusted_no_token_remote_refused", true, false, "", false, http.StatusLoopDetected, 0, 0},
		{"untrusted_token_cfg_none_sent_remote_refused", true, true, "", false, http.StatusLoopDetected, 0, 0},
		{"untrusted_wrong_token_remote_refused", true, true, "wrong-token", false, http.StatusLoopDetected, 0, 0},
		// Untrusted marker on a route we DO own: process locally (untokened
		// cluster first-hop delivery must not require a token).
		{"untrusted_no_token_local_owner_processed", true, false, "", true, http.StatusOK, 0, 1},
		// Trusted peer marker: process locally to terminate the chain even
		// though the locator says the route is remote.
		{"trusted_correct_token_remote_processed", true, true, serverToken, false, http.StatusOK, 0, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var peer *persistence.PeerInfo
			if !tc.locatorLocal {
				peer = &persistence.PeerInfo{
					InstanceID: "owner-node",
					Endpoints:  map[string]string{"http": "http://owner:9090"},
				}
			}
			locator := &stubLocator{peer: peer, local: tc.locatorLocal}
			fwd := &recordingForwarder{}

			opts := []transport.FactoryOption{
				transport.WithRouteLocator(locator),
				transport.WithMessageForwarder(fwd),
			}
			if tc.configureToken {
				opts = append(opts, transport.WithForwardToken(serverToken))
			}
			factory := transport.NewFactory(opts...)
			emitCount := startReceiver(t, factory,
				ports.ReceiverSpec{ID: "spoof"}, "route-X")

			headers := map[string]string{}
			if tc.sendMarker {
				headers["X-Bridge-Forwarded"] = "true"
			}
			if tc.sendToken != "" {
				headers["X-Bridge-Forward-Token"] = tc.sendToken
			}

			rec := postJSON(t, factory.Handler(),
				"/transport/http/receivers/spoof/messages",
				map[string]any{
					"subject": "orders.created",
					"payload": json.RawMessage(`{"x":1}`),
				}, headers)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if got := len(fwd.getCalls()); got != tc.wantForwards {
				t.Fatalf("want %d forward(s), got %d", tc.wantForwards, got)
			}
			if got := emitCount.Load(); got != tc.wantEmits {
				t.Fatalf("want %d local emit(s), got %d", tc.wantEmits, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Trailing JSON rejection
// ---------------------------------------------------------------------------

// A well-formed ingress body carries exactly one JSON value. A second
// value, trailing garbage, or a trailing array must be rejected rather
// than silently ignored (request smuggling / ambiguous-parse defence).
// Insignificant trailing whitespace is still accepted.
func TestReceiver_TrailingJSONRejected(t *testing.T) {
	factory := transport.NewFactory()
	startReceiver(t, factory, ports.ReceiverSpec{ID: "trailing"}, "")
	const url = "/transport/http/receivers/trailing/messages"

	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"two_objects", `{"subject":"a","payload":{}}{"subject":"b"}`, http.StatusBadRequest},
		{"object_then_garbage", `{"subject":"a","payload":{}} garbage`, http.StatusBadRequest},
		{"object_then_array", `{"subject":"a","payload":{}}[1,2]`, http.StatusBadRequest},
		{"single_object_trailing_ws", "{\"subject\":\"a\",\"payload\":{}}\n  ", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postRaw(t, factory.Handler(), url, tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("body %q: expected %d, got %d: %s", tc.body, tc.wantCode, rec.Code, rec.Body.String())
			}
			if tc.wantCode == http.StatusBadRequest &&
				!strings.Contains(rec.Body.String(), "trailing data") {
				t.Fatalf("expected a trailing-data error message, got: %s", rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 413 mapping
// ---------------------------------------------------------------------------

// A body exceeding the configured cap must map to 413 (the stdlib
// http.MaxBytesError), not the generic 400 used for malformed JSON.
func TestReceiver_BodyTooLarge_Returns413(t *testing.T) {
	factory := transport.NewFactory()
	startReceiver(t, factory,
		ports.ReceiverSpec{ID: "toobig", Config: transport.Config{MaxBodySize: 32}}, "")

	body := `{"subject":"x","payload":"` + strings.Repeat("y", 256) + `"}`
	rec := postRaw(t, factory.Handler(), "/transport/http/receivers/toobig/messages", body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// WWW-Authenticate challenge on 401
// ---------------------------------------------------------------------------

// Both the receiver POST and the SSE GET endpoint must return a
// RFC 7235 WWW-Authenticate Bearer challenge with their 401 so standard
// HTTP clients learn how to authenticate.
func TestReceiver_Unauthorized_SetsWWWAuthenticate(t *testing.T) {
	factory := transport.NewFactory()
	startReceiver(t, factory, ports.ReceiverSpec{
		ID:     "secure",
		Config: transport.Config{APIKey: shared.NewSecret("super-secret-key-123")},
	}, "")

	rec := postJSON(t, factory.Handler(),
		"/transport/http/receivers/secure/messages",
		map[string]any{"subject": "s", "payload": json.RawMessage(`{}`)}, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="gobridge"` {
		t.Fatalf("expected Bearer challenge, got %q", got)
	}
}

func TestSSESender_Unauthorized_SetsWWWAuthenticate(t *testing.T) {
	factory := transport.NewFactory()
	s, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     "sse-secure",
		Config: transport.Config{Mode: "sse", APIKey: shared.NewSecret("sse-secret-key-1234")},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	sender := s.(*transport.SSESender)

	req := httptest.NewRequest("GET", "/transport/http/senders/sse-secure/events", nil)
	rec := httptest.NewRecorder()
	sender.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="gobridge"` {
		t.Fatalf("expected Bearer challenge, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// API key minimum length
// ---------------------------------------------------------------------------

// An inline api_key, when set, must meet the enforced minimum length so
// a too-short key cannot silently weaken endpoint protection. Empty
// (unset) is allowed.
func TestConfig_Validate_APIKeyMinimum(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty_unset_ok", "", false},
		{"too_short", "short", true},
		{"boundary_15", strings.Repeat("a", 15), true},
		{"boundary_16", strings.Repeat("a", 16), false},
		{"long_ok", "this-is-a-sufficiently-long-key", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := transport.Config{APIKey: shared.NewSecret(tc.key)}.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// A rejected under-floor inline api_key must produce an error that names
// the 16-character minimum as the cause, so an operator whose
// previously accepted short key now fails understands why instead of
// seeing an opaque validation failure.
func TestConfig_Validate_APIKeyFloorErrorNamesMinimum(t *testing.T) {
	err := transport.Config{APIKey: shared.NewSecret("short")}.Validate()
	if err == nil {
		t.Fatal("expected an error for a 5-character inline api_key")
	}
	msg := err.Error()
	for _, want := range []string{"api_key", "too short", "minimum is 16"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("floor error must name the 16-char minimum as the cause; missing %q in: %s", want, msg)
		}
	}
}

// rawJSON is a minimal ports.RawConfig that decodes via a JSON round
// trip, exercising the registry decode path (Register -> decode ->
// Config.Validate) end-to-end.
type rawJSON map[string]any

func (r rawJSON) Decode(target any) error {
	b, err := json.Marshal(map[string]any(r))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

// The registry decode path must reject a too-short inline api_key and
// accept a compliant one.
func TestRegister_RejectsShortInlineAPIKey(t *testing.T) {
	reg := ports.NewRegistry()
	if err := transport.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := reg.Decode(transport.Kind, rawJSON{"api_key": "short"}); err == nil {
		t.Fatal("expected decode to reject a too-short inline api_key")
	}

	cfg, err := reg.Decode(transport.Kind, rawJSON{"api_key": strings.Repeat("k", 20)})
	if err != nil {
		t.Fatalf("expected a compliant api_key to decode, got: %v", err)
	}
	if cfg == nil || cfg.Kind() != transport.Kind {
		t.Fatalf("expected a %q PluginConfig, got %#v", transport.Kind, cfg)
	}
}

// ---------------------------------------------------------------------------
// SSE INTERNAL-ONLY header strip on egress
// ---------------------------------------------------------------------------

// An SSE frame handed to a (possibly external) subscriber must not leak
// INTERNAL-ONLY reserved headers (the bridge's own dispatch
// bookkeeping). BRIDGE-TO-BRIDGE propagated headers and application
// headers pass through — a sender cannot tell a peer bridge from an
// external client, so stripping only the internal-only set is the safe
// default.
func TestSSESender_StripsInternalOnlyHeadersOnEgress(t *testing.T) {
	factory := transport.NewFactory()
	s, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID:     "sse-hdr",
		Config: transport.Config{Mode: "sse"},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	sender := s.(*transport.SSESender)

	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/transport/http/senders/sse-hdr/events")
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from SSE endpoint, got %d", resp.StatusCode)
	}

	wait.Until(t, 2*time.Second, "SSE client registered", func() bool {
		return sender.ClientCount() >= 1
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "evt-h",
		Subject: "s.topic",
		Payload: []byte(`{"k":1}`),
	})
	// SetHeader writes directly (no reserved strip) so the test can
	// install reserved headers the way the runtime stamping path would.
	env.SetHeader(messaging.HeaderRouteID, "internal-route-secret") // INTERNAL-ONLY -> strip
	env.SetHeader(messaging.HeaderCorrelationID, "corr-keep")       // BRIDGE-TO-BRIDGE -> keep
	env.SetHeader("x-app-trace", "app-keep")                        // application -> keep

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	data := readSSEDataJSON(t, resp.Body, 2*time.Second)
	var ev struct {
		ID      string         `json:"id"`
		Headers map[string]any `json:"headers"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal SSE data %q: %v", data, err)
	}

	if _, leaked := ev.Headers[messaging.HeaderRouteID]; leaked {
		t.Fatalf("INTERNAL-ONLY header %q leaked to SSE client: %v",
			messaging.HeaderRouteID, ev.Headers)
	}
	if got := ev.Headers[messaging.HeaderCorrelationID]; got != "corr-keep" {
		t.Fatalf("BRIDGE-TO-BRIDGE header must pass through, got %v (all: %v)", got, ev.Headers)
	}
	if got := ev.Headers["x-app-trace"]; got != "app-keep" {
		t.Fatalf("application header must pass through, got %v (all: %v)", got, ev.Headers)
	}
}

// readSSEDataJSON scans an SSE stream for the first "data: " line and
// returns its JSON payload. It uses a per-line goroutine feeding a
// buffered channel (mirroring the existing broadcast test) so it never
// blocks past the deadline and leaks no goroutine after returning.
func readSSEDataJSON(t *testing.T, body io.Reader, deadline time.Duration) []byte {
	t.Helper()
	scanner := bufio.NewScanner(body)
	timeout := time.After(deadline)
	for {
		lineCh := make(chan struct {
			s  string
			ok bool
		}, 1)
		go func() {
			if scanner.Scan() {
				lineCh <- struct {
					s  string
					ok bool
				}{scanner.Text(), true}
				return
			}
			lineCh <- struct {
				s  string
				ok bool
			}{"", false}
		}()

		select {
		case r := <-lineCh:
			if !r.ok {
				t.Fatal("SSE stream ended before a data line")
				return nil
			}
			if strings.HasPrefix(r.s, "data: ") {
				return []byte(strings.TrimPrefix(r.s, "data: "))
			}
		case <-timeout:
			t.Fatal("timed out waiting for SSE data line")
			return nil
		}
	}
}
