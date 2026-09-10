package configstoretest

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Run exercises the ConfigStore contract against a fresh store per case.
// Conditional saves are checked when the store implements ConditionalConfigStore.
func Run(t *testing.T, newStore func(t *testing.T) ports.ConfigStore) {
	t.Helper()

	t.Run("LoadMissing", func(t *testing.T) {
		got, err := newStore(t).Load(t.Context())
		assert.Nil(t, got)
		assert.True(t, errors.Is(err, shared.ErrNotFound) || errors.Is(err, fs.ErrNotExist),
			"missing config must return ErrNotFound or fs.ErrNotExist, got %v", err)
	})
	t.Run("SaveRoundTrip", func(t *testing.T) {
		store := newStore(t)
		cfg := blueprint()
		require.NoError(t, store.Save(t.Context(), cfg))
		assert.Equal(t, 1, cfg.Version)

		got, err := store.Load(t.Context())
		require.NoError(t, err)
		assert.Equal(t, cfg, got)
	})
	t.Run("SaveAdvancesStoredVersion", func(t *testing.T) {
		store := newStore(t)
		for i, suppliedVersion := range []int{0, 99, 0} {
			cfg := blueprint()
			cfg.Version = suppliedVersion
			require.NoError(t, store.Save(t.Context(), cfg))
			got, err := store.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, i+1, got.Version, "the store owns version assignment")
			assert.Equal(t, got.Version, cfg.Version, "the caller sees the committed version")
		}
	})
	t.Run("SaveRejectsNil", func(t *testing.T) {
		assert.ErrorIs(t, newStore(t).Save(t.Context(), nil), shared.ErrInvalidConfig)
	})
	t.Run("ValidateWarnings", func(t *testing.T) {
		store := newStore(t)
		cfg := blueprint()
		cfg.Receivers = []ports.ReceiverDef{{ID: "input", Transport: "http"}}
		cfg.Senders = []ports.SenderDef{{ID: "output", Transport: "http"}}
		cfg.Bindings = []ports.BindingDef{{ID: "destination", SenderID: "output", Address: "http://localhost/messages"}}
		cfg.Routes = []ports.RouteDef{{ID: "route", ReceiverID: "input", Bindings: []string{"destination"}, DeliveryMode: "direct_hold"}}
		want, err := config.ValidateWithWarnings(cfg)
		require.NoError(t, err)
		require.NotEmpty(t, want, "the fixture must exercise an advisory warning")

		warnings, err := store.Validate(t.Context(), cfg)
		require.NoError(t, err)
		assert.Equal(t, want, warnings)

		cfg.Bridge.ID = ""
		warnings, err = store.Validate(t.Context(), cfg)
		var validationErr *ports.BlueprintValidationError
		require.ErrorAs(t, err, &validationErr)
		assert.NotEmpty(t, validationErr.Errors)
		assert.Equal(t, want, warnings, "warnings survive hard validation errors")
	})
	t.Run("MergeOverlay", func(t *testing.T) {
		store := newStore(t)
		base := blueprint()
		overlay := &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{LogLevel: "debug"},
			HTTP:   &ports.HTTPConfig{AdminAddr: ":9090"},
		}
		originalBase, originalOverlay := blueprint(), *overlay
		originalHTTP := *overlay.HTTP
		want, err := config.DefaultMerge(base, overlay)
		require.NoError(t, err)

		got, err := store.Merge(t.Context(), base, overlay)
		require.NoError(t, err)
		require.Equal(t, want, got)
		assert.Equal(t, originalBase, base)
		assert.Equal(t, originalOverlay, *overlay)
		assert.Equal(t, originalHTTP, *overlay.HTTP)
		assert.Equal(t, base.Bridge.ID, got.Bridge.ID)
		assert.Equal(t, "debug", got.Bridge.LogLevel)
		require.NotNil(t, got.HTTP)
		assert.Equal(t, ":9090", got.HTTP.AdminAddr)
		assert.Equal(t, base.HTTP.MonitorAddr, got.HTTP.MonitorAddr)

		got.HTTP.AdminAddr = ":9191"
		assert.Equal(t, originalBase, base, "merged state must not alias the base")
		assert.Equal(t, originalHTTP, *overlay.HTTP, "merged state must not alias the overlay")
	})
	t.Run("CancelledContext", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cfg := blueprint()

		_, err := store.Load(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		assert.ErrorIs(t, store.Save(ctx, cfg), context.Canceled)
		_, err = store.Validate(ctx, cfg)
		assert.ErrorIs(t, err, context.Canceled)
		_, err = store.Merge(ctx, cfg, blueprint())
		assert.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, cfg.Version, "a cancelled save must not advance the caller's version")
		if cas, ok := store.(ports.ConditionalConfigStore); ok {
			assert.ErrorIs(t, cas.SaveIfVersion(ctx, cfg, 0), context.Canceled)
		}
		_, err = store.Load(t.Context())
		assert.True(t, errors.Is(err, shared.ErrNotFound) || errors.Is(err, fs.ErrNotExist),
			"a cancelled save must not create the config, got %v", err)
	})
	t.Run("Conditional", func(t *testing.T) {
		runConditional(t, newStore)
	})
	t.Run("Initialization", func(t *testing.T) {
		runInitialization(t, newStore)
	})
}

