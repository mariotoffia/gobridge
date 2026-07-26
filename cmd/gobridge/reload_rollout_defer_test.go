package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// TestApplyCommitted_RolloutPendingDeferralIsNotReportedAsAPause proves the
// in-band applier distinguishes the two deferral causes.
//
// Both a paused bridge and a coordinated-rollout hand-off are
// committed-not-applied (ErrApplyInFlight, never rolled back), so the OUTCOME is
// shared. The cause is not: an admin pause needs a StartBridge to resolve, while
// a rollout-pending delta resolves itself when the cluster barrier commits. The
// pipeline hardcoded "bridge paused" for every deferral, which would send an
// operator chasing a pause that does not exist.
func TestApplyCommitted_RolloutPendingDeferralIsNotReportedAsAPause(t *testing.T) {
	p := newReloadPipeline(ports.NewRegistry(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fileCh := make(chan *ports.BridgeConfig)
	go p.run(ctx, fileCh)

	cfg := testConfig("bridge-demo", 5, "info")
	errCh := make(chan error, 1)
	go func() { errCh <- p.applyCommitted(context.Background(), cfg) }()

	got := receiveConfig(t, p.changes())
	p.onSwap(bridge.SwapEvent{
		NewConfig:   got,
		Deferred:    true,
		DeferReason: bridge.DeferReasonRolloutPending,
	})

	select {
	case err := <-errCh:
		if !errors.Is(err, ports.ErrApplyInFlight) {
			t.Fatalf("a rollout-pending deferral is committed-not-applied (no rollback), got: %v", err)
		}
		if strings.Contains(err.Error(), "paused") {
			t.Fatalf("a rollout-pending deferral must NOT be reported as an admin pause, got: %v", err)
		}
		if !strings.Contains(err.Error(), "rollout") {
			t.Fatalf("a rollout-pending deferral must name the cluster rollout barrier, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyCommitted hung on a deferred swap instead of resolving")
	}
}
