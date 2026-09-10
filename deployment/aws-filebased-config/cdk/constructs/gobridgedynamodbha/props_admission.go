package gobridgedynamodbha

// Props and placement admission: the checks that depend on the CALLER's inputs
// (and on the VPC they picked) rather than on the shared config document, which
// config_admission.go owns.

import (
	"fmt"
	"math"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
)

func requireTwoAvailabilityZones(vpc awsec2.IVpc, selection *awsec2.SubnetSelection) {
	selected := vpc.SelectSubnets(selection)
	if selected.IsPendingLookup != nil && *selected.IsPendingLookup {
		// Vpc.FromLookup uses a two-pass CDK context provider. The first pass
		// intentionally returns placeholder subnets; the app is re-run after
		// context resolution, when this same function enforces the real AZ set.
		return
	}
	zones := map[string]struct{}{}
	if selected.AvailabilityZones != nil {
		for _, zone := range *selected.AvailabilityZones {
			if zone != nil && *zone != "" {
				zones[*zone] = struct{}{}
			}
		}
	}
	if len(zones) < 2 {
		panic("GoBridgeDynamoDBHA: VpcSubnets must span at least two Availability Zones")
	}
}

func validateProps(props *DynamoDBHAProps) {
	if props == nil {
		panic("GoBridgeDynamoDBHA: props must not be nil")
	}
	if props.Vpc == nil {
		panic("GoBridgeDynamoDBHA: Vpc is required")
	}
	if props.Image == nil {
		panic("GoBridgeDynamoDBHA: Image is required")
	}
	if props.BridgeConfig == nil {
		panic("GoBridgeDynamoDBHA: BridgeConfig is required")
	}
	if props.MemberSlots != nil && props.WorkerServiceName != nil {
		longest := 0
		for _, id := range props.MemberSlots.WorkerMemberIDs {
			if len(id) > longest {
				longest = len(id)
			}
		}
		// Each slot's service takes the pinned name as a prefix and its own id as a
		// suffix, and ECS caps a service name at 255 characters. Catch it here rather
		// than at CreateService, halfway through a deploy.
		if total := len(*props.WorkerServiceName) + 1 + longest; total > maxECSServiceNameLength {
			panic(fmt.Sprintf("GoBridgeDynamoDBHA: WorkerServiceName is %d characters and the longest member "+
				"slot id is %d, so a slot service name would be %d characters; ECS allows at most %d. "+
				"Shorten the pinned name or the slot ids",
				len(*props.WorkerServiceName), longest, total, maxECSServiceNameLength))
		}
	}
	if props.MemberSlots != nil && props.WorkerDesiredCount != nil {
		panic("GoBridgeDynamoDBHA: WorkerDesiredCount cannot be combined with MemberSlots: the roster IS the " +
			"slot count, and scaling a slot's service past one task would run two processes under one member_id")
	}
	if props.WorkerDesiredCount != nil {
		value := *props.WorkerDesiredCount
		if math.IsNaN(value) || math.IsInf(value, 0) {
			panic("GoBridgeDynamoDBHA: WorkerDesiredCount must be a resolved finite integer >= 2 to preserve a warm standby")
		}
		if unresolved := awscdk.Token_IsUnresolved(value); unresolved != nil && *unresolved {
			panic("GoBridgeDynamoDBHA: WorkerDesiredCount must be a resolved finite integer >= 2; unresolved tokens cannot prove the warm-standby invariant")
		}
		if math.Trunc(value) != value || value < 2 {
			panic("GoBridgeDynamoDBHA: WorkerDesiredCount must be a resolved finite integer >= 2 to preserve a warm standby")
		}
	}
}
