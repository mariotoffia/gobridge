package file

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

var (
	_ ports.CredentialRepository = (*Repository)(nil)
	_ ports.CredentialAdmin      = (*Repository)(nil)
)

func passwordCreds(username, password string) *connectivity.CredentialSet {
	pw := connectivity.NewPasswordCredential(username, password)
	return connectivity.NewCredentialSet(&pw, nil)
}

func tlsCreds() *connectivity.CredentialSet {
	tls := connectivity.NewTLSMaterial(
		"-----BEGIN CERTIFICATE-----\ntest-cert\n-----END CERTIFICATE-----",
		"-----BEGIN PRIVATE KEY-----\ntest-key\n-----END PRIVATE KEY-----",
		[]string{"-----BEGIN CERTIFICATE-----\nca1\n-----END CERTIFICATE-----"},
		false,
	)
	return connectivity.NewCredentialSet(nil, &tls)
}

func combinedCreds() *connectivity.CredentialSet {
	pw := connectivity.NewPasswordCredential("admin", "s3cret")
	tls := connectivity.NewTLSMaterial(
		"-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----",
		"-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----",
		[]string{"-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----"},
		true,
	)
	return connectivity.NewCredentialSet(&pw, &tls)
}

// --- Constructor tests ---

// Verifies New succeeds for an existing temporary directory.
func TestNew_ValidPath(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)
	assert.NotNil(t, repo)
}

// Verifies New rejects an empty root path.
func TestNew_EmptyPath(t *testing.T) {
	_, err := New("")
	require.Error(t, err)
}

// Verifies New creates missing parent directories under the given root.
func TestNew_AutoCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "creds")
	repo, err := New(dir)
	require.NoError(t, err)
	assert.NotNil(t, repo)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// Verifies WithNamespace sets the repository namespace.
func TestNew_WithNamespace(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir, WithNamespace("prod"))
	require.NoError(t, err)
	assert.Equal(t, "prod", repo.Namespace())
}

// --- Scheme / Namespace ---

// Verifies Scheme returns the file credential scheme constant.
func TestScheme(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, Scheme, repo.Scheme())
}

// Verifies Namespace defaults to empty when not configured.
func TestNamespace_Default(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "", repo.Namespace())
}

// Verifies Namespace reflects the configured value from WithNamespace.
func TestNamespace_Configured(t *testing.T) {
	repo, err := New(t.TempDir(), WithNamespace("staging"))
	require.NoError(t, err)
	assert.Equal(t, "staging", repo.Namespace())
}

// --- URI-to-path / path-to-URI ---

// Verifies uriToPath maps well-formed file URIs to expected filesystem paths.
func TestURIToPath_ValidURIs(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)

	tests := []struct {
		name     string
		uri      string
		wantPath string
	}{
		{
			name:     "simple",
			uri:      "file://myhost",
			wantPath: filepath.Join(dir, "myhost"+FileExtension),
		},
		{
			name:     "nested",
			uri:      "file://broker/mqtt/prod",
			wantPath: filepath.Join(dir, "broker", "mqtt", "prod"+FileExtension),
		},
		{
			name:     "single segment",
			uri:      "file://server",
			wantPath: filepath.Join(dir, "server"+FileExtension),
		},
		{
			name:     "existing json extension",
			uri:      "file://server.json",
			wantPath: filepath.Join(dir, "server"+FileExtension),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.uriToPath(tt.uri)
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, got)
		})
	}
}

// Verifies uriToPath rejects malformed or non-file URIs.
func TestURIToPath_Invalid(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	tests := []struct {
		name string
		uri  string
	}{
		{"empty", ""},
		{"no scheme", "broker/mqtt"},
		{"wrong scheme", "vault://secret/db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repo.uriToPath(tt.uri)
			assert.Error(t, err)
		})
	}
}

