package file

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// fakeDirInfo is an os.FileInfo for a directory with a configurable permission
// bit set, used to drive tightenDirPermissions without a real loose directory.
type fakeDirInfo struct{ perm os.FileMode }

func (f fakeDirInfo) Name() string       { return "creds" }
func (f fakeDirInfo) Size() int64        { return 0 }
func (f fakeDirInfo) Mode() os.FileMode  { return os.ModeDir | f.perm }
func (f fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (f fakeDirInfo) IsDir() bool        { return true }
func (f fakeDirInfo) Sys() any           { return nil }

// withFS is a white-box test option injecting filesystem seams.
func withFS(
	mkdirAll func(string, os.FileMode) error,
	stat func(string) (os.FileInfo, error),
	chmod func(string, os.FileMode) error,
) Option {
	return func(r *Repository) {
		if mkdirAll != nil {
			r.mkdirAll = mkdirAll
		}
		if stat != nil {
			r.stat = stat
		}
		if chmod != nil {
			r.chmod = chmod
		}
	}
}

// TestGet_NilCredentialsEnvelopeReturnsError is the regression test: an
// envelope with no credentials — {"version":1} (key absent) or
// {"credentials":null} — must return an error, never (nil, nil), so the
// transport does not connect anonymously and the poller does not skip it.
func TestGet_NilCredentialsEnvelopeReturnsError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"version only, credentials key absent", `{"version":1}`},
		{"credentials explicitly null", `{"credentials":null,"version":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			repo, err := New(dir)
			require.NoError(t, err)

			uri := "file://broker"
			fp, err := repo.uriToPath(uri)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o700))
			require.NoError(t, os.WriteFile(fp, []byte(tc.body), 0o600))

			creds, err := repo.Get(context.Background(), uri)
			require.Nil(t, creds)
			require.Error(t, err, "an envelope without credentials must error, not return (nil, nil)")
			require.ErrorIs(t, err, shared.ErrInvalidPayload)
		})
	}
}

// TestNew_ReadOnlyMount_DirExists_NoCrash is an regression test: when the
// base directory cannot be created because the mount is read-only but the
// directory already exists (an operator-provisioned K8s Secret volume), New
// must succeed rather than crash-loop the pod.
func TestNew_ReadOnlyMount_DirExists_NoCrash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // exists; default os.Stat sees it
	repo, err := New(dir, withFS(
		func(string, os.FileMode) error { return syscall.EROFS }, // MkdirAll fails (read-only)
		nil, // real os.Stat: the dir exists
		nil, // real os.Chmod: we own the TempDir
	))
	require.NoError(t, err, "a pre-existing read-only mount must not crash New")
	require.NotNil(t, repo)
}

// TestNew_ReadOnlyMount_ChmodEROFS_WarnsAndContinues is an regression test:
// a loose-permission directory whose chmod fails with EROFS/EPERM (operator-
// controlled mount) must be tolerated with a WARN, not a hard error.
func TestNew_ReadOnlyMount_ChmodEROFS_WarnsAndContinues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	repo, err := New(dir,
		WithLogger(logger),
		withFS(
			nil, // real MkdirAll (idempotent on existing dir)
			func(string) (os.FileInfo, error) { return fakeDirInfo{perm: 0o755}, nil }, // looks loose
			func(string, os.FileMode) error { return syscall.EROFS },                   // cannot chmod
		),
	)
	require.NoError(t, err, "a read-only mount that cannot be tightened must be tolerated")
	require.NotNil(t, repo)
	require.Contains(t, logs.String(), "cannot tighten base directory permissions")
}

// TestNew_LooseDir_ChmodUnexpectedError_HardFails verifies the relaxation is
// scoped: a chmod failure that is NOT a read-only/permission condition is still
// a hard error (a real, unexpected problem must surface).
func TestNew_LooseDir_ChmodUnexpectedError_HardFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repo, err := New(dir, withFS(
		nil,
		func(string) (os.FileInfo, error) { return fakeDirInfo{perm: 0o755}, nil },
		func(string, os.FileMode) error { return errors.New("unexpected io failure") },
	))
	require.Error(t, err)
	require.Nil(t, repo)
	require.Contains(t, err.Error(), "restrict base path permissions")
}

// TestNew_MkdirFails_DirMissing_HardFails verifies New still fails when the
// directory genuinely cannot be created and does not already exist.
func TestNew_MkdirFails_DirMissing_HardFails(t *testing.T) {
	t.Parallel()

	repo, err := New("/nonexistent/creds", withFS(
		func(string, os.FileMode) error { return syscall.EROFS },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		nil,
	))
	require.Error(t, err)
	require.Nil(t, repo)
	require.Contains(t, err.Error(), "failed to create base path")
}

// TestGet_WorldReadableFileWarnsOnce is the LOW regression test: reading a
// group/world readable credential file (e.g. a Secret mounted 0644) emits a
// single WARN and never fails.
func TestGet_WorldReadableFileWarnsOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo, err := New(dir, WithLogger(logger))
	require.NoError(t, err)

	uri := "file://broker"
	fp, err := repo.uriToPath(uri)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o700))
	require.NoError(t, os.WriteFile(fp, []byte(`{"credentials":{"Password":{"Username":"u","Password":"p"}},"version":1}`), 0o600))
	require.NoError(t, os.Chmod(fp, 0o644)) // force group/world readable

	for range 3 {
		_, err = repo.Get(context.Background(), uri)
		require.NoError(t, err, "a world-readable file must still be read, not rejected")
	}
	require.Equal(t, 1, strings.Count(logs.String(), "group/world accessible"),
		"the world-readable WARN must fire exactly once")
}

// TestGet_ErrorPathsNeverLeakSecret is the leak-regression test: a credential
// file whose bytes contain a secret but is otherwise malformed must never echo
// that secret into the returned error or any log line.
func TestGet_ErrorPathsNeverLeakSecret(t *testing.T) {
	t.Parallel()

	const secret = "SUPERSECRETVALUE"
	dir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo, err := New(dir, WithLogger(logger))
	require.NoError(t, err)

	uri := "file://broker"
	fp, err := repo.uriToPath(uri)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o700))
	// Malformed JSON that embeds the secret (truncated object).
	require.NoError(t, os.WriteFile(fp, []byte(`{"credentials":{"Password":{"Password":"`+secret), 0o600))

	_, err = repo.Get(context.Background(), uri)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret, "the parse error must not echo credential file bytes")
	require.NotContains(t, logs.String(), secret, "no log line may contain the secret")
}
