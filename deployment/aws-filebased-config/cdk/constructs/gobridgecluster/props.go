package gobridgecluster

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// AutoScalingProps opts the worker service into target-tracking CPU
// autoscaling. Min/Max bound the worker DesiredCount; TargetCPU is
// the target average ECS service CPU utilization in percent. When
// TargetCPU is zero (the default) it is treated as 70.
type AutoScalingProps struct {
	Min       float64
	Max       float64
	TargetCPU float64
}

// ClusterProps configures a [GoBridgeCluster] facade. It is the
// public surface for consumers who want a control + worker pair
// sharing one EFS filesystem.
//
// Required: Vpc, Image, Bootstrap, BridgeConfig.
//
// Conditionally required (Phase 2 validation surfaces a typed error
// when missing while the yaml needs them): QueueRegistry,
// SsmParamRegistry.
type ClusterProps struct {
	// Vpc is the VPC both Fargate services and the EFS mount
	// targets live in. Required.
	Vpc awsec2.IVpc

	// VpcSubnets selects the subnets used for ECS placement and
	// (when EfsConfig is auto-created) EFS mount targets. nil
	// means "all private subnets in Vpc". Applied to BOTH services.
	VpcSubnets *awsec2.SubnetSelection

	// Cluster is an existing ECS cluster shared by both services.
	// When nil a fresh cluster is created in Vpc as a child of
	// this construct.
	Cluster awsecs.ICluster

	// EfsConfig provides the EFS filesystem and access points
	// shared by both services. When nil a default
	// [GoBridgeEfsConfig] is created with always-on encryption,
	// ELASTIC throughput and RETAIN policy.
	EfsConfig *cdkconstructs.GoBridgeEfsConfig

	// EfsKmsKey, when non-nil, is forwarded to BOTH base calls for
	// KMS grants on the task roles.
	EfsKmsKey awskms.IKey

	// Image is the required sealed gobridgecdk.BridgeImageSource used by both
	// services. Use ImageFromRegistry, ImageFromEcrRepository or ImageFromGoBuild.
	Image imgsource.Source

	// Bootstrap is the deployment-owned runtime configuration. Its
	// NodeRole is forced per service by this facade — control gets
	// NodeRoleControl, workers get NodeRoleWorker. Never mutated
	// in place. Required.
	Bootstrap infra.BootstrapConfig

	// BridgeConfig is the sealed source produced by
	// gobridgecdk.BridgeYamlAsset / BridgeYamlInline. Required.
	BridgeConfig source.Source

	// QueueRegistry resolves SQS queue names referenced by the
	// parsed bridge config. Conditionally required.
	QueueRegistry *registry.QueueRegistry

	// SsmParamRegistry resolves SSM parameter URIs referenced by
	// the parsed bridge config. Conditionally required.
	SsmParamRegistry *registry.SsmParamRegistry

	// ControlSecurityGroup, when non-nil, is the security group
	// attached to the control Fargate service. When nil one is
	// auto-created.
	ControlSecurityGroup awsec2.ISecurityGroup

	// WorkerSecurityGroup, when non-nil, is the security group
	// attached to the worker Fargate service. When nil one is
	// auto-created.
	WorkerSecurityGroup awsec2.ISecurityGroup

	// CPU overrides the default Fargate CPU units (512). Applied
	// to BOTH task definitions.
	CPU *float64

	// MemoryMiB overrides the default Fargate memory (1024 MiB).
	// Applied to BOTH task definitions.
	MemoryMiB *float64

	// MountPath overrides the default container EFS mount path
	// ("/var/lib/gobridge"). Applied to BOTH services.
	MountPath *string

	// LogRetention overrides the default CloudWatch log retention
	// (one month). Applied to BOTH services.
	LogRetention awslogs.RetentionDays

	// LogRemovalPolicy overrides the default RETAIN policy on log
	// groups. Applied to BOTH services.
	LogRemovalPolicy awscdk.RemovalPolicy

	// ControlServiceName overrides the auto-generated control ECS
	// service name.
	ControlServiceName *string

	// WorkerServiceName overrides the auto-generated worker ECS
	// service name.
	WorkerServiceName *string

	// WorkerDesiredCount sets the worker service DesiredCount.
	// Default 2 when nil. Must be >= 1 when set.
	WorkerDesiredCount *float64

	// AutoScaling, when non-nil, opts the worker service into
	// target-tracking CPU autoscaling. Off by default.
	AutoScaling *AutoScalingProps
}
