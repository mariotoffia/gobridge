package outbox

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/stretchr/testify/require"
)

func TestFencePreventsClaimsIncludingFinalDrain(t *testing.T) {
	// No backing store: reaching Claim would panic instead of silently passing.
	d := &Drainer{hasDrained: true, tokenFn: func() (persistence.LeaseToken, bool) { return persistence.LeaseToken{}, true }}
	d.Fence()
	n, _, err := d.drainBatch(t.Context(), persistence.LeaseToken{})
	require.Zero(t, n)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, d.finalDrain(t.Context()), context.Canceled)
}
