package transport_test

// Deterministic tests for audit chunk, findings 5 and 9 (HTTP cluster
// forwarder):
//
//   - Finding 5: a forward is a POST with a body; the client must NOT
//     follow a 3xx (following turns 301/302/303 into a bodyless GET,
//     silently dropping the body). The redirect is surfaced as its own
//     response and classified as a PERMANENT forward failure.
//   - Finding 9: the response body must be drained FULLY before Close so
//     the keep-alive connection is reusable; a large peer body must not
//     force a fresh connection on the next forward.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestForwarder_RedirectNotFollowedBodyPreserved(t *testing.T) {
	type capturedReq struct {
		method string
		body   string
	}
	var mu sync.Mutex
	var reqs []capturedReq

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedReq{method: r.Method, body: string(body)})
		mu.Unlock()
		// 302 with a Location the forwarder must NOT chase.
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer remote.Close()

	// MaxRetries: 0 — a redirect is permanent, so the retry loop is not
	// under test and would only add real-clock backoff.
	fwd := transport.NewHTTPForwarderWithConfig("/transport/http",
		transport.ForwarderConfig{MaxRetries: 0, Timeout: 5 * time.Second})
	peer := &persistence.PeerInfo{
		InstanceID: "redirect-peer",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "msg-redirect", Subject: "test.redirect", Payload: []byte(`{"k":"v"}`),
	})

	err := fwd.Forward(context.Background(), peer, "route-redirect", env)
	if err == nil {
		t.Fatal("a redirect response must surface as a forward error, not a false success")
	}
	// Redirect is a misconfiguration (the same URL will redirect again) →
	// permanent, dead-lettered, never retried.
	if shared.IsRecoverableError(err) {
		t.Fatalf("redirect must classify as permanent (not recoverable), got %v", err)
	}
	be, ok := shared.AsBridgeError(err)
	if !ok || be.Class != shared.ErrorPermanent {
		t.Fatalf("expected permanent BridgeError, got %T %v", err, err)
	}
	if !errors.Is(err, shared.ErrForwardFailed) {
		t.Fatalf("redirect must map to ErrForwardFailed, got %v", err)
	}

	// Exactly one request reached the peer: the original POST WITH its
	// body. The client did not follow the redirect into a bodyless GET.
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("redirect must not be followed: expected 1 request, got %d (%+v)", len(reqs), reqs)
	}
	if reqs[0].method != http.MethodPost {
		t.Fatalf("expected the single request to be POST, got %s", reqs[0].method)
	}
	if reqs[0].body == "" {
		t.Fatal("forward body was dropped: the peer received an empty body")
	}
}

func TestForwarder_FullBodyDrainAllowsConnectionReuse(t *testing.T) {
	// A body larger than the former 4096-byte drain cap: if the forwarder
	// stops reading short, the pool cannot reuse the connection.
	largeBody := bytes.Repeat([]byte("x"), 64*1024)

	var newConns int32
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(largeBody)
	}))
	remote.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&newConns, 1)
		}
	}
	remote.Start()
	defer remote.Close()

	fwd := transport.NewHTTPForwarderWithConfig("/transport/http",
		transport.ForwarderConfig{MaxRetries: 0, Timeout: 5 * time.Second})
	peer := &persistence.PeerInfo{
		InstanceID: "reuse-peer",
		Endpoints:  map[string]string{"http": remote.URL},
	}

	// Two SEQUENTIAL forwards through the same forwarder (same client).
	// With the full drain the first connection returns to the pool and is
	// reused; a short drain would strand unread bytes and force a second.
	for i := 0; i < 2; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: "msg-reuse", Subject: "test.reuse", Payload: []byte(`{}`),
		})
		if err := fwd.Forward(context.Background(), peer, "route-reuse", env); err != nil {
			t.Fatalf("forward %d: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(&newConns); got != 1 {
		t.Fatalf("expected connection reuse (1 new connection) after full body drain, got %d", got)
	}
}
