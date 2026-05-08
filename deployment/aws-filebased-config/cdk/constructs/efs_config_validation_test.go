//go:build !race

package constructs_test

import "testing"

// Test_TierB_Validation_EFSSubnetMismatch is a placeholder for
// matrix row 14. The matrix requires:
//
//	"GoBridgeEfsConfig VpcSubnets must match GoBridge cluster VpcSubnets"
//
// Production gap: the GoBridgeEfsConfig docstring states the parent
// construct enforces that match, but neither GoBridgeSingle nor
// GoBridgeCluster currently compares VpcSubnets selections between
// the EFS construct and the ECS service placement. Until the check
// is added there is nothing to assert against.
//
// TODO(matrix-row 'EFS subnet selection != ECS'): add the parity
// check in GoBridgeSingle/Cluster (see efs_config.go:28-30 doc) and
// replace the Skip below with a real assertion.
func Test_TierB_Validation_EFSSubnetMismatch(t *testing.T) {
	t.Skip("matrix row 14: EFS-vs-ECS subnet parity check not implemented in GoBridgeSingle/Cluster; see TODO")
}
