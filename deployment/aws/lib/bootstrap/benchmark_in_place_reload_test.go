package bootstrap

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The App reloading one owner's route of many, in place and by a full
// replacement. The fakes dial nothing, so the numbers leave out the connect,
// TLS and resubscribe cost a full replacement pays for every session and an
// in-place reload pays for the changed unit only.

func BenchmarkAppReload_InPlaceOneOfManyUnits(b *testing.B) {
	benchmarkAppReload(b, false)
}

func BenchmarkAppReload_FullReplacementOfManyUnits(b *testing.B) {
	benchmarkAppReload(b, true)
}

// benchmarkAppReload applies, per iteration, a configuration of 20 owners
// that differs from the running one in the route of owner o0 — and, when
// bridgeWide, in the drain timeout too, which takes the full replacement.
func benchmarkAppReload(b *testing.B, bridgeWide bool) {
	owners := make([]string, 20)
	for i := range owners {
		owners[i] = fmt.Sprintf("o%d", i)
	}
	app := newInPlaceTestApp(b, newTrackedTransportFactory(false), adminKeyResolver())
	require.NoError(b, applyTo(b, app, inPlaceTestConfig(owners...)))
	for i := 0; b.Loop(); i++ {
		next := inPlaceTestConfig(owners...)
		next.Version = i + 2
		if i%2 == 0 {
			withRouteChange(next, "o0", i+2)
			if bridgeWide {
				next.Bridge.DrainTimeout = "2s"
			}
		}
		require.NoError(b, applyTo(b, app, next))
	}
}
