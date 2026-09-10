//go:build !race

package constructs_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"

	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
)

// efsParityFixture builds a VPC plus a GoBridgeEfsConfig whose mount
// targets are confined to the availability zones named by azs, and
// returns the VPC's full AZ list for the caller to contrast against.
func efsParityFixture(t *testing.T, azIdx ...int) (awsec2.IVpc, []*string, *gobridgecdk.GoBridgeEfsConfig) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)

	all := *vpc.AvailabilityZones()
	if len(all) < 2 {
		t.Fatalf("fixture needs a multi-AZ VPC, got %d zone(s)", len(all))
	}

	confined := make([]*string, 0, len(azIdx))
	for _, i := range azIdx {
		confined = append(confined, all[i])
	}
	efs := gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{
			Vpc: vpc,
			VpcSubnets: &awsec2.SubnetSelection{
				SubnetType:        awsec2.SubnetType_PRIVATE_WITH_EGRESS,
				AvailabilityZones: &confined,
			},
		})
	return vpc, all, efs
}

// Test_TierB_Validation_EFSSubnetMismatch covers matrix row 14:
//
//	"GoBridgeEfsConfig VpcSubnets must match GoBridge cluster VpcSubnets"
//
// An EFS mount target serves only its own availability zone. When a
// caller supplies a filesystem whose mount targets miss an AZ the parent
// places ECS tasks in, every task scheduled there fails to mount at
// container start — after a clean synth and a green deploy. Synthesis
// must refuse instead.
func Test_TierB_Validation_EFSSubnetMismatch(t *testing.T) {
	// Mount targets in the FIRST AZ only...
	vpc, all, efs := efsParityFixture(t, 0)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("AssertEfsSubnetParity must panic when ECS places tasks in an " +
				"availability zone the filesystem has no mount target in")
		}
		msg := fmt.Sprintf("%v", r)
		// The uncovered zone must be named so the operator can act on it.
		if !strings.Contains(msg, *all[1]) {
			t.Fatalf("panic must name the uncovered AZ %q, got: %s", *all[1], msg)
		}
		if !strings.Contains(msg, "GoBridgeSingle") {
			t.Fatalf("panic must name the calling construct, got: %s", msg)
		}
	}()

	// ...but ECS placement defaults to ALL private subnets, i.e. both AZs.
	gobridgecdk.AssertEfsSubnetParity("GoBridgeSingle", vpc, nil, efs)
}

// Test_TierB_Validation_EFSSubnetParity_AcceptsCoveringSelection is the
// positive half of matrix row 14: the check must not fire when the
// filesystem covers every zone ECS can schedule into. Without this, a
// parity check that panicked unconditionally would still pass the
// negative test above.
func Test_TierB_Validation_EFSSubnetParity_AcceptsCoveringSelection(t *testing.T) {
	// Mount targets in BOTH AZs; ECS default placement covers both.
	vpc, _, efs := efsParityFixture(t, 0, 1)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parity check must accept a filesystem covering every ECS zone, panicked: %v", r)
		}
	}()
	gobridgecdk.AssertEfsSubnetParity("GoBridgeSingle", vpc, nil, efs)
}

// Test_TierB_Validation_EFSSubnetParity_NarrowerECSPlacementIsFine pins
// the direction of the check: it is a COVERAGE test, not an equality
// test. ECS confined to a subset of the filesystem's zones is valid —
// every task still lands beside a mount target — and rejecting it would
// break legitimate stacks that intentionally pin placement.
func Test_TierB_Validation_EFSSubnetParity_NarrowerECSPlacementIsFine(t *testing.T) {
	vpc, all, efs := efsParityFixture(t, 0, 1)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ECS placement narrower than the filesystem must be accepted, panicked: %v", r)
		}
	}()
	gobridgecdk.AssertEfsSubnetParity("GoBridgeSingle", vpc, &awsec2.SubnetSelection{
		SubnetType:        awsec2.SubnetType_PRIVATE_WITH_EGRESS,
		AvailabilityZones: &[]*string{all[0]},
	}, efs)
}
