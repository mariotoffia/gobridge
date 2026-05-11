package bridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuilder_PlanCommit_EquivalentToBuild validates that the public
// Plan/Commit two-phase API yields a runtime equivalent in shape to
// the single-shot Build path.
func TestBuilder_PlanCommit_EquivalentToBuild(t *testing.T) {
	ctx := context.Background()

	rtBuild, err := buildWith(directHoldConfig(), "sqs").Build(ctx)
	require.NoError(t, err)

	builder := buildWith(directHoldConfig(), "sqs")

	plan, err := builder.Plan(ctx)
	require.NoError(t, err)
	require.NotNil(t, plan)

	rtCommit, err := plan.Commit(ctx)
	require.NoError(t, err)
	require.NotNil(t, rtCommit)

	buildRoutes := rtBuild.Routes()
	commitRoutes := rtCommit.Routes()
	require.Len(t, commitRoutes, len(buildRoutes))
	for i := range buildRoutes {
		assert.Equal(t, buildRoutes[i].ID, commitRoutes[i].ID)
		assert.Equal(t, buildRoutes[i].DeliveryMode, commitRoutes[i].DeliveryMode)
	}
}

// TestBuildPlan_CommitTwiceReturnsError validates the one-shot
// invariant: a BuildPlan cannot be committed more than once.
func TestBuildPlan_CommitTwiceReturnsError(t *testing.T) {
	ctx := context.Background()

	plan, err := buildWith(directHoldConfig(), "sqs").Plan(ctx)
	require.NoError(t, err)

	rt, err := plan.Commit(ctx)
	require.NoError(t, err)
	require.NotNil(t, rt)

	_, err = plan.Commit(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already committed")
}

// TestBuildPlan_CommitNilPlan validates the defensive nil-receiver
// guard on Commit.
func TestBuildPlan_CommitNilPlan(t *testing.T) {
	var plan *BuildPlan
	_, err := plan.Commit(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil plan")
}
