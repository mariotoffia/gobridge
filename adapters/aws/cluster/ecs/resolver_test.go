package ecs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

const validMetadataBody = `{"Networks":[{"NetworkMode":"awsvpc","IPv4Addresses":["10.0.0.5"],"PrivateDNSName":"ip-10-0-0-5.ec2.internal"}]}`

// newFlakyServer returns an httptest server whose handler is driven by fn,
// which receives the 1-based request count and writes the response. The
// returned counter reports how many requests were served.
func newFlakyServer(t *testing.T, fn func(count int64, w http.ResponseWriter)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fn(count.Add(1), w)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// TestResolve_RetriesUntilSuccess verifies transient non-200 responses are
// retried and Resolve succeeds once the metadata endpoint recovers (finding 10).
func TestResolve_RetriesUntilSuccess(t *testing.T) {
	srv, count := newFlakyServer(t, func(c int64, w http.ResponseWriter) {
		if c < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(validMetadataBody))
	})
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)

	r := NewEcsEndpointResolver(WithMaxAttempts(5), WithRetryBackoff(time.Millisecond))
	got, err := r.Resolve(context.Background(), "0.0.0.0:8080")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if want := "http://ip-10-0-0-5.ec2.internal:8080"; got["http"] != want {
		t.Fatalf("endpoint: got %q, want %q", got["http"], want)
	}
	if n := count.Load(); n != 3 {
		t.Fatalf("attempts: got %d, want 3", n)
	}
}

// TestResolve_RetriesNotYetPopulatedMetadata verifies that a 200 response whose
// metadata is not yet fully populated (empty Networks early in task life) is
// retried rather than accepted or failed permanently.
func TestResolve_RetriesNotYetPopulatedMetadata(t *testing.T) {
	srv, count := newFlakyServer(t, func(c int64, w http.ResponseWriter) {
		if c < 3 {
			_, _ = w.Write([]byte(`{"Networks":[]}`))
			return
		}
		_, _ = w.Write([]byte(validMetadataBody))
	})
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)

	r := NewEcsEndpointResolver(WithMaxAttempts(5), WithRetryBackoff(time.Millisecond))
	got, err := r.Resolve(context.Background(), "0.0.0.0:8080")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got["http"] == "" {
		t.Fatalf("expected a resolved endpoint, got empty")
	}
	if n := count.Load(); n != 3 {
		t.Fatalf("attempts: got %d, want 3", n)
	}
}

// TestResolve_ExhaustsAttempts verifies Resolve gives up after the configured
// number of attempts and reports the bound in the error.
func TestResolve_ExhaustsAttempts(t *testing.T) {
	srv, count := newFlakyServer(t, func(_ int64, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)

	r := NewEcsEndpointResolver(WithMaxAttempts(3), WithRetryBackoff(time.Millisecond))
	_, err := r.Resolve(context.Background(), "0.0.0.0:8080")
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if want := "after 3 attempt(s)"; !contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err.Error(), want)
	}
	if n := count.Load(); n != 3 {
		t.Fatalf("attempts: got %d, want 3", n)
	}
}

// TestResolve_NoMetadataURI verifies a missing metadata env var fails
// immediately without any HTTP call (permanent misconfiguration, not retried).
func TestResolve_NoMetadataURI(t *testing.T) {
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", "")

	r := NewEcsEndpointResolver(WithMaxAttempts(5), WithRetryBackoff(time.Millisecond))
	_, err := r.Resolve(context.Background(), "0.0.0.0:8080")
	if !errors.Is(err, errNoMetadataURI) {
		t.Fatalf("expected errNoMetadataURI, got %v", err)
	}
}

// TestResolve_ContextCancelled verifies a cancelled context stops the retry
// loop immediately.
func TestResolve_ContextCancelled(t *testing.T) {
	srv, _ := newFlakyServer(t, func(_ int64, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := NewEcsEndpointResolver(WithMaxAttempts(5), WithRetryBackoff(time.Second))
	_, err := r.Resolve(ctx, "0.0.0.0:8080")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestResolve_DeterministicBackoff exercises the retry backoff on an injected
// fake clock so no wall-clock time is consumed: the resolver blocks on the fake
// timer until the test advances it (TESTS.md: no sleeping in timing tests).
func TestResolve_DeterministicBackoff(t *testing.T) {
	srv, count := newFlakyServer(t, func(c int64, w http.ResponseWriter) {
		if c < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(validMetadataBody))
	})
	t.Setenv("ECS_CONTAINER_METADATA_URI_V4", srv.URL)

	fake := clocktest.New()
	r := NewEcsEndpointResolver(
		WithMaxAttempts(5),
		WithRetryBackoff(30*time.Second),
		WithClock(fake),
	)

	type result struct {
		endpoints map[string]string
		err       error
	}
	done := make(chan result, 1)
	go func() {
		ep, err := r.Resolve(context.Background(), "0.0.0.0:8080")
		done <- result{ep, err}
	}()

	// The first attempt fails and the resolver blocks on the fake timer.
	waitForTimer(t, fake)
	fake.Advance(30 * time.Second)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Resolve: unexpected error: %v", res.err)
		}
		if res.endpoints["http"] == "" {
			t.Fatal("expected a resolved endpoint")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not complete after advancing the clock")
	}
	if n := count.Load(); n != 2 {
		t.Fatalf("attempts: got %d, want 2", n)
	}
}

func waitForTimer(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.TimerCount() >= 1 {
			return
		}
		time.Sleep(time.Millisecond) // OTHER: bounded poll for fake-clock timer registration (no fake-time dependency)
	}
	t.Fatal("resolver never armed a retry timer")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
