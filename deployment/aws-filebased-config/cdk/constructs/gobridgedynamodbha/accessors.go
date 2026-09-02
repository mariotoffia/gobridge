package gobridgedynamodbha

// Public accessors. Everything this facade exposes to a composition root — the
// services and task definitions it created, the shared infrastructure it owns, and
// the two identities (the coordinated-rollout roster and the rollout table name)
// that only the static member-slot profile has.

import (
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
)

func (g *GoBridgeDynamoDBHA) ControlService() awsecs.IService { return g.control }

// WorkerService returns the FIRST worker-side service. It stays single-valued for
// the autoscaled profile, where there is only one; a consumer that must cover the
// whole fleet (ALB targets, per-service alarms) has to use WorkerServices, because
// the static member-slot profile runs one service per roster member.
func (g *GoBridgeDynamoDBHA) WorkerService() awsecs.IService { return g.workers[0] }

// WorkerServices returns every worker-side service: one for the autoscaled
// profile, one per roster worker slot for the static member-slot profile.
func (g *GoBridgeDynamoDBHA) WorkerServices() []awsecs.FargateService {
	return append([]awsecs.FargateService(nil), g.workers...)
}

// MemberSlotIDs returns the coordinated-rollout roster this deployment provisions
// — the control slot first, then the worker slots — or nil on the autoscaled
// profile, whose interchangeable tasks carry no member identity.
func (g *GoBridgeDynamoDBHA) MemberSlotIDs() []string {
	if len(g.memberSlots) == 0 {
		return nil
	}
	return append([]string(nil), g.memberSlots...)
}

func (g *GoBridgeDynamoDBHA) ControlTaskDefinition() awsecs.FargateTaskDefinition {
	return g.controlBase.TaskDefinition
}
func (g *GoBridgeDynamoDBHA) WorkerTaskDefinition() awsecs.FargateTaskDefinition {
	return g.workerBases[0].TaskDefinition
}

// WorkerTaskDefinitions returns every worker-side task definition, one per slot.
func (g *GoBridgeDynamoDBHA) WorkerTaskDefinitions() []awsecs.FargateTaskDefinition {
	out := make([]awsecs.FargateTaskDefinition, 0, len(g.workerBases))
	for _, base := range g.workerBases {
		out = append(out, base.TaskDefinition)
	}
	return out
}
func (g *GoBridgeDynamoDBHA) Cluster() awsecs.ICluster                    { return g.cluster }
func (g *GoBridgeDynamoDBHA) EfsConfig() *cdkconstructs.GoBridgeEfsConfig { return g.efsConfig }
func (g *GoBridgeDynamoDBHA) Data() *DynamoDBHAData                       { return g.data }
func (g *GoBridgeDynamoDBHA) ControlSecurityGroup() awsec2.ISecurityGroup { return g.controlSG }
func (g *GoBridgeDynamoDBHA) WorkerSecurityGroup() awsec2.ISecurityGroup  { return g.workerSG }
func (g *GoBridgeDynamoDBHA) ControlPortMappings() []gobridgebase.PortMapping {
	return g.controlBase.PortMappings
}
func (g *GoBridgeDynamoDBHA) WorkerPortMappings() []gobridgebase.PortMapping {
	return g.workerBases[0].PortMappings
}

// RolloutTableName returns the RESOLVED physical name of the deployment-owned
// rollout coordination table, or "" on the autoscaled profile. Unlike
// Data().RolloutTableName() — a CDK token, like every other table accessor — this
// is the literal that is serialized into each task definition's bootstrap
// document, where an unresolved token would reach the runtime verbatim.
func (g *GoBridgeDynamoDBHA) RolloutTableName() string { return g.rolloutTableName }

func (g *GoBridgeDynamoDBHA) FailoverObjective() time.Duration { return g.failoverObjective }
func (g *GoBridgeDynamoDBHA) MetricsNamespace() string         { return g.metricsNamespace }
