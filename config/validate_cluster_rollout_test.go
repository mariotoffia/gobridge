package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// coordinatedRolloutConfig builds a minimal valid clustered blueprint opted into
// the coordinated rollout barrier, which each test then perturbs.
func coordinatedRolloutConfig() *ports.BridgeConfig {
	cfg := minimalValidBridgeConfig()
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Bridge.Cluster = &ports.ClusterConfig{
		Rollout: "coordinated",
		Members: []string{"node-a", "node-b", "node-c"},
	}
	return cfg
}

// TestValidateClusterRollout_AcceptsACompleteCoordinatedCohort proves the happy
// shape validates, so the rules below are rejecting the specific defect named
// and not the feature itself.
func TestValidateClusterRollout_AcceptsACompleteCoordinatedCohort(t *testing.T) {
	require.NoError(t, Validate(coordinatedRolloutConfig()))
}

// TestValidateClusterRollout_AcceptsTheDefaultModes proves an unset or explicit
// "refuse" needs no roster: those are ADR 0012's behavior, where no barrier
// exists and the roster is unused.
func TestValidateClusterRollout_AcceptsTheDefaultModes(t *testing.T) {
	for _, mode := range []string{"", "refuse"} {
		cfg := coordinatedRolloutConfig()
		cfg.Bridge.Cluster.Rollout = mode
		cfg.Bridge.Cluster.Members = nil
		assert.NoError(t, Validate(cfg), "rollout mode %q", mode)
	}
}

// TestValidateClusterRollout_RejectsAnUnknownMode proves a typo is not silently
// treated as the default. "cordinated" resolving to refuse would leave an
// operator believing coordinated rollout is enabled while every change is
// refused — the failure would only surface during a change.
func TestValidateClusterRollout_RejectsAnUnknownMode(t *testing.T) {
	cfg := coordinatedRolloutConfig()
	cfg.Bridge.Cluster.Rollout = "cordinated"

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.cluster.rollout")
}

// TestValidateClusterRollout_RejectsAnEmptyRoster is the rule that matters most.
// The roster is the membership epoch the barrier freezes and counts
// acknowledgements against (I2); an EMPTY epoch is vacuously covered by zero
// acknowledgements, so the first coordinator observation would commit a config
// no member ever validated — the barrier would silently not be a barrier.
func TestValidateClusterRollout_RejectsAnEmptyRoster(t *testing.T) {
	cfg := coordinatedRolloutConfig()
	cfg.Bridge.Cluster.Members = nil

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.cluster.members")
}

// TestValidateClusterRollout_RejectsADuplicateMember proves duplicates are
// rejected rather than deduped. A repeated id means the roster was written by
// hand against a real cohort and one entry is wrong; the resulting epoch would be
// smaller than the operator believes, so the barrier would commit without one
// node's acknowledgement.
func TestValidateClusterRollout_RejectsADuplicateMember(t *testing.T) {
	cfg := coordinatedRolloutConfig()
	cfg.Bridge.Cluster.Members = []string{"node-a", "node-b", "node-a"}

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate member id")
}

// TestValidateClusterRollout_RejectsAnEmptyMemberID proves a blank entry is
// caught: it would be a cohort member no process can ever match, so every
// rollout would deadline-abort waiting for an acknowledgement that cannot come.
func TestValidateClusterRollout_RejectsAnEmptyMemberID(t *testing.T) {
	cfg := coordinatedRolloutConfig()
	cfg.Bridge.Cluster.Members = []string{"node-a", ""}

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty member id")
}

// TestValidateClusterRollout_RejectsCoordinatedOnAStandaloneDeployment proves
// the mode is meaningless without a cohort: the apply-path guard never consults
// the barrier for a non-clustered deployment, so accepting it would advertise
// coordination that never runs.
func TestValidateClusterRollout_RejectsCoordinatedOnAStandaloneDeployment(t *testing.T) {
	cfg := coordinatedRolloutConfig()
	cfg.Bridge.DeploymentMode = "standalone"
	cfg.Bridge.Cluster.Endpoints = nil

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered")
}

// TestValidateClusterRollout_EndpointsAreNotARoster pins the distinction the
// design originally got wrong. bridge.cluster.endpoints is THIS instance's
// capability map; supplying it does not declare a cohort, so a coordinated
// deployment with endpoints but no members is still rejected.
func TestValidateClusterRollout_EndpointsAreNotARoster(t *testing.T) {
	cfg := coordinatedRolloutConfig()
	cfg.Bridge.Cluster.Members = nil
	cfg.Bridge.Cluster.Endpoints = map[string]string{"http": "http://10.0.0.1:8080"}

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge.cluster.members")
}
