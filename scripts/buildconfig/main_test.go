package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteOverlay_PreservesConfigAndSource(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "command")
	out := filepath.Join(dir, "build")
	if err := os.Mkdir(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	placeholder := filepath.Join(pkg, "initial-config.base64")
	if err := os.WriteFile(placeholder, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "config with spaces.yaml")
	input := []byte("bridge: {id: test}\n" + strings.Repeat("# padding\n", 20000))
	if err := os.WriteFile(source, input, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOverlay(source, pkg, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "overlay.json"))
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct{ Replace map[string]string }
	if err := json.Unmarshal(data, &overlay); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(overlay.Replace[placeholder])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != base64.StdEncoding.EncodeToString(input) {
		t.Fatal("embedded data differs from input")
	}
	data, err = os.ReadFile(placeholder)
	if err != nil || string(data) != "original" {
		t.Fatalf("source placeholder changed: %v", err)
	}
}

func TestWriteOverlay_RequiresSupportedCommand(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "config")
	if err := os.WriteFile(source, []byte("bridge: {id: test}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeOverlay(source, dir, t.TempDir()); err == nil {
		t.Fatal("accepted a command without an embedded-config file")
	}
}

func TestWriteOverlay_RejectsOutputCollisions(t *testing.T) {
	for _, kind := range []string{"payload-symlink", "payload-hardlink", "overlay-symlink", "input-is-payload"} {
		t.Run(kind, func(t *testing.T) {
			pkg, out := t.TempDir(), t.TempDir()
			placeholder := filepath.Join(pkg, "initial-config.base64")
			if err := os.WriteFile(placeholder, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(pkg, "config.yaml")
			if err := os.WriteFile(source, []byte("bridge: {id: test}"), 0o600); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(out, "initial-config.base64")
			var err error
			switch kind {
			case "payload-symlink":
				err = os.Symlink(placeholder, payload)
			case "payload-hardlink":
				err = os.Link(placeholder, payload)
			case "overlay-symlink":
				err = os.Symlink(placeholder, filepath.Join(out, "overlay.json"))
			case "input-is-payload":
				source = payload
				err = os.WriteFile(source, []byte("bridge: {id: test}"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := writeOverlay(source, pkg, out); err == nil {
				t.Fatal("accepted a pre-existing output")
			}
			data, err := os.ReadFile(placeholder)
			if err != nil || string(data) != "original" {
				t.Fatalf("changed source placeholder: %v", err)
			}
			data, err = os.ReadFile(source)
			if err != nil || string(data) != "bridge: {id: test}" {
				t.Fatalf("changed input document: %v", err)
			}
		})
	}
}

func TestMakeBuild_RunsHelperForHostAndBinaryForTarget(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.work"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	targetOS, targetArch := "linux", "amd64"
	if runtime.GOOS == targetOS {
		targetOS = "windows"
	}
	if runtime.GOARCH == targetArch {
		targetArch = "arm64"
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  env)
    case "$2" in GOHOSTOS) echo %s;; GOHOSTARCH) echo %s;; *) exit 24;; esac;;
  run)
    test "$GOOS" = %s
    test "$GOARCH" = %s
    echo helper >> "$CALLS";;
  -C)
    test "$GOOS" = %s
    test "$GOARCH" = %s
    echo binary >> "$CALLS";;
  *) exit 25;;
esac
`, runtime.GOOS, runtime.GOARCH, runtime.GOOS, runtime.GOARCH, targetOS, targetArch)
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	cmd := exec.CommandContext(t.Context(), "make", "--no-print-directory", "-f", filepath.Join(root, "Makefile"),
		"build-gobridge", "GIT_SHA=test", "IMAGE_TAG=dev")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GOOS="+targetOS, "GOARCH="+targetArch, "CALLS="+calls)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-build: %v\n%s", err, output)
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "helper\nbinary\n" {
		t.Fatalf("wrong build sequence: %v", err)
	}
}
