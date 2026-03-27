package runtime_test

// ═══════════════════════════════════════════════
// CredentialResolver Race & Cache Tests
//
// Tests validating TOCTOU race fix (GO-3) and
// cache behavior under concurrent access.
//
// Summary:
// ┌──────┬────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                │ Status   │
// ├──────┼────────────────────────────────────────────┼──────────┤
// │ T001 │ Concurrent resolve+invalidate no race      │ PASS     │
// │ T002 │ Expired entry evicted correctly             │ PASS     │
// │ T003 │ Cache disabled returns fresh each time      │ PASS     │
// └──────┴────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/runtime"
)

type fakeCredRepo struct {
	mu        sync.Mutex
	scheme    string
	namespace string
	calls     int
	creds     *domain.CredentialSet
	err       error
}

func (r *fakeCredRepo) Scheme() string    { return r.scheme }
func (r *fakeCredRepo) Namespace() string { return r.namespace }

func (r *fakeCredRepo) Get(_ context.Context, _ string) (*domain.CredentialSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.creds, nil
}

func (r *fakeCredRepo) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestCredentialResolver_ConcurrentResolve validates no race condition
// under concurrent access with the -race detector.
func TestCredentialResolver_ConcurrentResolve(t *testing.T) {
	repo := &fakeCredRepo{
		scheme:    "test",
		namespace: "",
		creds: &domain.CredentialSet{
			Password: &domain.PasswordCredential{
				Username: "user",
				Password: "pass",
			},
		},
	}

	resolver := runtime.NewCredentialResolver(runtime.WithCredentialCacheTTL(5 * time.Second))
	resolver.Register(repo)

	var wg sync.WaitGroup
	const goroutines = 50

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx := context.Background()

			_, err := resolver.Resolve(ctx, "test://creds/main")
			if err != nil {
				t.Errorf("goroutine %d: resolve error: %v", idx, err)
				return
			}

			if idx%10 == 0 {
				resolver.InvalidateCache("test://creds/main")
			}
		}(i)
	}

	wg.Wait()

	if repo.CallCount() < 1 {
		t.Fatal("expected at least one backend call")
	}
}

// TestCredentialResolver_CacheExpiry validates expired entries are evicted.
func TestCredentialResolver_CacheExpiry(t *testing.T) {
	repo := &fakeCredRepo{
		scheme:    "test",
		namespace: "",
		creds: &domain.CredentialSet{
			Password: &domain.PasswordCredential{
				Username: "user",
				Password: "secret",
			},
		},
	}

	resolver := runtime.NewCredentialResolver(runtime.WithCredentialCacheTTL(50 * time.Millisecond))
	resolver.Register(repo)

	ctx := context.Background()

	_, err := resolver.Resolve(ctx, "test://creds/a")
	if err != nil {
		t.Fatal(err)
	}

	initialCalls := repo.CallCount()

	_, err = resolver.Resolve(ctx, "test://creds/a")
	if err != nil {
		t.Fatal(err)
	}

	if repo.CallCount() != initialCalls {
		t.Fatal("second call should use cache")
	}

	time.Sleep(60 * time.Millisecond)

	_, err = resolver.Resolve(ctx, "test://creds/a")
	if err != nil {
		t.Fatal(err)
	}

	if repo.CallCount() <= initialCalls {
		t.Fatal("after expiry, should call backend again")
	}
}

// TestCredentialResolver_CacheDisabled validates that disabling the cache
// calls the backend on every resolve.
func TestCredentialResolver_CacheDisabled(t *testing.T) {
	repo := &fakeCredRepo{
		scheme:    "test",
		namespace: "",
		creds: &domain.CredentialSet{
			Password: &domain.PasswordCredential{
				Username: "user",
				Password: "pass",
			},
		},
	}

	resolver := runtime.NewCredentialResolver(runtime.WithCredentialCacheDisabled())
	resolver.Register(repo)

	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := resolver.Resolve(ctx, "test://creds/b")
		if err != nil {
			t.Fatal(err)
		}
	}

	if repo.CallCount() != 5 {
		t.Fatalf("expected 5 backend calls with cache disabled, got %d", repo.CallCount())
	}
}
