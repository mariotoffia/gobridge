package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// programmableRepo is a CredentialRepository whose Get result can be swapped
// between calls so tests can drive the cache into an expired state and then
// fail (or succeed) the backend deterministically.
type programmableRepo struct {
	scheme    string
	namespace string
	mu        sync.Mutex
	creds     *connectivity.CredentialSet
	err       error
	calls     atomic.Int32
}

func (p *programmableRepo) Scheme() string    { return p.scheme }
func (p *programmableRepo) Namespace() string { return p.namespace }

func (p *programmableRepo) Get(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creds, p.err
}

func (p *programmableRepo) set(creds *connectivity.CredentialSet, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creds, p.err = creds, err
}

// TestCredentialResolver_ServesStaleOnRetryableError is the regression
// test (retryable branch): once a cache entry has expired, a RETRYABLE
// (transient) backend failure must serve the last-known-good value instead of
// failing the rebuild, emit MetricCredentialStaleServed, and NOT extend the
// TTL (so recovery is immediate once the backend returns).
func TestCredentialResolver_ServesStaleOnRetryableError(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	good := connectivity.NewCredentialSet(pwCred("good-user", "good-pass"), nil)
	repo := &programmableRepo{scheme: "pms", creds: good}
	rec := &ports.RecordingExporter{}

	r := NewCredentialResolver(
		WithCredentialClock(fake),
		WithCredentialCacheTTL(time.Minute),
		WithCredentialMetrics(rec),
	)
	r.Register(repo)

	// Prime the cache with the known-good value.
	got, err := r.Resolve(context.Background(), "pms://ns/p")
	require.NoError(t, err)
	require.Equal(t, "good-user", got.Password().Username())

	// Expire the cache entry and break the backend with a transient error.
	fake.Advance(2 * time.Minute)
	repo.set(nil, shared.ErrUnavailable)

	got, err = r.Resolve(context.Background(), "pms://ns/p")
	require.NoError(t, err, "a retryable backend error must serve stale, not fail")
	require.Equal(t, "good-user", got.Password().Username(), "must serve the last-known-good value")

	require.NotEmpty(t, rec.FindEntries(shared.MetricCredentialStaleServed), "stale-served metric must be emitted")
	require.NotEmpty(t, rec.FindEntries(shared.MetricCredentialResolveFailure), "the underlying resolve failure must still be counted")

	// TTL was not extended: once the backend recovers, the fresh value flows.
	fresh := connectivity.NewCredentialSet(pwCred("fresh-user", "fresh-pass"), nil)
	repo.set(fresh, nil)
	got, err = r.Resolve(context.Background(), "pms://ns/p")
	require.NoError(t, err)
	require.Equal(t, "fresh-user", got.Password().Username(), "recovery must be immediate; stale must not be pinned by a refreshed TTL")
}

// TestCredentialResolver_PropagatesPermanentError is the regression test
// (permanent branch): a PERMANENT backend error must propagate even when a
// stale cached value exists — stale credentials must never mask a revocation
// (NOT_AUTHORIZED) or a hard NOT_FOUND.
func TestCredentialResolver_PropagatesPermanentError(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	good := connectivity.NewCredentialSet(pwCred("good-user", "good-pass"), nil)
	repo := &programmableRepo{scheme: "pms", creds: good}
	rec := &ports.RecordingExporter{}

	r := NewCredentialResolver(
		WithCredentialClock(fake),
		WithCredentialCacheTTL(time.Minute),
		WithCredentialMetrics(rec),
	)
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "pms://ns/p")
	require.NoError(t, err)

	fake.Advance(2 * time.Minute)
	repo.set(nil, shared.ErrNotFound) // PERMANENT

	_, err = r.Resolve(context.Background(), "pms://ns/p")
	require.Error(t, err, "a permanent error must propagate, not serve stale")
	require.ErrorIs(t, err, shared.ErrNotFound)

	require.Empty(t, rec.FindEntries(shared.MetricCredentialStaleServed), "stale must NOT be served on a permanent error")
	require.NotEmpty(t, rec.FindEntries(shared.MetricCredentialResolveFailure), "the resolve failure must be counted")
}

// TestCredentialResolver_NilCredsIsErrorNotPermanentMiss is the regression
// test: a repository that returns (nil, nil) must be treated as a hard error
// (INVALID_PAYLOAD) rather than cached as a permanent miss / allowed to connect
// anonymously, and the resolver must recover cleanly once the backend returns
// real credentials (proving nothing poisonous was cached).
func TestCredentialResolver_NilCredsIsErrorNotPermanentMiss(t *testing.T) {
	t.Parallel()

	repo := &programmableRepo{scheme: "pms", creds: nil, err: nil} // (nil, nil)
	rec := &ports.RecordingExporter{}

	r := NewCredentialResolver(WithCredentialMetrics(rec))
	r.Register(repo)

	_, err := r.Resolve(context.Background(), "pms://ns/p")
	require.Error(t, err, "(nil, nil) from a repository must be an error, not a silent anonymous connect")
	require.ErrorIs(t, err, shared.ErrInvalidPayload)
	require.NotEmpty(t, rec.FindEntries(shared.MetricCredentialResolveFailure))

	// No poisoned cache entry: a subsequent real value resolves normally.
	good := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
	repo.set(good, nil)
	got, err := r.Resolve(context.Background(), "pms://ns/p")
	require.NoError(t, err)
	require.Equal(t, "u", got.Password().Username())
}

// TestCredentialResolver_ErrorRedactsURIUserinfo verifies that a URI carrying
// embedded userinfo (user:pass@) never appears verbatim in a resolver error
// string — neither the "no repository" nor the parse-failure path may leak it.
func TestCredentialResolver_ErrorRedactsURIUserinfo(t *testing.T) {
	t.Parallel()

	r := NewCredentialResolver() // no repositories registered

	// Parseable URI, unregistered scheme -> "no credential repository" path.
	_, err := r.Resolve(context.Background(), "pms://user:s3cr3t@ns/param")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cr3t", "userinfo must be redacted from the resolver error")

	// Unparseable URI (control char) -> resolveRepo url.Parse-failure path.
	_, err = r.Resolve(context.Background(), "pms://user:s3cr3t@ns/\x7fbad")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cr3t", "userinfo must be redacted even when the URI fails to parse")
}
