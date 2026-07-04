package transport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ---------------------------------------------------------------------------
// BUG RES-007: HTTP Forwarder doesn't classify 4xx vs 5xx
// ---------------------------------------------------------------------------

func TestBugForwarder_TransientStatusesReturnTransientError(t *testing.T) {
	cases := []struct {
		code     int
		sentinel *shared.BridgeError
	}{
		// Every 5xx is transient (peer down / overloaded / proxy error).
		{500, shared.ErrUnavailable},
		{502, shared.ErrUnavailable},
		{503, shared.ErrUnavailable},
		{504, shared.ErrUnavailable},
		// 429/408 are transient too: a throttled or slow peer would have
		// accepted the message moments later — classifying them as
		// permanent dead-letters messages that only needed a retry.
		{429, shared.ErrThrottled},
		{408, shared.ErrTimeout},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer remote.Close()
			// MaxRetries: 0 — classification is what is under test, not
			// the retry loop; retrying here would only add real-clock
			// backoff delays.
			fwd := transport.NewHTTPForwarderWithConfig("/transport/http",
				transport.ForwarderConfig{MaxRetries: 0, Timeout: 5 * time.Second})
			peer := &persistence.PeerInfo{
				InstanceID: "remote-transient",
				Endpoints:  map[string]string{"http": remote.URL},
			}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "msg-transient",
				Subject: "test.transient",
				Payload: []byte(`{}`),
			})

			err := fwd.Forward(context.Background(), peer, "route-transient", env)
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", tc.code)
			}

			// Transient statuses must be retriable, never dead-lettered.
			if !shared.IsRecoverableError(err) {
				t.Fatalf("expected recoverable/transient error for %d, got non-recoverable: %v", tc.code, err)
			}

			// Each status maps to its domain sentinel.
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("expected %v for %d, got %v", tc.sentinel, tc.code, err)
			}
		})
	}
}

func TestBugForwarder_4xxReturnsPermanentError(t *testing.T) {
	codes := []int{400, 401, 403, 404, 409, 422}
	for _, code := range codes {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer remote.Close()
			fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
			peer := &persistence.PeerInfo{
				InstanceID: "remote-4xx",
				Endpoints:  map[string]string{"http": remote.URL},
			}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "msg-4xx",
				Subject: "test.4xx",
				Payload: []byte(`{}`),
			})

			err := fwd.Forward(context.Background(), peer, "route-4xx", env)
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", code)
			}

			// 4xx must be permanent (not retriable).
			if shared.IsRecoverableError(err) {
				t.Fatalf("expected permanent/non-recoverable error for %d, got recoverable", code)
			}

			be, ok := shared.AsBridgeError(err)
			if !ok {
				t.Fatalf("expected BridgeError for %d, got %T", code, err)
			}
			if be.Class != shared.ErrorPermanent {
				t.Fatalf("expected ErrorPermanent class for %d, got %q", code, be.Class)
			}
		})
	}
}

func TestBugForwarder_2xxReturnsNoError(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"}) //nolint:errcheck
	}))
	defer remote.Close()
	fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
	peer := &persistence.PeerInfo{
		InstanceID: "remote-ok",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-ok",
		Subject: "test.ok",
		Payload: []byte(`{}`),
	})

	if err := fwd.Forward(context.Background(), peer, "route-ok", env); err != nil {
		t.Fatalf("expected no error for 200, got %v", err)
	}
}
