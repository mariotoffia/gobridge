//go:build !race

package gobridgealbattachment_test

import (
	"os"
	"testing"

	"github.com/aws/jsii-runtime-go"
)

// TestMain closes the jsii kernel once for the whole test binary.
//
// Closing per test is a race. Close() shuts the child's stdin, the Node
// runtime exits, and that wakes the background cmd.Wait() jsii keeps on the
// child — which then reaps the same descriptors Close() has already freed,
// outside jsii's own mutex. Go 1.26's os/exec dereferences that freed pipe
// state and the binary dies with SIGSEGV, so a per-test Close rolls the dice
// once per test. One close per binary leaves a single, final teardown.
//
// Upstream still has this shape as of jsii-runtime-go v1.140.0, so the fix
// belongs here rather than in a dependency bump.
func TestMain(m *testing.M) {
	code := m.Run()
	jsii.Close()
	os.Exit(code)
}
