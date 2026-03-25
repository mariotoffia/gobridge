package credentials_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/credentials"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// dummyRepo implements types.CredentialsRepository for test purposes.
type dummyRepo struct {
	scheme    string
	namespace string
}

func (d dummyRepo) GetScheme() string    { return d.scheme }
func (d dummyRepo) GetNamespace() string { return d.namespace }
func (d dummyRepo) GetCredentials(serverURI string) (*types.Credentials, error) {
	return &types.Credentials{}, nil
}

// countingRepo counts GetCredentials calls for cache testing.
type countingRepo struct {
	scheme    string
	namespace string
	callCount atomic.Int32
}

func (c *countingRepo) GetScheme() string    { return c.scheme }
func (c *countingRepo) GetNamespace() string { return c.namespace }
func (c *countingRepo) GetCredentials(serverURI string) (*types.Credentials, error) {
	c.callCount.Add(1)
	return &types.Credentials{
		Type:        []types.CredentialsType{types.CredentialsTypeUsernamePassword},
		Credentials: []any{types.UsernamePasswordCredentials{Username: "test", Password: "pass"}},
	}, nil
}

func TestResolver_ResolveRepository(t *testing.T) {
	r := credentials.NewResolver()

	// Register several repositories
	root := dummyRepo{scheme: "pms", namespace: ""}
	tenantA := dummyRepo{scheme: "pms", namespace: "tenantA"}
	tenantAApp1 := dummyRepo{scheme: "pms", namespace: "tenantA/app1"}
	otherScheme := dummyRepo{scheme: "mqtt", namespace: "tenantA/app1"}

	r.RegisterRepository(root)
	r.RegisterRepository(tenantA)
	r.RegisterRepository(tenantAApp1)
	r.RegisterRepository(otherScheme)

	tests := []struct {
		serverURI     string
		wantNamespace string
		wantFound     bool
	}{
		{
			serverURI:     "pms://tenantA/app1/prod/db/password",
			wantNamespace: "tenantA/app1",
			wantFound:     true,
		},
		{
			serverURI:     "pms://tenantA/app2/prod/db/password",
			wantNamespace: "tenantA",
			wantFound:     true,
		},
		{
			serverURI:     "pms://tenantB/appX/xyz",
			wantNamespace: "",
			wantFound:     true,
		},
		{
			serverURI:     "mqtt://tenantA/app1/broker",
			wantNamespace: "tenantA/app1",
			wantFound:     true,
		},
		{
			serverURI:     "mqtt://tenantA/app2/broker",
			wantNamespace: "",
			wantFound:     false,
		},
		{
			serverURI:     "invalid-uri-format",
			wantNamespace: "",
			wantFound:     false,
		},
	}

	for _, tc := range tests {
		repo, found, err := r.ResolveRepository(tc.serverURI)
		if err != nil {
			if tc.wantFound {
				t.Errorf("ResolveRepository(%q) returned error %v, want no error", tc.serverURI, err)
			}
			// skip further checks if error
			continue
		}
		if found != tc.wantFound {
			t.Errorf("ResolveRepository(%q) found = %v, want %v", tc.serverURI, found, tc.wantFound)
			continue
		}
		if !found {
			// we expected no repository
			continue
		}
		gotNs := repo.GetNamespace()
		if gotNs != tc.wantNamespace {
			t.Errorf("ResolveRepository(%q) namespace = %q, want %q", tc.serverURI, gotNs, tc.wantNamespace)
		}
	}
}

