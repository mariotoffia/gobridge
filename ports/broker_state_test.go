package ports_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// carryOverStore is a managed subscription store over a map. An identity that
// is not in the map has no baseline, as the port contract says.
type carryOverStore struct {
	baselines map[string][]string
	listErr   error
}

func (s *carryOverStore) List(_ context.Context, identity string) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	filters, ok := s.baselines[identity]
	if !ok {
		return nil, shared.ErrNotFound
	}
	return slices.Clone(filters), nil
}

func (s *carryOverStore) Remember(_ context.Context, identity string, filters []string) error {
	s.baselines[identity] = append(s.baselines[identity], filters...)
	return nil
}

func (s *carryOverStore) Forget(context.Context, string, []string) error { return nil }

func TestCarryOverManagedSubscriptionHistory_CopiesTheLegacyHistoryWhenTheKeyHasNone(t *testing.T) {
	store := &carryOverStore{baselines: map[string][]string{"legacy": {"orders/#", "$share/g/audit/#"}}}

	require.NoError(t, ports.CarryOverManagedSubscriptionHistory(t.Context(), store, "current", "legacy"))

	assert.Equal(t, []string{"orders/#", "$share/g/audit/#"}, store.baselines["current"])
}

func TestCarryOverManagedSubscriptionHistory_KeepsAHistoryTheKeyAlreadyHas(t *testing.T) {
	store := &carryOverStore{baselines: map[string][]string{
		"current": {"orders/new"},
		"legacy":  {"orders/old"},
	}}

	require.NoError(t, ports.CarryOverManagedSubscriptionHistory(t.Context(), store, "current", "legacy"))

	assert.Equal(t, []string{"orders/new"}, store.baselines["current"])
}

func TestCarryOverManagedSubscriptionHistory_CopiesNothingWithoutALegacyBaseline(t *testing.T) {
	store := &carryOverStore{baselines: map[string][]string{}}

	require.NoError(t, ports.CarryOverManagedSubscriptionHistory(t.Context(), store, "current", "legacy"))

	_, established := store.baselines["current"]
	assert.False(t, established, "no legacy history means the current key stays without a baseline")
}

func TestCarryOverManagedSubscriptionHistory_CopiesNothingForAnEmptyLegacyKey(t *testing.T) {
	store := &carryOverStore{baselines: map[string][]string{"": {"orders/#"}}}

	require.NoError(t, ports.CarryOverManagedSubscriptionHistory(t.Context(), store, "current", ""))

	_, established := store.baselines["current"]
	assert.False(t, established)
}

func TestCarryOverManagedSubscriptionHistory_ReturnsAStoreFailure(t *testing.T) {
	outage := errors.New("store unavailable")
	store := &carryOverStore{baselines: map[string][]string{}, listErr: outage}

	err := ports.CarryOverManagedSubscriptionHistory(t.Context(), store, "current", "legacy")

	require.ErrorIs(t, err, outage)
}