// Verifies uriToPath rejects path traversal attempts in the URI authority and segments.
func TestURIToPath_PathTraversal(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	traversals := []string{
		"file://../../../etc/passwd",
		"file:///../../../etc/passwd",
		"file://..%2F..%2Fetc/passwd",
		"file://legit/../../escape",
	}

	for _, uri := range traversals {
		t.Run(uri, func(t *testing.T) {
			_, err := repo.uriToPath(uri)
			assert.Error(t, err, "path traversal should be rejected")
		})
	}
}

// Verifies List rejects prefix values that escape the repository root.
func TestList_PathTraversal(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	_, err = repo.List(context.Background(), "../../../etc")
	assert.Error(t, err, "list prefix traversal should be rejected")
}

// Verifies pathToURI inverts uriToPath for a representative URI.
func TestURIToPath_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)

	original := "file://broker/mqtt/prod"
	p, err := repo.uriToPath(original)
	require.NoError(t, err)

	roundTripped, err := repo.pathToURI(p)
	require.NoError(t, err)
	assert.Equal(t, original, roundTripped)
}

// --- Create ---

// Verifies Create writes versioned credentials with injected-clock timestamps and password fields.
func TestCreate_Success(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	repo, err := New(dir, WithClock(clocktest.NewAt(fixed)))
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	creds := passwordCreds("user1", "pass1")

	err = repo.Create(ctx, uri, creds)
	require.NoError(t, err)

	p, err := repo.uriToPath(uri)
	require.NoError(t, err)
	assert.FileExists(t, p)

	data, err := os.ReadFile(p)
	require.NoError(t, err)

	var stored storedCredentials
	require.NoError(t, json.Unmarshal(data, &stored))

	assert.Equal(t, int64(1), stored.Version)
	assert.Equal(t, fixed, stored.CreatedAt)
	assert.Equal(t, fixed, stored.UpdatedAt)
	assert.Equal(t, "user1", stored.Credentials.Password.Username)
	assert.Equal(t, "pass1", stored.Credentials.Password.Password)
}

// Verifies a second Create for the same URI returns ErrAlreadyExists.
func TestCreate_DuplicateRejectsWithAlreadyExists(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	err = repo.Create(ctx, uri, passwordCreds("u2", "p2"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrAlreadyExists))
}

// Verifies Create creates intermediate directories for nested URI paths.
func TestCreate_NestedDirectoryCreation(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://deep/nested/path/creds"
	err = repo.Create(ctx, uri, passwordCreds("u", "p"))
	require.NoError(t, err)

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "u", got.Password().Username())
}

// --- Get ---

// Verifies Get returns stored password credentials for an existing URI.
func TestGet_Success(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("alice", "wonderland")))

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	require.NotNil(t, got.Password())
	assert.Equal(t, "alice", got.Password().Username())
	assert.Equal(t, "wonderland", got.Password().Password().Reveal())
}

// Verifies Get returns ErrNotFound when the credential file is absent.
func TestGet_NotFound(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	_, err = repo.Get(context.Background(), "file://nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies Get surfaces parse errors for invalid JSON without mapping them to ErrNotFound.
func TestGet_CorruptedJSON(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)

	p := filepath.Join(dir, "corrupt"+FileExtension)
	require.NoError(t, os.WriteFile(p, []byte("{invalid json"), 0600))

	_, err = repo.Get(context.Background(), "file://corrupt")
	require.Error(t, err)
	assert.False(t, errors.Is(err, shared.ErrNotFound))
}

// --- Update ---

// Verifies Update replaces stored credentials when the expected version matches.
func TestUpdate_Success(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("v1user", "v1pass")))

	err = repo.Update(ctx, uri, passwordCreds("v2user", "v2pass"), 1)
	require.NoError(t, err)

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "v2user", got.Password().Username())
	assert.Equal(t, "v2pass", got.Password().Password().Reveal())
}

