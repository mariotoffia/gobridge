package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestManager_Watch_PoisonedLayerDoesNotBlockOtherLayers covers the Chunk-1
// finding that a bad layer was cached BEFORE the merged-result validation, so
// every later good update from another layer re-merged against the poison and
// was dropped forever. The fix validates the merged result before caching a
// layer, so a rejected update leaves the previous good value in place.
//
// Scenario: base + overlay A + overlay B (both watched). B emits a poison
// update that makes the merged config invalid; the update must be dropped and
// NOT cached. A then emits a good update, which must apply.
func TestManager_Watch_PoisonedLayerDoesNotBlockOtherLayers(t *testing.T) {
	base := minimalValidConfig("base")

	chA := make(chan *ports.BridgeConfig, 1)
	chB := make(chan *ports.BridgeConfig, 1)

	// poisonMerged is signalled the moment the merge splices B's poison in, so
	// the test can wait for the poison to be processed (and dropped) before
	// pushing A's good update — deterministic ordering without a sleep.
	poisonMerged := make(chan struct{}, 1)

	mergeFn := func(b, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
		merged, err := DefaultMerge(b, overlay)
		if err != nil {
			return nil, err
		}
		if overlay != nil && overlay.Bridge.InstanceID == "POISON" {
			select {
			case poisonMerged <- struct{}{}:
			default:
			}
			merged.Bridge.ID = "" // invalid: bridge.id is required -> Validate fails
		}
		return merged, nil
	}

	mgr := NewManager(
		Layer{Name: "base", Loader: &stubLoader{cfg: base}},
		WithOverlay(Layer{Name: "A", Loader: &stubLoader{cfg: &ports.BridgeConfig{}}, Watcher: &stubWatcher{ch: chA}}),
		WithOverlay(Layer{Name: "B", Loader: &stubLoader{cfg: &ports.BridgeConfig{}}, Watcher: &stubWatcher{ch: chB}}),
		WithMergeFunc(mergeFn),
	)

	_, err := mgr.Load(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := mgr.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(mgr.Stop)

	// B emits a poison update: merged config becomes invalid and is dropped.
	chB <- &ports.BridgeConfig{Bridge: ports.BridgeSettings{InstanceID: "POISON"}}
	select {
	case <-poisonMerged:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for poison layer to be merged")
	}

	// A emits a good update AFTER the poison was processed. Before the fix, B's
	// poison would be cached and re-merged here, dropping A too. With the fix, B
	// was never cached, so A applies cleanly.
	// A real log level: the field is a validated enum, and the base leaves it
	// unset, so seeing it in the output proves A's update applied.
	chA <- &ports.BridgeConfig{Bridge: ports.BridgeSettings{LogLevel: "debug"}}

	select {
	case cfg := <-out:
		require.NotNil(t, cfg)
		assert.Equal(t, "debug", cfg.Bridge.LogLevel, "good update from layer A must apply despite layer B's poison")
		assert.Equal(t, "base", cfg.Bridge.ID, "base id must survive (poison never cached)")
	case <-time.After(2 * time.Second):
		t.Fatal("layer A good update was dropped: poisoned layer B blocked it")
	}

	// The rejected layer stays visible to operators as a degraded signal.
	assert.True(t, mgr.WatchDegraded(), "rejected layer update must surface as degraded")
}
