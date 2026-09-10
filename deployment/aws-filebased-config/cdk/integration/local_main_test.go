//go:build integration_local
// +build integration_local

package integration

import (
	"os"
	"testing"
)

// The local backend's containers and the Docker network they share outlive every
// test in the binary: the network can only be removed once the last container
// has left it, which is later than any t.Cleanup can run.
func TestMain(m *testing.M) {
	code := m.Run()
	localShutdown()
	os.Exit(code)
}
