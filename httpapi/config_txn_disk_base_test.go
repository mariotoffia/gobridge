package httpapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

func bindingIDSet(bindings []ports.BindingDef) map[string]bool {
	ids := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		ids[b.ID] = true
	}
	return ids
}

// TestConfigTxn_ComputeMerged_BasesOnDisk_NotStaleMemory is the content-level
// regression: the CAS reads the disk version (Begin/Commit) but the
// pre-fix computeMerged based the merged CONTENT on the in-memory applied config
// (configProvider). An operator's out-of-band disk edit that the watcher had not
// yet applied therefore passed the version-only CAS and was silently clobbered
// by a commit whose content came from stale memory. computeMerged must now base
// the merge on the same source of truth as the CAS: the on-disk config.
func TestConfigTxn_ComputeMerged_BasesOnDisk_NotStaleMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Disk config: carries a binding that exists ONLY on disk (the operator's
	// out-of-band edit the running config has not caught up with).
	diskCfg := sampleBridgeConfig()
	diskCfg.Version = 1
	diskCfg.Bindings = append(diskCfg.Bindings, ports.BindingDef{
		ID: "disk-only-binding", SenderID: "tx-1", SessionID: "sess-1", Address: "topic/disk",
	})
	require.NoError(t, parser.WriteFile(path, diskCfg))

	// In-memory applied config: same version, STALE content (lacks the disk
	// binding, carries a mem-only binding). The pre-fix code merged on top of
	// this and would have dropped disk-only-binding on commit.
	memCfg := sampleBridgeConfig()
	memCfg.Version = 1
	memCfg.Bindings = append(memCfg.Bindings, ports.BindingDef{
		ID: "mem-only-binding", SenderID: "tx-1", SessionID: "sess-1", Address: "topic/mem",
	})

	store := &parser.FileStore{Path: path, Registry: newTestRegistry(t)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return memCfg }, nil, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, 0)
	require.NoError(t, err)
	defer func() { _ = mgr.Rollback(txn.ID) }()

	preview, _, err := mgr.Preview(ctx, txn.ID)
	require.NoError(t, err)

	ids := bindingIDSet(preview.Bindings)
	assert.True(t, ids["disk-only-binding"], "merged config must reflect the on-disk edit")
	assert.False(t, ids["mem-only-binding"], "merged config must NOT reflect stale in-memory state")
}

// TestConfigTxn_Commit_DiskVersionBump_Conflicts is the version-level guard for
// a concurrent disk edit that bumps the version after the transaction
// baselined must be caught by the commit-time CAS (which reads the disk version)
// rather than silently overwriting the newer file.
func TestConfigTxn_Commit_DiskVersionBump_Conflicts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := sampleBridgeConfig()
	cfg.Version = 1
	require.NoError(t, parser.WriteFile(path, cfg))

	store := &parser.FileStore{Path: path, Registry: newTestRegistry(t)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return cfg }, nil, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, 0)
	require.NoError(t, err)

	// Concurrent disk edit bumps the version after the txn baselined at v1.
	bumped := sampleBridgeConfig()
	bumped.Version = 2
	require.NoError(t, parser.WriteFile(path, bumped))

	_, err = mgr.Commit(ctx, txn.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errVersionConflict)
}