// Verifies Update bumps the persisted version in the on-disk JSON.
func TestUpdate_VersionIncremented(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	require.NoError(t, repo.Update(ctx, uri, passwordCreds("u2", "p2"), 1))

	p, err := repo.uriToPath(uri)
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)

	var stored storedCredentials
	require.NoError(t, json.Unmarshal(data, &stored))
	assert.Equal(t, int64(2), stored.Version)
}

// Verifies Update returns ErrNotFound for a missing credential file.
func TestUpdate_NotFound(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	err = repo.Update(context.Background(), "file://ghost", passwordCreds("u", "p"), 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies Update returns ErrVersionMismatch when the supplied version is wrong.
func TestUpdate_VersionMismatch(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	err = repo.Update(ctx, uri, passwordCreds("u2", "p2"), 99)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrVersionMismatch))
}

// Verifies Update with version zero skips optimistic locking and applies the change.
func TestUpdate_NoVersionCheck(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	err = repo.Update(ctx, uri, passwordCreds("u2", "p2"), 0)
	require.NoError(t, err)

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "u2", got.Password().Username())
}

// --- Delete ---

// Verifies Delete removes the file when the version matches and Get then returns ErrNotFound.
func TestDelete_Success(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	err = repo.Delete(ctx, uri, 1)
	require.NoError(t, err)

	_, err = repo.Get(ctx, uri)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies Delete returns ErrNotFound when the credential file does not exist.
func TestDelete_NotFound(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	err = repo.Delete(context.Background(), "file://ghost", 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies Delete returns ErrVersionMismatch when the supplied version is wrong.
func TestDelete_VersionMismatch(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	err = repo.Delete(ctx, uri, 42)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrVersionMismatch))
}

// Verifies Delete with version zero skips optimistic locking and removes the file.
func TestDelete_NoVersionCheck(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	err = repo.Delete(ctx, uri, 0)
	require.NoError(t, err)

	_, err = repo.Get(ctx, uri)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// --- List ---

// Verifies List returns no URIs for an empty repository.
func TestList_EmptyRepo(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	uris, err := repo.List(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, uris)
}

// Verifies List returns all stored URIs when the prefix is empty.
func TestList_WithFiles(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, "file://broker/a", passwordCreds("u", "p")))
	require.NoError(t, repo.Create(ctx, "file://broker/b", passwordCreds("u", "p")))
	require.NoError(t, repo.Create(ctx, "file://other", passwordCreds("u", "p")))

	uris, err := repo.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, uris, 3)
	assert.Contains(t, uris, "file://broker/a")
	assert.Contains(t, uris, "file://broker/b")
	assert.Contains(t, uris, "file://other")
}

// Verifies List filters URIs by hierarchical prefix.
func TestList_PrefixFiltering(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, "file://broker/mqtt/prod", passwordCreds("u", "p")))
	require.NoError(t, repo.Create(ctx, "file://broker/mqtt/dev", passwordCreds("u", "p")))
	require.NoError(t, repo.Create(ctx, "file://broker/sqs/prod", passwordCreds("u", "p")))
	require.NoError(t, repo.Create(ctx, "file://database/pg", passwordCreds("u", "p")))

	uris, err := repo.List(ctx, "broker/mqtt")
	require.NoError(t, err)
	assert.Len(t, uris, 2)
	for _, u := range uris {
		assert.Contains(t, u, "broker/mqtt")
	}
}

// Verifies List returns an empty result when no URIs match the prefix.
func TestList_NonexistentPrefix(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, "file://broker/a", passwordCreds("u", "p")))

	uris, err := repo.List(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, uris)
}

// --- Version / Timestamp ---

