package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/bridge"
)

// TestRolloutHealth_MapsEveryDivergenceField pins the reference binary's half of
// the deep-health rollout contract.
//
// Two composition roots build this block from the same bridge.RolloutStatus —
// this one and the shipped AWS bootstrap — and they cannot share the mapping
// across a module boundary. A field that reaches one root's JSON and not the
// other's is the same defect twice: the operator page looks complete while the
// answer they need is missing, and which fields they get depends on which image
// they deployed. So each root pins its own mapping.
func TestRolloutHealth_MapsEveryDivergenceField(t *testing.T) {
	observedAt := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	status := bridge.RolloutStatus{
		MemberID:           "node-a",
		Generation:         4,
		State:              "committed",
		ConfirmPending:     true,
		ConfigVersion:      12,
		Epoch:              []string{"node-a", "node-b"},
		Acked:              []string{"node-a", "node-b"},
		Converged:          []string{"node-b"},
		Reason:             "a peer nacked",
		Staged:             true,
		Applied:            false,
		ObservedAt:         observedAt,
		ObservationAge:     9 * time.Second,
		Stale:              true,
		LastError:          "rollout store read timed out",
		ArtifactGeneration: 3,
		TerminalGeneration: 4,
		TerminalReason:     "replace this member",
	}

	got := rolloutHealth(status)

	require.NotNil(t, got)
	assert.Equal(t, "node-a", got.MemberID)
	assert.Equal(t, uint64(4), got.Generation)
	assert.Equal(t, "committed", got.State)
	assert.True(t, got.ConfirmPending, "an operator must not read a provisional commit as final")
	assert.Equal(t, 12, got.ConfigVersion)
	assert.Equal(t, []string{"node-a", "node-b"}, got.Epoch)
	assert.Equal(t, []string{"node-a", "node-b"}, got.Acked)
	assert.Equal(t, []string{"node-b"}, got.Converged)
	assert.Equal(t, "a peer nacked", got.Reason)
	assert.True(t, got.CandidateStaged)
	assert.False(t, got.Applied)
	assert.Equal(t, observedAt, got.ObservedAt)
	assert.Equal(t, int64(9000), got.ObservationAgeMS)
	assert.True(t, got.Stale)
	assert.Equal(t, "rollout store read timed out", got.LastError)
	assert.Equal(t, uint64(3), got.ArtifactGeneration)
	assert.Equal(t, uint64(4), got.TerminalGeneration)
	assert.Equal(t, "replace this member", got.TerminalReason)
}