func runConditional(t *testing.T, newStore func(t *testing.T) ports.ConfigStore) {
	t.Helper()
	for _, tc := range []struct {
		name string
		run  func(*testing.T, ports.ConditionalConfigStore)
	}{
		{"CreateIfAbsent", func(t *testing.T, store ports.ConditionalConfigStore) {
			cfg := blueprint()
			cfg.Version = 99
			require.NoError(t, store.SaveIfVersion(t.Context(), cfg, 0))
			assert.Equal(t, 1, cfg.Version)
			got, err := store.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, cfg, got)
			assert.ErrorIs(t, store.SaveIfVersion(t.Context(), blueprint(), 0), shared.ErrVersionMismatch)
		}},
		{"UpdateCurrentVersion", func(t *testing.T, store ports.ConditionalConfigStore) {
			cfg := blueprint()
			require.NoError(t, store.Save(t.Context(), cfg))
			current, err := store.Load(t.Context())
			require.NoError(t, err)
			cfg.Version = 99
			cfg.Bridge.LogLevel = "debug"
			require.NoError(t, store.SaveIfVersion(t.Context(), cfg, current.Version))
			assert.Equal(t, current.Version+1, cfg.Version)
			got, err := store.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, cfg, got)
		}},
		{"RejectStaleVersion", func(t *testing.T, store ports.ConditionalConfigStore) {
			cfg := blueprint()
			require.NoError(t, store.Save(t.Context(), cfg))
			stale, err := store.Load(t.Context())
			require.NoError(t, err)
			cfg.Bridge.LogLevel = "debug"
			require.NoError(t, store.Save(t.Context(), cfg))
			before := *stale

			assert.ErrorIs(t, store.SaveIfVersion(t.Context(), stale, stale.Version), shared.ErrVersionMismatch)
			assert.Equal(t, before, *stale, "a conflict must not change the caller's version")
			got, err := store.Load(t.Context())
			require.NoError(t, err)
			assert.Equal(t, cfg, got, "a conflict must leave the committed document untouched")
		}},
		{"RejectMissingNonzeroVersion", func(t *testing.T, store ports.ConditionalConfigStore) {
			cfg := blueprint()
			assert.ErrorIs(t, store.SaveIfVersion(t.Context(), cfg, 7), shared.ErrVersionMismatch)
			assert.Zero(t, cfg.Version)
			_, err := store.Load(t.Context())
			assert.True(t, errors.Is(err, shared.ErrNotFound) || errors.Is(err, fs.ErrNotExist),
				"a stale writer must not recreate a missing document, got %v", err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ok := newStore(t).(ports.ConditionalConfigStore)
			if !ok {
				t.Skip("store does not implement ConditionalConfigStore")
			}
			tc.run(t, store)
		})
	}
}

func blueprint() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "config-store",
			ShutdownTimeout: "10s",
			LogLevel:        "info",
		},
		ConfigWatch: &ports.ConfigWatchDef{Mode: "poll", PollInterval: "30s"},
		HTTP:        &ports.HTTPConfig{AdminAddr: ":8080", MonitorAddr: ":8081"},
	}
}
