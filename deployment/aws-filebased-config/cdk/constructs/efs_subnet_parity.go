package constructs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
)

// AssertEfsSubnetParity enforces Validation Matrix row 14
// ("GoBridgeEfsConfig VpcSubnets must match GoBridge cluster VpcSubnets").
//
// An EFS mount target serves its entire availability zone. A Fargate task
// placed in an AZ with no mount target cannot mount the filesystem — the
// task fails at container start, long after a clean `cdk synth` and a
// successful deploy. Nothing in CloudFormation catches it, which is why the
// parent construct has to.
//
// The check therefore compares AVAILABILITY ZONES, not subnet IDs: two
// different subnet selections that cover the same AZs are perfectly valid,
// and demanding identical subnet IDs would reject them.
//
// It is only meaningful when the caller SUPPLIES an EfsConfig. When
// GoBridgeSingle/Cluster auto-creates one it passes its own props.VpcSubnets
// straight through, so parity is structural.
//
// parent names the calling construct for the panic message
// ("GoBridgeSingle" / "GoBridgeCluster"). ecsSubnets is the parent's
// placement selection (nil means the CDK default, private-with-egress —
// the same default GoBridgeEfsConfig applies).
//
// Panics on mismatch, matching the Tier-B convention used by the rest of
// this package: a misconfiguration that cannot produce a working stack
// fails synthesis rather than deploying broken.
func AssertEfsSubnetParity(
	parent string,
	vpc awsec2.IVpc,
	ecsSubnets *awsec2.SubnetSelection,
	efs *GoBridgeEfsConfig,
) {
	if vpc == nil || efs == nil {
		return
	}

	// A filesystem in a different VPC can never be mounted, whatever the
	// AZ names say — mount targets are VPC-scoped.
	if efs.vpc != nil && *efs.vpc.VpcId() != *vpc.VpcId() {
		panic(fmt.Sprintf(
			"%s: EfsConfig is in VPC %q but the ECS services run in VPC %q; "+
				"EFS mount targets are VPC-scoped and cannot be reached across VPCs",
			parent, *efs.vpc.VpcId(), *vpc.VpcId()))
	}

	selection := ecsSubnets
	if selection == nil {
		selection = &awsec2.SubnetSelection{
			SubnetType: awsec2.SubnetType_PRIVATE_WITH_EGRESS,
		}
	}
	ecsAZs := availabilityZonesOf(vpc.SelectSubnets(selection))
	if len(ecsAZs) == 0 || len(efs.mountAZs) == 0 {
		// Either side unresolved (a pending Vpc.fromLookup, or a dummy
		// context on the first synth). CDK's own guidance for that state is
		// to validate nothing; the mismatch surfaces on the next synth once
		// the lookup is cached.
		return
	}

	have := make(map[string]struct{}, len(efs.mountAZs))
	for _, az := range efs.mountAZs {
		have[az] = struct{}{}
	}
	var missing []string
	for _, az := range ecsAZs {
		if _, ok := have[az]; !ok {
			missing = append(missing, az)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)

	panic(fmt.Sprintf(
		"%s: VpcSubnets places ECS tasks in availability zone(s) [%s] that the "+
			"supplied EfsConfig has no mount target in (mount targets: [%s]). "+
			"Tasks scheduled there will fail to mount the filesystem at container "+
			"start. Pass the same VpcSubnets selection to GoBridgeEfsConfig, or "+
			"narrow the %s VpcSubnets to the zones the filesystem covers.",
		parent, strings.Join(missing, ", "), strings.Join(efs.mountAZs, ", "), parent))
}

// availabilityZonesOf flattens the AZ list of a resolved selection,
// tolerating the nil-heavy jsii shape and dropping the still-unresolved
// case (IsPendingLookup) that CDK explicitly asks callers not to validate.
func availabilityZonesOf(sel *awsec2.SelectedSubnets) []string {
	if sel == nil || sel.AvailabilityZones == nil {
		return nil
	}
	if sel.IsPendingLookup != nil && *sel.IsPendingLookup {
		return nil
	}
	azs := make([]string, 0, len(*sel.AvailabilityZones))
	for _, az := range *sel.AvailabilityZones {
		if az != nil && *az != "" {
			azs = append(azs, *az)
		}
	}
	sort.Strings(azs)
	return azs
}
