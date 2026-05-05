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
	"github.com/mariotoffia/gobridge/domain"
)

// ---------------------------------------------------------------------------
// BUG RES-007: HTTP Forwarder doesn't classify 4xx vs 5xx
// ---------------------------------------------------------------------------

func TestBugForwarder_5xxReturnsTransientError(t *testing.T) {
	codes := []int{500, 502, 503, 504}
	for _, code := range codes {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer remote.Close()
			fwd := transport.NewHTTPForwarder("/transport/http", 5*time.Second)
			peer := &domain.PeerInfo{
				InstanceID: "remote-5xx",
				Endpoints:  map[string]string{"http": remote.URL},
			}
			env := &domain.Envelope{
				ID:      "msg-5xx",
				Subject: "test.5xx",
				Payload: []byte(`{}`),
			}

			err := fwd.Forward(context.Background(), peer, "route-5xx", env)
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", code)
			}

			// 5xx must be transient (retriable).
			if !domain.IsRecoverableError(err) {
				t.Fatalf("expected recoverable/transient error for %d, got non-recoverable", code)
			}

			// Should match ErrUnavailable sentinel.
			if !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("expected ErrUnavailable for %d, got %v", code, err)
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
			peer := &domain.PeerInfo{
				InstanceID: "remote-4xx",
				Endpoints:  map[string]string{"http": remote.URL},
			}
			env := &domain.Envelope{
				ID:      "msg-4xx",
				Subject: "test.4xx",
				Payload: []byte(`{}`),
			}

			err := fwd.Forward(context.Background(), peer, "route-4xx", env)
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", code)
			}

			// 4xx must be permanent (not retriable).
			if domain.IsRecoverableError(err) {
				t.Fatalf("expected permanent/non-recoverable error for %d, got recoverable", code)
			}

			be, ok := domain.AsBridgeError(err)
			if !ok {
				t.Fatalf("expected BridgeError for %d, got %T", code, err)
			}
			if be.Class != domain.ErrorPermanent {
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
	peer := &domain.PeerInfo{
		InstanceID: "remote-ok",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := &domain.Envelope{
		ID:      "msg-ok",
		Subject: "test.ok",
		Payload: []byte(`{}`),
	}

	if err := fwd.Forward(context.Background(), peer, "route-ok", env); err != nil {
		t.Fatalf("expected no error for 200, got %v", err)
	}
}
