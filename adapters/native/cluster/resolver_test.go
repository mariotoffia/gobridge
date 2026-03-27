package cluster_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/native/cluster"
)

func TestNativeEndpointResolver_StaticHost(t *testing.T) {
	ctx := context.Background()
	r := cluster.NewNativeEndpointResolver(cluster.WithStaticHost("test.local"))

	m, err := r.Resolve(ctx, ":9090")
	if err != nil {
		t.Fatal(err)
	}
	if got := m["http"]; got != "http://test.local:9090" {
		t.Fatalf("http endpoint: got %q want %q", got, "http://test.local:9090")
	}
}

func TestNativeEndpointResolver_DiscoverHost(t *testing.T) {
	ctx := context.Background()
	r := cluster.NewNativeEndpointResolver()

	m, err := r.Resolve(ctx, ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatal("expected non-empty endpoint map")
	}
	httpURL, ok := m["http"]
	if !ok || httpURL == "" {
		t.Fatalf("expected non-empty http key, got %#v", m)
	}
	if !strings.HasPrefix(httpURL, "http://") {
		t.Fatalf("expected http URL prefix, got %q", httpURL)
	}
}

func TestNativeEndpointResolver_PortExtraction(t *testing.T) {
	ctx := context.Background()
	r := cluster.NewNativeEndpointResolver(cluster.WithStaticHost("example.test"))

	m, err := r.Resolve(ctx, "0.0.0.0:4567")
	if err != nil {
		t.Fatal(err)
	}
	httpURL := m["http"]
	if !strings.Contains(httpURL, ":4567") {
		t.Fatalf("expected port 4567 in %q", httpURL)
	}
}
