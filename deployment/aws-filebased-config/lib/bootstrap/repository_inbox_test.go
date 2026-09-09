package bootstrap

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestRepositoryInboxKeepsWithdrawalAheadOfLatestSnapshot(t *testing.T) {
	q := newRepositoryInbox()
	q.put(repositoryEvent{observation: ports.ConfigObservation{Kind: ports.ConfigPresent, Config: withdrawalConfig(1)}})
	q.put(repositoryEvent{observation: ports.ConfigObservation{Kind: ports.ConfigMissing}, epoch: 1})
	q.put(repositoryEvent{observation: ports.ConfigObservation{Kind: ports.ConfigPresent, Config: withdrawalConfig(2)}, epoch: 1})
	q.put(repositoryEvent{observation: ports.ConfigObservation{Kind: ports.ConfigMissing}, epoch: 2})
	latest := withdrawalConfig(3)
	q.put(repositoryEvent{observation: ports.ConfigObservation{Kind: ports.ConfigPresent, Config: latest}, epoch: 2})
	q.close()
	missing, ok, closed := q.take()
	require.True(t, ok)
	require.False(t, closed)
	require.Equal(t, ports.ConfigMissing, missing.observation.Kind)
	require.Equal(t, uint64(2), missing.epoch)
	present, ok, closed := q.take()
	require.True(t, ok)
	require.False(t, closed)
	require.Same(t, latest, present.observation.Config)
	require.Equal(t, missing.epoch, present.epoch)
	_, ok, closed = q.take()
	require.False(t, ok)
	require.True(t, closed)
}
