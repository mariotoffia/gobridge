package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubRepo struct {
	scheme    string
	namespace string
	creds     *domain.CredentialSet
	callCount atomic.Int32
}

func (s *stubRepo) Scheme() string    { return s.scheme }
func (s *stubRepo) Namespace() string { return s.namespace }
func (s *stubRepo) Get(_ context.Context, _ string) (*domain.CredentialSet, error) {
	s.callCount.Add(1)
	return s.creds, nil
}

func TestCredentialResolver_SingleSchemeDispatch(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver()
	r.Register(repo)

	got, err := r.Resolve(context.Background(), "file://path/to/creds")
	require.NoError(t, err)
	assert.Equal(t, creds, got)
	assert.Equal(t, int32(1), repo.callCount.Load())
}

func TestCredentialResolver_MultiSchemeDispatch(t *testing.T) {
	fileCreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "file-user", Password: "fp"},
	}
	pmsCreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "pms-user", Password: "pp"},
	}
	fileRepo := &stubRepo{scheme: "file", creds: fileCreds}
	pmsRepo := &stubRepo{scheme: "pms", creds: pmsCreds}

	r := NewCredentialResolver()
	r.Register(fileRepo)
	r.Register(pmsRepo)

	got, err := r.Resolve(context.Background(), "file://some/path")
	require.NoError(t, err)
	assert.Equal(t, fileCreds, got)
	assert.Equal(t, int32(1), fileRepo.callCount.Load())
	assert.Equal(t, int32(0), pmsRepo.callCount.Load())

	got, err = r.Resolve(context.Background(), "pms://other/path")
	require.NoError(t, err)
	assert.Equal(t, pmsCreds, got)
	assert.Equal(t, int32(1), pmsRepo.callCount.Load())
}

func TestCredentialResolver_NamespaceLongestPrefix(t *testing.T) {
	rootCreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "root"},
	}
	tenantACreds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "tenantA"},
	}
	app1Creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "app1"},
	}

	rootRepo := &stubRepo{scheme: "pms", namespace: "", creds: rootCreds}
	tenantARepo := &stubRepo{scheme: "pms", namespace: "tenantA", creds: tenantACreds}
	app1Repo := &stubRepo{scheme: "pms", namespace: "tenantA/app1", creds: app1Creds}

	r := NewCredentialResolver()
	r.Register(rootRepo)
	r.Register(tenantARepo)
	r.Register(app1Repo)

	tests := []struct {
		name     string
		uri      string
		wantUser string
		wantRepo *stubRepo
	}{
		{
			name:     "deepest match tenantA/app1",
			uri:      "pms://tenantA/app1/prod/db",
			wantUser: "app1",
			wantRepo: app1Repo,
		},
		{
			name:     "mid match tenantA",
			uri:      "pms://tenantA/app2/prod/db",
			wantUser: "tenantA",
			wantRepo: tenantARepo,
		},
		{
			name:     "fallback to root",
			uri:      "pms://tenantB/appX/xyz",
			wantUser: "root",
			wantRepo: rootRepo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), tt.uri)
			require.NoError(t, err)
			assert.Equal(t, tt.wantUser, got.Password.Username)
		})
	}
}

func TestCredentialResolver_NotFoundError(t *testing.T) {
	r := NewCredentialResolver()

	_, err := r.Resolve(context.Background(), "vault://secret/data")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrNotFound), "expected ErrNotFound, got: %v", err)
}

func TestCredentialResolver_CacheHitMiss(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "cached"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	got1, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, creds, got1)

	got2, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, creds, got2)

	assert.Equal(t, int32(1), repo.callCount.Load(), "second call should be served from cache")
}

func TestCredentialResolver_CacheExpiry(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "expiring"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(10 * time.Millisecond))
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(1), repo.callCount.Load())

	time.Sleep(20 * time.Millisecond)

	_, err = r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(2), repo.callCount.Load(), "expired cache entry should cause re-fetch")
}

func TestCredentialResolver_CacheDisabled(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "nocache"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheDisabled())
	r.Register(repo)

	for i := 0; i < 3; i++ {
		_, err := r.Resolve(context.Background(), "file://data")
		require.NoError(t, err)
	}

	assert.Equal(t, int32(3), repo.callCount.Load(), "all calls should hit the repo directly")
}

func TestCredentialResolver_InvalidateCache(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "inv"},
	}
	repo := &stubRepo{scheme: "file", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(1), repo.callCount.Load())

	r.InvalidateCache("file://data")

	_, err = r.Resolve(context.Background(), "file://data")
	require.NoError(t, err)
	assert.Equal(t, int32(2), repo.callCount.Load(), "invalidated entry should cause re-fetch")
}

func TestCredentialResolver_ClearCache(t *testing.T) {
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "clear"},
	}
	repo := &stubRepo{scheme: "pms", creds: creds}

	r := NewCredentialResolver(WithCredentialCacheTTL(time.Hour))
	r.Register(repo)

	uris := []string{"pms://a/x", "pms://a/y", "pms://a/z"}
	for _, uri := range uris {
		_, err := r.Resolve(context.Background(), uri)
		require.NoError(t, err)
	}
	assert.Equal(t, 3, r.CacheStats().Size)

	r.ClearCache()
	assert.Equal(t, 0, r.CacheStats().Size)
}
