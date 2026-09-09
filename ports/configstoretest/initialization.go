package configstoretest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func runInitialization(t *testing.T, newStore func(t *testing.T) ports.ConfigStore) {
	t.Helper()
	store := newStore(t)
	initializer, ok := store.(ports.ConfigInitializer)
	if !ok {
		t.Skip("store does not support strict initialization")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	candidate := blueprint()
	candidate.Version = 99
	created, err := initializer.CreateIfAbsent(ctx, candidate)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, created)
	require.Equal(t, 99, candidate.Version)
	_, err = initializer.CreateIfAbsent(t.Context(), nil)
	require.ErrorIs(t, err, shared.ErrInvalidConfig)
	created, err = initializer.CreateIfAbsent(t.Context(), candidate)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, 1, candidate.Version)
	other := blueprint()
	other.Bridge.ID, other.Version = "different-candidate", 777
	created, err = initializer.CreateIfAbsent(t.Context(), other)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, 777, other.Version)
	winner, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, candidate, winner)
}
