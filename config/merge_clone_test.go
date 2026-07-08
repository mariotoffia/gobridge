package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestDefaultMerge_ClonesHTTPWhenOverlayOmitsIt covers the Chunk-1 finding that
// DefaultMerge cloned Cluster but left HTTP aliasing the base layer when the
// overlay carried no http block. A consumer mutating merged.HTTP could then
// reach back into and poison the cached base config.
func TestDefaultMerge_ClonesHTTPWhenOverlayOmitsIt(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP:   &ports.HTTPConfig{AdminAddr: ":8080"},
	}
	overlay := &ports.BridgeConfig{} // no HTTP block

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.HTTP)
	require.NotSame(t, base.HTTP, merged.HTTP, "merged.HTTP must not alias base.HTTP")

	merged.HTTP.AdminAddr = ":9999"
	assert.Equal(t, ":8080", base.HTTP.AdminAddr, "mutating merged.HTTP must not affect base")
}

// TestDefaultMerge_ClonesStoresWhenOverlayOmitsThem covers the same finding for
// the per-role store pointers (lease/outbox/dlq), which likewise aliased base.
func TestDefaultMerge_ClonesStoresWhenOverlayOmitsThem(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "dynamodb"},
			Outbox: &ports.StoreConfig{Type: "dynamodb"},
			DLQ:    &ports.StoreConfig{Type: "sqs"},
		},
	}
	overlay := &ports.BridgeConfig{} // no stores block

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.Stores.Lease)
	require.NotSame(t, base.Stores.Lease, merged.Stores.Lease, "merged lease must not alias base")
	require.NotSame(t, base.Stores.Outbox, merged.Stores.Outbox, "merged outbox must not alias base")
	require.NotSame(t, base.Stores.DLQ, merged.Stores.DLQ, "merged dlq must not alias base")

	merged.Stores.Lease.Type = "poisoned"
	merged.Stores.Outbox.Type = "poisoned"
	merged.Stores.DLQ.Type = "poisoned"
	assert.Equal(t, "dynamodb", base.Stores.Lease.Type, "mutating merged lease must not affect base")
	assert.Equal(t, "dynamodb", base.Stores.Outbox.Type, "mutating merged outbox must not affect base")
	assert.Equal(t, "sqs", base.Stores.DLQ.Type, "mutating merged dlq must not affect base")
}
