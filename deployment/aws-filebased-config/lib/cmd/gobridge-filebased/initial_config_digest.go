package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// writeInitialConfigDigest is build metadata, not configuration admission. Hash
// the exact embedded bytes without parsing, normalizing, or resolving secrets.
func writeInitialConfigDigest(stdout, stderr io.Writer, encoded string) int {
	contents, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "invalid embedded initial configuration encoding")
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%x\n", sha256.Sum256(contents)); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot write embedded initial configuration digest")
		return 1
	}
	return 0
}
