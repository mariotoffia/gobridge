package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestConfigTxn_Begin_NotFoundStartsAtVersionZero verifies both source sentinels
// start a transaction at version zero without creating a durable config.
func TestConfigTxn_Begin_NotFoundStartsAtVersionZero(t *testing.T) {
	for _, tc := range configNotFoundCases() {
		t.Run(tc.name, func(t *testing.T) {
			mgr, store, _ := newNotFoundTxnManager(t, tc.err, nil)
			txn, err := mgr.Begin(t.Context(), 0)
			require.NoError(t, err)
			assert.Zero(t, txn.baseVersion)
			require.NoError(t, mgr.Rollback(txn.ID))
			assert.Empty(t, store.saves, "discard must not materialize an absent config")
			assert.Nil(t, store.current)
		})
	}
}

// TestConfigTxn_FirstWrite_NotFoundCommitsWithCAS verifies the complete empty
// source flow: Begin(0) -> Preview/Patch(default) -> Commit(CAS 0 -> 1).
// Switching the sentinel after Begin also pins the merge and commit read branches.
func TestConfigTxn_FirstWrite_NotFoundCommitsWithCAS(t *testing.T) {
	for _, tc := range configNotFoundCases() {
		t.Run(tc.name, func(t *testing.T) {
			var applied *ports.BridgeConfig
			mgr, store, base := newNotFoundTxnManager(t, tc.err,
				func(_ context.Context, cfg *ports.BridgeConfig) error { applied = cfg; return nil })
			load := store.load
			store.load = store.casConfigStore.Load // isolate Begin from the remaining missing-source reads
			txn, err := mgr.Begin(t.Context(), 0)
			require.NoError(t, err)
			store.load = load

			preview, _, err := mgr.Preview(t.Context(), txn.ID)
			require.NoError(t, err)
			assert.Equal(t, base, preview)
			assert.NotSame(t, base, preview, "a first-write preview must not alias the running default")
			patch := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: base.Bridge.ID, LogLevel: "debug"}}
			_, _, err = mgr.Patch(t.Context(), txn.ID, patch)
			require.NoError(t, err)
			version, err := mgr.Commit(t.Context(), txn.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, version)
			require.Len(t, store.saves, 1)
			assert.Equal(t, 1, store.current.Version)
			assert.Equal(t, "debug", store.current.Bridge.LogLevel)
			assert.Equal(t, store.current, applied)
			assert.Zero(t, base.Version)
			assert.Equal(t, "info", base.Bridge.LogLevel)
		})
	}
}

// TestConfigTxn_FirstWrite_NotFoundHasNoRollbackTarget verifies a failed first
// apply leaves the committed version intact: no prior config -> no restore write.
func TestConfigTxn_FirstWrite_NotFoundHasNoRollbackTarget(t *testing.T) {
	for _, tc := range configNotFoundCases() {
		t.Run(tc.name, func(t *testing.T) {
			applyErr := shared.ErrInvalidConfig
			mgr, store, base := newNotFoundTxnManager(t, tc.err,
				func(context.Context, *ports.BridgeConfig) error { return applyErr })
			store.load = store.casConfigStore.Load
			txn, err := mgr.Begin(t.Context(), 0)
			require.NoError(t, err)
			// The first Commit load computes the merge; only the second reads the
			// durable prior config. Isolate that branch from Begin/computeMerged.
			loads := 0
			store.load = func(ctx context.Context) (*ports.BridgeConfig, error) {
				loads++
				if loads == 2 {
					return nil, tc.err
				}
				return store.casConfigStore.Load(ctx)
			}
			version, err := mgr.Commit(t.Context(), txn.ID)
			require.ErrorIs(t, err, errConfigApplyFailed)
			assert.ErrorIs(t, err, applyErr)
			assert.NotErrorIs(t, err, errConfigRolledBack)
			assert.Equal(t, 1, version)
			require.Len(t, store.saves, 1, "no durable prior means no compensating write")
			assert.Equal(t, 1, store.current.Version)
			assert.Zero(t, base.Version)
		})
	}
}

// TestConfigTxn_LoadErrorsPropagate verifies every pre-write read fails closed
// for unrelated errors, including wrapped errors and misleading not-found text.
func TestConfigTxn_LoadErrorsPropagate(t *testing.T) {
	for _, phase := range []string{"begin", "preview", "patch", "commit merge", "commit prior"} {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"unavailable", shared.ErrUnavailable},
			{"wrapped unauthorized", fmt.Errorf("load: %w", shared.ErrNotAuthorized)},
			{"permission", fs.ErrPermission},
			{"invalid config", shared.ErrInvalidConfig},
			{"cancelled", context.Canceled},
			{"deadline", context.DeadlineExceeded},
			{"not found text", errors.New("config not found")},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				mgr, store, base := newNotFoundTxnManager(t, fs.ErrNotExist, nil)
				var id string
				if phase != "begin" {
					txn, err := mgr.Begin(t.Context(), 0)
					require.NoError(t, err)
					id = txn.ID
				}
				load := store.load
				loads := 0
				store.load = func(ctx context.Context) (*ports.BridgeConfig, error) {
					loads++
					if phase == "commit prior" && loads == 1 {
						return load(ctx)
					}
					return nil, tc.err
				}
				var err error
				switch phase {
				case "begin":
					_, err = mgr.Begin(t.Context(), 0)
				case "preview":
					_, _, err = mgr.Preview(t.Context(), id)
				case "patch":
					_, _, err = mgr.Patch(t.Context(), id, &ports.BridgeConfig{})
				default:
					_, err = mgr.Commit(t.Context(), id)
				}
				require.ErrorIs(t, err, tc.err)
				assert.Empty(t, store.saves)
				assert.Nil(t, store.current)
				assert.Zero(t, base.Version)
			})
		}
	}
}

func configNotFoundCases() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"file", fs.ErrNotExist},
		{"wrapped file", fmt.Errorf("load: %w", fs.ErrNotExist)},
		{"domain", shared.ErrNotFound},
		{"wrapped domain", fmt.Errorf("load: %w", shared.ErrNotFound)},
	}
}

func newNotFoundTxnManager(t *testing.T, missing error, applier func(context.Context, *ports.BridgeConfig) error) (*configTxnManager, *configLoadStore, *ports.BridgeConfig) {
	t.Helper()
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "bridge-empty", LogLevel: "info"}}
	store := &configLoadStore{casConfigStore: &casConfigStore{}}
	store.load = func(ctx context.Context) (*ports.BridgeConfig, error) {
		cfg, err := store.casConfigStore.Load(ctx)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, missing
		}
		return cfg, err
	}
	mgr := newTxnManager(store, func() *ports.BridgeConfig { return base }, applier, nil, clocktest.NewAt(time.Unix(0, 0)))
	mgr.singleWriter = false // first writes must use CAS, never a plain Save
	t.Cleanup(func() {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		mgr.cleanupLocked()
	})
	return mgr, store, base
}
