package runtime_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestRuntime_SatisfiesDrivingPorts is a compile-time guard that
// *runtime.Runtime satisfies the driving-port interfaces declared in
// the ports package. If anyone widens or narrows the runtime API in a
// way that breaks driving adapters, this test fails to compile.
func TestRuntime_SatisfiesDrivingPorts(t *testing.T) {
	t.Parallel()

	var (
		_ ports.RuntimeQuery   = (*runtime.Runtime)(nil)
		_ ports.RuntimeCommand = (*runtime.Runtime)(nil)
		_ ports.Runtime        = (*runtime.Runtime)(nil)
	)
}
