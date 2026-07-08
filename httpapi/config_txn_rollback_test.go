package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingConfigStore is a ConfigStore fake that tracks the on-disk state and
// every Save so a test can assert the exact bytes left on disk after a rollback.
type recordingConfigStore struct {
	mu      sync.Mutex
	current *ports.BridgeConfig
	saves   []*ports.BridgeConfig
}

func (s *recordingConfigStore) Load(_ context.Context) (*ports.BridgeConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, fs.ErrNotExist
	}
	clone := *s.current
	return &clone, nil
}

func (s *recordingConfigStore) Save(_ context.Context, cfg *ports.BridgeConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *cfg
	s.current = &clone
	s.saves = append(s.saves, &clone)
	return nil
}

func (s *recordingConfigStore) Validate(_ context.Context, _ *ports.BridgeConfig) ([]string, error) {
	return nil, nil
}

func (s *recordingConfigStore) Merge(_ context.Context, base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	_ = base
	clone := *overlay
	return &clone, nil
}

var _ ports.ConfigStore = (*recordingConfigStore)(nil)

// TestConfigTxnCommit_RestoresPreviousConfigOnApplyFailure is the focused
// regression for the committed_not_applied restart-bomb: when the in-band apply
// fails, the previous on-disk config must be restored (so a restart recovers)
// and the commit must report rolled_back with the restored version — never
// leave the rejected config on disk.
func TestConfigTxnCommit_RestoresPreviousConfigOnApplyFailure(t *testing.T) {
	good := sampleBridgeConfig()
	good.Version = 7
	good.Bridge.LogLevel = "info"

	store := &recordingConfigStore{current: cloneBridgeConfig(good)}
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	applyErr := errors.New("runtime build failed")
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return good },
		func(context.Context, *ports.BridgeConfig) error { return applyErr }, nil, clk)

	ctx := context.Background()
	txn, err := mgr.Begin(ctx, time.Minute)
	require.NoError(t, err)

	version, err := mgr.Commit(ctx, txn.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errConfigRolledBack)
	assert.ErrorIs(t, err, applyErr)
	assert.Equal(t, 7, version, "rolled_back must report the restored (previous) version")

	// On-disk config must be the previous good config (version 7), NOT the
	// rejected bumped version 8.
	onDisk, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, 7, onDisk.Version, "disk must be restored to the previous version")
	assert.Equal(t, "info", onDisk.Bridge.LogLevel)

	// Two writes happened: the rejected version 8 then the version-7 restore.
	require.GreaterOrEqual(t, len(store.saves), 2)
	assert.Equal(t, 8, store.saves[len(store.saves)-2].Version, "rejected version was written durably first")
	assert.Equal(t, 7, store.saves[len(store.saves)-1].Version, "then rolled back to the previous version")
}

func cloneBridgeConfig(c *ports.BridgeConfig) *ports.BridgeConfig {
	clone := *c
	return &clone
}
