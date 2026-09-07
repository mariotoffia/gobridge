package httpapi

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// TestConfigTxnFileStoreVersions verifies one increment per commit or compensating write.
func TestConfigTxnFileStoreVersions(t *testing.T) {
	for _, failApply := range []bool{false, true} {
		name := "commit"
		if failApply {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			store := &parser.FileStore{
				Path: filepath.Join(t.TempDir(), "config.yaml"), Registry: newTestRegistry(t),
			}
			cfg := sampleBridgeConfig()
			cfg.Version = 7
			cfg.Bridge.LogLevel = "info"
			require.NoError(t, parser.WriteFile(store.Path, cfg))
			applyErr := errors.New("apply rejected")
			mgr := newTxnManager(store, func() *ports.BridgeConfig { return cfg },
				func(_ context.Context, applied *ports.BridgeConfig) error {
					assert.Equal(t, 8, applied.Version)
					if failApply {
						return applyErr
					}
					return nil
				}, nil, clocktest.New())
			txn, err := mgr.Begin(t.Context(), 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = mgr.Rollback(txn.ID) })
			_, _, err = mgr.Patch(t.Context(), txn.ID, &ports.BridgeConfig{
				Bridge: ports.BridgeSettings{LogLevel: "debug"},
			})
			require.NoError(t, err)

			version, err := mgr.Commit(t.Context(), txn.ID)
			wantVersion, wantLevel := 8, "debug"
			if failApply {
				require.ErrorIs(t, err, errConfigRolledBack)
				assert.ErrorIs(t, err, applyErr)
				wantVersion, wantLevel = 9, "info"
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, wantVersion, version)
			got, err := store.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, wantVersion, got.Version)
			assert.Equal(t, wantLevel, got.Bridge.LogLevel)
		})
	}
}
