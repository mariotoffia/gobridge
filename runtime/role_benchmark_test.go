package runtime_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Role is read on every bare /ready probe and every deep-health snapshot, so its
// cost is the cost of a load-balancer poll. With no exclusive session it is one
// lock and one classification; the benchmark pins that a probe never pays more.
func BenchmarkRuntime_Role_Standalone(b *testing.B) {
	rt := goruntime.New(goruntime.WithInstanceID("bench"))
	for b.Loop() {
		if rt.Role() != ports.RoleStandalone {
			b.Fatal("unexpected role")
		}
	}
}
