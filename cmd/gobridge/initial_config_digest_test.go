package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestInitialConfigDigestCommand(t *testing.T) {
	for _, tc := range []struct {
		name, contents, encoded string
		invalid                 bool
	}{
		{name: "normal", contents: "bridge:\r\n  id: exact bytes \n"},
		{name: "empty"},
		{name: "not a parsed config", contents: "[this is not valid configuration\x00"},
		{name: "invalid encoding", encoded: base64.StdEncoding.EncodeToString([]byte("private-payload")) + "!", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := tc.encoded
			if !tc.invalid {
				encoded = base64.StdEncoding.EncodeToString([]byte(tc.contents))
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInitialConfigDigestProcess$")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0", "GOBRIDGE_TEST_METADATA_PROCESS=digest", "GOBRIDGE_TEST_METADATA_ENCODING="+encoded)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.Output()
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid encoding succeeded")
				}
				if len(stdout) != 0 {
					t.Fatalf("invalid encoding wrote stdout: %q", stdout)
				}
				if stderr.String() != "invalid embedded initial configuration encoding\n" {
					t.Fatalf("unexpected diagnostic: %q", stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("digest command failed: %v; stderr: %s", err, stderr.String())
			}
			want := fmt.Sprintf("%x\n", sha256.Sum256([]byte(tc.contents)))
			if string(stdout) != want {
				t.Fatalf("digest = %q, want %q", stdout, want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("metadata startup wrote diagnostics: %s", stderr.String())
			}
		})
	}
}

func TestInitialConfigDigestHelp(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInitialConfigDigestProcess$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0", "GOBRIDGE_TEST_METADATA_PROCESS=help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help failed: %v; output: %s", err, out)
	}
	if !strings.Contains(string(out), "-initial-config-digest") {
		t.Fatalf("metadata flag absent from help: %s", out)
	}
}

// Only the isolated test child supplies the linker variable through an
// environment value. Production has no environment initializer fallback.
func TestInitialConfigDigestProcess(t *testing.T) {
	mode := os.Getenv("GOBRIDGE_TEST_METADATA_PROCESS")
	if mode == "" {
		return
	}
	initialConfigBase64 = os.Getenv("GOBRIDGE_TEST_METADATA_ENCODING")
	os.Args = []string{os.Args[0], "-initial-config-digest", "-config", "missing-configuration"}
	if mode == "help" {
		os.Args = []string{os.Args[0], "-help"}
	}
	flag.CommandLine = flag.NewFlagSet("metadata", flag.ExitOnError)
	os.Exit(run())
}