// Verifies sequential updates with version zero monotonically increase the stored version.
func TestVersion_IncrementsOnUpdate(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	p, err := repo.uriToPath(uri)
	require.NoError(t, err)

	readVersion := func() int64 {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		var s storedCredentials
		require.NoError(t, json.Unmarshal(data, &s))
		return s.Version
	}

	assert.Equal(t, int64(1), readVersion())

	// version=0 skips check, so we can chain updates easily
	require.NoError(t, repo.Update(ctx, uri, passwordCreds("u2", "p2"), 0))
	assert.Equal(t, int64(2), readVersion())

	require.NoError(t, repo.Update(ctx, uri, passwordCreds("u3", "p3"), 0))
	assert.Equal(t, int64(3), readVersion())
}

// Verifies CreatedAt is preserved while UpdatedAt advances across updates.
func TestCreatedAt_PreservedAcrossUpdates(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(fixed)
	repo, err := New(dir, WithClock(clk))
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://mybroker"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))

	p, err := repo.uriToPath(uri)
	require.NoError(t, err)

	readStored := func() storedCredentials {
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		var s storedCredentials
		require.NoError(t, json.Unmarshal(data, &s))
		return s
	}

	original := readStored()
	clk.Advance(10 * time.Millisecond)

	require.NoError(t, repo.Update(ctx, uri, passwordCreds("u2", "p2"), 0))

	updated := readStored()

	assert.Equal(t, fixed, original.CreatedAt)
	assert.Equal(t, fixed, original.UpdatedAt)
	assert.Equal(t, original.CreatedAt, updated.CreatedAt)
	assert.Equal(t, fixed.Add(10*time.Millisecond), updated.UpdatedAt)
}

// --- Concurrency ---

// Verifies concurrent Get calls succeed for the same URI.
func TestConcurrent_Reads(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://shared"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("reader", "pass")))

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			got, err := repo.Get(ctx, uri)
			assert.NoError(t, err)
			if got != nil && got.Password() != nil {
				assert.Equal(t, "reader", got.Password().Username())
			}
		}()
	}
	wg.Wait()
}

// Verifies concurrent Update calls with version zero all succeed and leave consistent state.
func TestConcurrent_Writes(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://shared"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("initial", "pass")))

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = repo.Update(ctx, uri, passwordCreds("writer", "pass"), 0)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		assert.NoError(t, e, "goroutine %d failed", i)
	}

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, "writer", got.Password().Username())
}

// --- TLS material round-trip ---

// Verifies TLS PEM material round-trips through Create and Get.
func TestTLSMaterial_RoundTrip(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://tls-broker"
	want := tlsCreds()
	require.NoError(t, repo.Create(ctx, uri, want))

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	require.NotNil(t, got.TLS())
	assert.Nil(t, got.Password())

	assert.Equal(t, want.TLS().CertPEM(), got.TLS().CertPEM())
	assert.Equal(t, want.TLS().KeyPEM().Reveal(), got.TLS().KeyPEM().Reveal())
	assert.Equal(t, want.TLS().CAPEMs(), got.TLS().CAPEMs())
	assert.Equal(t, want.TLS().InsecureSkipVerify(), got.TLS().InsecureSkipVerify())
}

// --- Combined credential round-trip ---

// Verifies password and TLS fields round-trip together through Create and Get.
func TestCombinedCredentials_RoundTrip(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://combined"
	want := combinedCreds()
	require.NoError(t, repo.Create(ctx, uri, want))

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)

	require.NotNil(t, got.Password())
	assert.Equal(t, want.Password().Username(), got.Password().Username())
	assert.Equal(t, want.Password().Password().Reveal(), got.Password().Password().Reveal())

	require.NotNil(t, got.TLS())
	assert.Equal(t, want.TLS().CertPEM(), got.TLS().CertPEM())
	assert.Equal(t, want.TLS().KeyPEM().Reveal(), got.TLS().KeyPEM().Reveal())
	assert.Equal(t, want.TLS().CAPEMs(), got.TLS().CAPEMs())
	assert.Equal(t, want.TLS().InsecureSkipVerify(), got.TLS().InsecureSkipVerify())
}
