package dockerexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDockerexec_Run_Succeeds(t *testing.T) {
	if _, err := os.Stat("testdata/docker"); err != nil {
		t.Fatal(err)
	}
	withTestDockerPath(t)

	out, err := Run(time.Second, "version")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(out) != "fake docker version\n" {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestDockerexec_Run_TimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("testdata/docker is a POSIX shell script")
	}
	withTestDockerPath(t)

	_, err := Run(100*time.Millisecond, "sleep")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func withTestDockerPath(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Join(wd, "testdata")+string(os.PathListSeparator)+oldPath)
}
