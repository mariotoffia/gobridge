package httpapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// TestConfigTxnCommit_ConcurrentCommitDuringApply_NoClobber pins the fix for the
// rollback-clobber window. commitDurable clears the active transaction and
// releases the manager lock BEFORE the (slow) apply, so a second Begin+Commit
// could otherwise land a durable write inside the first commit's apply window;
// if that first apply then failed, its rollback would clobber the second
// commit's newer on-disk version — silent loss of an acknowledged commit.
//
// commitMu serializes the whole commit pipeline so the second commit cannot
// write until the first has fully applied or rolled back. Here the first
// commit's apply fails and rolls back to v7, so the second commit observes a
// version conflict against its now-stale base (8) and writes nothing — disk
// stays at the rolled-back v7 instead of a clobbered-away newer version.
func TestConfigTxnCommit_ConcurrentCommitDuringApply_NoClobber(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 7

	store := &recordingConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	firstApplyErr := errors.New("txn1 runtime build failed")
	var applyCalls atomic.Int32
	applier := func(_ context.Context, _ *ports.BridgeConfig) error {
		if applyCalls.Add(1) == 1 {
			close(applyStarted) // txn1's durable write (v8) is done; apply in-flight, m.active cleared
			<-releaseApply      // hold the apply window open until the test releases it
			return firstApplyErr
		}
		return nil // only reachable if commitMu is absent (the bug): txn2's apply
	}

	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good }, applier, nil, clk)
	ctx := context.Background()

	// txn1 begins at base version 7.
	txn1, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	var (
		wg     sync.WaitGroup
		v1, v2 int
		err1   error
		err2   error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		v1, err1 = mgr.Commit(ctx, txn1.ID)
	}()

	<-applyStarted // txn1 wrote v8 durably and is now blocked in apply; m.active == nil

	// txn2 begins now and reads disk v8 as its base, then commits. With commitMu
	// its Commit blocks until txn1 finishes; without it, txn2 would write v9 into
	// the apply window and txn1's rollback would clobber it back to v7.
	txn2, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)
	wg.Add(1)
	go func() {
		defer wg.Done()
		v2, err2 = mgr.Commit(ctx, txn2.ID)
	}()

	close(releaseApply) // let txn1's apply fail and roll back to v7
	wg.Wait()

	// txn1 rolled back to the previous good version.
	assert.ErrorIs(t, err1, errConfigRolledBack)
	assert.ErrorIs(t, err1, firstApplyErr)
	assert.Equal(t, 7, v1, "txn1 reports the restored (previous) version")

	// txn2 must NOT have silently committed on top of the rolled-back txn1; its
	// stale base (8) no longer matches disk (7), so it conflicts and writes
	// nothing.
	assert.ErrorIs(t, err2, errVersionConflict)
	assert.Equal(t, 0, v2)

	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 7, onDisk.Version, "disk must remain at the rolled-back previous version, not a clobbered newer one")
}