// TestResolver_GetCredentialsWithCache validates caching behavior.
func TestResolver_GetCredentialsWithCache(t *testing.T) {
	repo := &countingRepo{scheme: "pms", namespace: ""}
	r := credentials.NewResolver(credentials.WithCacheTTL(1 * time.Hour))
	r.RegisterRepository(repo)

	uri := "pms://app/service/credentials"

	// First call should hit the repository
	creds1, err := r.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}
	if creds1 == nil {
		t.Fatal("expected credentials, got nil")
	}

	if repo.callCount.Load() != 1 {
		t.Errorf("expected 1 repository call, got %d", repo.callCount.Load())
	}

	// Second call should use cache
	creds2, err := r.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}
	if creds2 == nil {
		t.Fatal("expected credentials, got nil")
	}

	if repo.callCount.Load() != 1 {
		t.Errorf("expected still 1 repository call (cached), got %d", repo.callCount.Load())
	}

	// Check cache stats
	stats := r.CacheStats()
	if stats.Size != 1 {
		t.Errorf("expected cache size 1, got %d", stats.Size)
	}
	if stats.Active != 1 {
		t.Errorf("expected 1 active entry, got %d", stats.Active)
	}
}

// TestResolver_CacheDisabled validates disabled caching.
func TestResolver_CacheDisabled(t *testing.T) {
	repo := &countingRepo{scheme: "pms", namespace: ""}
	r := credentials.NewResolver(credentials.WithCacheDisabled())
	r.RegisterRepository(repo)

	uri := "pms://app/service/credentials"

	// Each call should hit the repository
	for i := 0; i < 3; i++ {
		_, err := r.GetCredentials(uri)
		if err != nil {
			t.Fatalf("GetCredentials failed: %v", err)
		}
	}

	if repo.callCount.Load() != 3 {
		t.Errorf("expected 3 repository calls (cache disabled), got %d", repo.callCount.Load())
	}
}

// TestResolver_InvalidateCache validates cache invalidation.
func TestResolver_InvalidateCache(t *testing.T) {
	repo := &countingRepo{scheme: "pms", namespace: ""}
	r := credentials.NewResolver(credentials.WithCacheTTL(1 * time.Hour))
	r.RegisterRepository(repo)

	uri := "pms://app/service/credentials"

	// First call
	_, err := r.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}

	if repo.callCount.Load() != 1 {
		t.Errorf("expected 1 repository call, got %d", repo.callCount.Load())
	}

	// Invalidate cache
	r.InvalidateCache(uri)

	// Next call should hit repository again
	_, err = r.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}

	if repo.callCount.Load() != 2 {
		t.Errorf("expected 2 repository calls after invalidation, got %d", repo.callCount.Load())
	}
}

// TestResolver_ClearCache validates cache clearing.
func TestResolver_ClearCache(t *testing.T) {
	repo := &countingRepo{scheme: "pms", namespace: ""}
	r := credentials.NewResolver(credentials.WithCacheTTL(1 * time.Hour))
	r.RegisterRepository(repo)

	// Cache multiple URIs
	uris := []string{
		"pms://app1/creds",
		"pms://app2/creds",
		"pms://app3/creds",
	}

	for _, uri := range uris {
		_, err := r.GetCredentials(uri)
		if err != nil {
			t.Fatalf("GetCredentials failed: %v", err)
		}
	}

	stats := r.CacheStats()
	if stats.Size != 3 {
		t.Errorf("expected cache size 3, got %d", stats.Size)
	}

	// Clear cache
	r.ClearCache()

	stats = r.CacheStats()
	if stats.Size != 0 {
		t.Errorf("expected cache size 0 after clear, got %d", stats.Size)
	}
}

// TestResolver_CacheExpiry validates cache expiry behavior.
func TestResolver_CacheExpiry(t *testing.T) {
	repo := &countingRepo{scheme: "pms", namespace: ""}
	// Very short TTL for testing
	r := credentials.NewResolver(credentials.WithCacheTTL(10 * time.Millisecond))
	r.RegisterRepository(repo)

	uri := "pms://app/service/credentials"

	// First call
	_, err := r.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}

	if repo.callCount.Load() != 1 {
		t.Errorf("expected 1 repository call, got %d", repo.callCount.Load())
	}

	// Wait for expiry
	time.Sleep(20 * time.Millisecond)

	// Next call should hit repository again (expired)
	_, err = r.GetCredentials(uri)
	if err != nil {
		t.Fatalf("GetCredentials failed: %v", err)
	}

	if repo.callCount.Load() != 2 {
		t.Errorf("expected 2 repository calls after expiry, got %d", repo.callCount.Load())
	}
}
