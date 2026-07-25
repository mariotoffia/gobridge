package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestSwapEvent_PausedDeferralNamesItsReason proves a deferred swap says WHY it
// was deferred.
//
// Deferred was single-cause until now (an admin StopBridge), so in-band appliers
// hardcode "bridge paused by admin" for every deferred event
// (cmd/gobridge/reload.go). The coordinated rollout barrier introduces a SECOND
// cause — the config is committed locally but pending the all-member commit — and
// rendering that as "paused by admin" would send an operator to the wrong runbook.
// Carrying the reason on the event keeps the two distinguishable at the source
// instead of forcing consumers to guess.
func TestSwapEvent_PausedDeferralNamesItsReason(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newBacklogSupervisor(7, WithOnSwap(onSwap), WithAllowDestructiveReload(true))

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	require.NoError(t, s.StopBridge(context.Background()))
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))

	ev := awaitSwap(t, swaps)
	require.True(t, ev.Deferred, "a paused reload is deferred (committed-not-applied)")
	assert.Equal(t, DeferReasonPaused, ev.DeferReason,
		"an admin-pause deferral must name the pause so a consumer need not assume the cause")
}
