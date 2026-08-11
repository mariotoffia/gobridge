package file

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestAtomicWrite_ExternalReaderNeverSeesPartialJSON is the regression: a
// process that reads the credential file directly (bypassing the repository
// mutex, as an out-of-process reader or a crash-time observer would) must
// never see a truncated or partially written secret. The pre-fix
// os.WriteFile path truncates the live file in place, so a concurrent reader
// can observe an empty or half-written file and fail to parse it; the atomic
// temp-file + rename path only ever exposes the complete old or complete new
// file.
func TestAtomicWrite_ExternalReaderNeverSeesPartialJSON(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://atomic"
	// A large payload widens the write window that the pre-fix truncate-then-
	// write path would expose to a concurrent reader.
	bigSecret := strings.Repeat("s3cr3t-", 16000)
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("user", bigSecret)))

	filePath, err := repo.uriToPath(uri)
	require.NoError(t, err)

	const iterations = 150
	done := make(chan struct{})
	var readErr error
	var readMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // external reader, no repository lock held
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(filePath)
			if err != nil {
				continue // file transiently absent is not the failure under test
			}
			var stored storedCredentials
			if err := json.Unmarshal(data, &stored); err != nil {
				readMu.Lock()
				if readErr == nil {
					readErr = err
				}
				readMu.Unlock()
				return
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		if err := repo.Update(ctx, uri, passwordCreds("user", bigSecret), 0); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()

	readMu.Lock()
	defer readMu.Unlock()
	if readErr != nil {
		t.Fatalf("external reader observed a partial/corrupt credential file: %v", readErr)
	}
}

// TestAtomicWrite_Perms0600 pins that the atomically written file keeps the
// 0600 secret permission.
func TestAtomicWrite_Perms0600(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://perms"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))
	require.NoError(t, repo.Update(ctx, uri, passwordCreds("u", "p2"), 0))

	filePath, err := repo.uriToPath(uri)
	require.NoError(t, err)
	info, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestAtomicWrite_LeavesNoTempFiles verifies the temp file used for the atomic
// rename is always cleaned up.
func TestAtomicWrite_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)
	ctx := context.Background()

	uri := "file://tmpcheck"
	require.NoError(t, repo.Create(ctx, uri, passwordCreds("u", "p")))
	require.NoError(t, repo.Update(ctx, uri, passwordCreds("u", "p2"), 0))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".credtmp-"),
			"leftover temp file: %s", e.Name())
	}
}

// TestGet_RespectsContextCancellation is the regression: a cancelled
// context must short-circuit before any filesystem work. The pre-fix code
// ignored ctx entirely.
func TestGet_RespectsContextCancellation(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)

	uri := "file://cancelled"
	require.NoError(t, repo.Create(context.Background(), uri, passwordCreds("u", "p")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = repo.Get(ctx, uri)
	require.ErrorIs(t, err, context.Canceled)
}

// TestMutations_RespectContextCancellation covers Create/Update/Delete/List
// for the same cancellation contract.
func TestMutations_RespectContextCancellation(t *testing.T) {
	repo, err := New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, repo.Create(context.Background(), "file://seed", passwordCreds("u", "p")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, repo.Create(ctx, "file://c", passwordCreds("u", "p")), context.Canceled)
	require.ErrorIs(t, repo.Update(ctx, "file://seed", passwordCreds("u", "p2"), 0), context.Canceled)
	require.ErrorIs(t, repo.Delete(ctx, "file://seed", 0), context.Canceled)
	_, listErr := repo.List(ctx, "")
	require.ErrorIs(t, listErr, context.Canceled)
}

// TestGet_CorruptFileReturnsBridgeError is the regression for error
// mapping: a malformed credential file surfaces as a classified BridgeError
// (ErrInvalidPayload) instead of a raw fmt-wrapped error.
func TestGet_CorruptFileReturnsBridgeError(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)

	uri := "file://corrupt"
	filePath, err := repo.uriToPath(uri)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o755))
	require.NoError(t, os.WriteFile(filePath, []byte("{ this is not valid json"), 0o600))

	_, err = repo.Get(context.Background(), uri)
	require.Error(t, err)
	require.ErrorIs(t, err, shared.ErrInvalidPayload)

	var be *shared.BridgeError
	require.True(t, errors.As(err, &be), "expected a *shared.BridgeError, got %T", err)
}

// --- Directory permission hardening (secret names must not be listable) ---

// Verifies New creates the base directory 0700: a group/world listable
// directory leaks the set of credential names even with 0600 files.
func TestNew_BaseDirCreated0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	_, err := New(dir)
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// Verifies New tightens a PRE-EXISTING loose base directory to 0700
// (MkdirAll does not touch the mode of directories that already exist).
func TestNew_TightensPreexistingLooseBaseDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// TempDir parents may carry restrictive umasks; force the loose mode.
	require.NoError(t, os.Chmod(dir, 0o755))

	_, err := New(dir)
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// Verifies Create makes nested credential directories 0700.
func TestCreate_NestedDirs0700(t *testing.T) {
	dir := t.TempDir()
	repo, err := New(dir)
	require.NoError(t, err)

	require.NoError(t, repo.Create(context.Background(), "file://broker/mqtt/prod", passwordCreds("u", "p")))

	info, err := os.Stat(filepath.Join(dir, "broker", "mqtt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// --- Symlink containment (lexical checks alone cannot catch these) ---

// Verifies a symlinked directory inside the base that points OUTSIDE it
// is rejected: the lexical prefix check passes (the path looks like
// base/sub/x) but the real path escapes, so Get/Create must fail.
func TestSymlinkEscapeRejected(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "creds")
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o700))

	repo, err := New(base)
	require.NoError(t, err)

	require.NoError(t, os.Symlink(outside, filepath.Join(base, "sub")))

	_, err = repo.Get(context.Background(), "file://sub/secret")
	require.Error(t, err, "Get through an escaping symlink must be rejected")
	assert.Contains(t, err.Error(), "escapes base directory")

	err = repo.Create(context.Background(), "file://sub/secret", passwordCreds("u", "p"))
	require.Error(t, err, "Create through an escaping symlink must be rejected")
	assert.Contains(t, err.Error(), "escapes base directory")

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "no file may be written outside the base directory")
}

// Verifies a symlink INSIDE the base pointing to another location
// inside the base is still allowed (containment, not a symlink ban).
func TestSymlinkWithinBaseAllowed(t *testing.T) {
	base := t.TempDir()
	repo, err := New(base)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(base, "real"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")))

	require.NoError(t, repo.Create(context.Background(), "file://alias/secret", passwordCreds("u", "p")))

	got, err := repo.Get(context.Background(), "file://real/secret")
	require.NoError(t, err)
	require.NotNil(t, got.Password())
	assert.Equal(t, "u", got.Password().Username())
}
