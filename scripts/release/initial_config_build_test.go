package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitialConfigBuild_PreservesBytesWithoutCommandLinePayload(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "initial config.yaml")
	target := filepath.Join(dir, "goenv")
	content := "bridge:\n  id: example\n# quotes ' \" and shell text $(false)\n" + strings.Repeat("# padding\n", 20000)
	writeTestFile(t, source, content)
	script := filepath.Join(repositoryRootForTest(t), "scripts", "write-build-goenv.sh")
	cmd := exec.CommandContext(t.Context(), "bash", script, source, target, "dev", "abcdef")
	cmd.Env = append(os.Environ(), "GOENV=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build environment: %v\n%s", err, output)
	}
	if len(output) != 0 {
		t.Fatalf("build helper must not print configuration: %s", output)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := `GOFLAGS="-ldflags=-s -w -X main.version=dev -X main.gitSHA=abcdef -X main.initialConfigBase64=` +
		base64.StdEncoding.EncodeToString([]byte(content)) + "\"\n"
	if string(data) != want {
		t.Fatal("build environment did not preserve the exact initial configuration")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("build environment mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInitialConfigBuild_RejectsUnsafeMetadataBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "initial.yaml")
	target := filepath.Join(dir, "goenv")
	writeTestFile(t, source, "bridge: {id: example}\n")
	script := filepath.Join(repositoryRootForTest(t), "scripts", "write-build-goenv.sh")
	cmd := exec.CommandContext(t.Context(), "bash", script, source, target, "dev\" -extld=bad", "abcdef")
	cmd.Env = append(os.Environ(), "GOENV=off")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("accepted metadata that changes the Go flag syntax: %s", output)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("invalid input changed the output: %v", err)
	}
}

func TestInitialConfigBuild_PreservesExistingGoEnvironment(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "initial.yaml")
	previous := filepath.Join(dir, "previous-goenv")
	target := filepath.Join(dir, "goenv")
	writeTestFile(t, source, "bridge: {id: example}\n")
	writeTestFile(t, previous, "GOPROXY=direct\nGOFLAGS=-trimpath\n")
	script := filepath.Join(repositoryRootForTest(t), "scripts", "write-build-goenv.sh")
	cmd := exec.CommandContext(t.Context(), "bash", script, source, target, "dev", "abcdef")
	cmd.Env = append(os.Environ(), "GOENV="+previous)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build environment: %v\n%s", err, output)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "GOPROXY=direct\n") || strings.Contains(string(data), "GOFLAGS=-trimpath") {
		t.Fatal("other settings must remain while build flags are replaced")
	}
}

func TestInitialConfigBuild_RefusesToOverwriteActiveGoEnvironment(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "initial.yaml")
	active := filepath.Join(dir, "active-goenv")
	writeTestFile(t, source, "bridge: {id: example}\n")
	const original = "GOPROXY=direct\n"
	writeTestFile(t, active, original)
	script := filepath.Join(repositoryRootForTest(t), "scripts", "write-build-goenv.sh")
	cmd := exec.CommandContext(t.Context(), "bash", script, source, active, "dev", "abcdef")
	cmd.Env = append(os.Environ(), "GOENV="+active)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("accepted the active Go environment as build output: %s", output)
	}
	data, err := os.ReadFile(active)
	if err != nil || string(data) != original {
		t.Fatalf("active Go environment was modified: %v", err)
	}
}
