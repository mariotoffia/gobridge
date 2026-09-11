// Package gobridgesingle exports the GoBridgeSingle facade construct
// — one ECS Fargate task with an optional RW EFS mount, no clustering, built on
// top of the shared gobridgebase. Lives in its own sub-package
// (rather than directly under cdk/constructs) to avoid the import
// cycle constructs → gobridgebase → constructs (for GoBridgeEfsConfig
// type access). The cdk/gobridge package re-exports the public surface;
// consumers should normally use that rather than importing this package
// directly.
package gobridgesingle

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
)

// SingleProps configures a [GoBridgeSingle] facade. It is the public
// surface for consumers who want one ECS Fargate control task with
// an optional RW EFS mount and no clustering. All optional fields fall back to
// the documented defaults; only the four required fields below MUST
// be supplied.
//
// Required: Vpc, Bootstrap, BridgeConfig.
//
// Conditionally required (Phase 2 validation surfaces a typed error
// when missing while the yaml needs them): Queues, QueueTags, Secrets.
type SingleProps struct {
	// Vpc is the VPC the Fargate task and the EFS mount targets
	// live in. Required (no default lookup at this stage
	// keeps it explicit; the design's optional-VPC behaviour will
	// land alongside the auto-lookup helper).
	Vpc awsec2.IVpc

	// VpcSubnets selects the subnets used for both ECS placement
	// and (when EfsConfig is auto-created) EFS mount targets. nil
	// means "all private subnets in Vpc".
	VpcSubnets *awsec2.SubnetSelection

	// Cluster is an existing ECS cluster. When nil a fresh cluster
	// is created in Vpc as a child of this construct.
	Cluster awsecs.ICluster

	// EfsConfig provides the EFS filesystem and access points.
	// Used only when file config or parsed store paths need a filesystem.
	// When needed and nil, a default [GoBridgeEfsConfig] is created with
	// always-on encryption, ELASTIC throughput and RETAIN policy.
	EfsConfig *cdkconstructs.GoBridgeEfsConfig

	// EfsKmsKey, when non-nil, is forwarded to the base for KMS
	// grants on the task role. Independent of EfsConfig — pass the
	// same key to both if you want CMK-encrypted EFS at rest AND
	// the runtime grant.
	EfsKmsKey awskms.IKey

	// Image is the image the task runs. Nil builds the published command at
	// this module's own version, linking only the transports the config uses.
	// Use ImageFromRegistry, ImageFromEcrRepository or ImageFromGoBuild to
	// choose otherwise.
	Image imgsource.Source

	// Bootstrap is the deployment-owned runtime configuration. Its
	// NodeRole is forced to NodeRoleControl by this facade —
	// SingleProps never produces a worker task. Required.
	Bootstrap infra.BootstrapConfig

	// BridgeConfig is the sealed source produced by
	// gobridgecdk.BridgeYamlAsset / BridgeYamlInline. Required.
	BridgeConfig source.Source

	// ManagedSubscriptionBaselines attests, for every persistent or exclusive
	// MQTT session that subscribes, the exact topic filters its broker identity
	// already holds; an empty list attests a NEW identity with none. Such a
	// session does not start without its baseline, and on this profile the
	// store may live on the config mount, which only the task can write — so
	// the facade stamps the attestation into the bootstrap document
	// (managed_subscription_baselines) and the runtime seeds it at every boot,
	// idempotently. Required for each such session; never attest empty for an
	// identity that may still hold subscriptions.
	ManagedSubscriptionBaselines map[string][]string

	// Queues maps each SQS queue name the bridge config uses to its CDK
	// queue. The task is granted exactly these queues.
	Queues map[string]awssqs.IQueue

	// QueueTags selects queues by tag instead of name, keyed like Queues.
	// Needed only when the config uses queue_tags.
	QueueTags map[string]registry.QueueTags

	// Secrets maps each SSM parameter path the config reads (/app/key or
	// pms://app/key) to its CDK parameter, so the task may read it.
	Secrets map[string]awsssm.IParameter

	// SecurityGroup, when non-nil, is the security group attached
	// to the Fargate service. When nil one is auto-created.
	SecurityGroup awsec2.ISecurityGroup

	// CPU overrides the default Fargate CPU units (512).
	CPU *float64

	// MemoryMiB overrides the default Fargate memory (1024 MiB).
	MemoryMiB *float64

	// MountPath overrides the default container EFS mount path
	// ("/var/lib/gobridge").
	MountPath *string

	// LogRetention overrides the default CloudWatch log retention
	// (one month).
	LogRetention awslogs.RetentionDays

	// LogRemovalPolicy overrides the default RETAIN policy on log
	// groups.
	LogRemovalPolicy awscdk.RemovalPolicy

	// ServiceName overrides the auto-generated ECS service name.
	ServiceName *string
}

// GoBridgeSingle is the L2 facade construct that deploys the
// single-task control profile of gobridge: one Fargate task with RW
// EFS mount, no worker, no clustering. It is a thin wrapper over
// [gobridgebase] — all task-def, EFS, IAM and image
// machinery is owned by the shared base; this construct only adds
// the surrounding ECS service, security group, EFS ingress rule and
// runs the Phase 1 / Phase 2 tier-B validators on the resolved
// BridgeConfig.
//
// DesiredCount is hard-coded to 1 and NOT exposed as a prop: it is
// a runtime invariant of the single-LeaseStore-writer semantics
// documented in the design. The deployment strategy is
// MinHealthyPercent=0 / MaxHealthyPercent=100 so the previous task
// is fully drained before the new one starts — eliminating
// concurrent EFS RW writers during rolling deploys.
type GoBridgeSingle struct {
	constructs.Construct

	base      *gobridgebase.Built
	service   awsecs.FargateService
	cluster   awsecs.ICluster
	efsConfig *cdkconstructs.GoBridgeEfsConfig
	sg        awsec2.ISecurityGroup
}

// NewGoBridgeSingle constructs a [GoBridgeSingle] under scope/id.
//
// Construction order:
//
//  1. Validate required props (panic with a clear message on miss).
//  2. Materialize the BridgeConfig source once and run Phase 1
//     fast-fail validation against it. On failure: panic.
//  3. Build (or reuse) the EFS config and ECS cluster.
//  4. Delegate task-def + IAM + image construction to
//     [gobridgebase.New] in CONTROL mode.
//  5. Create the FargateService with DesiredCount=1 and a 0/100
//     deployment strategy.
//  6. Wire EFS NFS ingress from the task SG to the EFS SG.
//  7. Run Phase 2 aggregated validation via CDK Annotations so a
//     single synth surfaces every missing registry reference.
//
// TODO: synth-time scope scan to enforce singleton
// constraint — error if multiple GoBridgeSingle / GoBridgeCluster
// siblings exist in the same Stack tree.
func NewGoBridgeSingle(scope constructs.Construct, id *string, props *SingleProps) *GoBridgeSingle {
	if props == nil {
		panic("GoBridgeSingle: props must not be nil")
	}
	validateSingleProps(props)
	queues, secrets, err := registry.FromProps(props.Queues, props.QueueTags, props.Secrets)
	if err != nil {
		panic(fmt.Sprintf("GoBridgeSingle: %v", err))
	}
	image := imgsource.OrDefault(props.Image)

	c := constructs.NewConstruct(scope, id)

	// Force control role on the bootstrap copy — SingleProps never
	// produces a worker.
	bootstrap := props.Bootstrap.Normalized()
	bootstrap.NodeRole = infra.NodeRoleControl

	// Phase 1 — fast-fail tier-B validation on the resolved config.
	mat, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic(fmt.Sprintf("GoBridgeSingle: materialize bridge config: %v", err))
	}
	mountPath := ""
	if props.MountPath != nil {
		mountPath = *props.MountPath
	}
	if err := validation.Phase1(validation.Phase1Input{
		Materialized: mat,
		Bootstrap:    bootstrap,
		MountPath:    mountPath,
		NodeRole:     infra.NodeRoleControl,
	}); err != nil {
		_ = mat.Close()
		panic(fmt.Sprintf("GoBridgeSingle: Phase 1 validation failed: %v", err))
	}
	baselines, err := managedSubscriptionBaselines(mat.Config, props.ManagedSubscriptionBaselines)
	if err != nil {
		_ = mat.Close()
		panic(fmt.Sprintf("GoBridgeSingle: %v", err))
	}
	needsEFS := validation.NeedsEFS(mat.Config, bootstrap)
	bootstrap.ManagedSubscriptionBaselines = baselines
	// Cleanup is best-effort; the base will materialize again from
	// the same source for image construction.
	_ = mat.Close()

	// EFS config — auto-create when not supplied. An auto-created config
	// gets props.VpcSubnets verbatim, so its mount targets always cover the
	// ECS placement; a SUPPLIED one may not, and a task in an AZ without a
	// mount target fails at container start (matrix row 14).
	var efsConfig *cdkconstructs.GoBridgeEfsConfig
	if needsEFS {
		efsConfig = props.EfsConfig
		if efsConfig == nil {
			efsConfig = cdkconstructs.NewGoBridgeEfsConfig(c, jsii.String("Efs"), &cdkconstructs.GoBridgeEfsConfigProps{
				Vpc:        props.Vpc,
				VpcSubnets: props.VpcSubnets,
				EfsKmsKey:  props.EfsKmsKey,
			})
		} else {
			cdkconstructs.AssertEfsSubnetParity("GoBridgeSingle", props.Vpc, props.VpcSubnets, efsConfig)
		}
	}
	configTable := gobridgebase.NewConfigTable(c, bootstrap)

	// ECS cluster — auto-create when not supplied.
	cluster := props.Cluster
	if cluster == nil {
		cluster = awsecs.NewCluster(c, jsii.String("Cluster"), &awsecs.ClusterProps{
			Vpc:                 props.Vpc,
			ContainerInsightsV2: awsecs.ContainerInsights_ENABLED,
		})
	}

	// Shared base (CONTROL mode allows target initialization and updates).
	built := gobridgebase.New(c, jsii.String("Base"), &gobridgebase.Props{
		Mode:             gobridgebase.ModeControl,
		Vpc:              props.Vpc,
		EfsConfig:        efsConfig,
		ConfigTable:      configTable,
		EfsKmsKey:        props.EfsKmsKey,
		Image:            image,
		Bootstrap:        bootstrap,
		Source:           props.BridgeConfig,
		QueueRegistry:    queues,
		SsmRegistry:      secrets,
		CPU:              props.CPU,
		MemoryMiB:        props.MemoryMiB,
		MountPath:        props.MountPath,
		LogRetention:     props.LogRetention,
		LogRemovalPolicy: props.LogRemovalPolicy,
	})

	// Security group for the task.
	sg := props.SecurityGroup
	if sg == nil {
		sg = awsec2.NewSecurityGroup(c, jsii.String("SvcSG"), &awsec2.SecurityGroupProps{
			Vpc:         props.Vpc,
			Description: jsii.String("gobridge control task"),
		})
	}

	// Allow task → EFS NFS ingress when the EFS construct owns the SG.
	if efsConfig != nil {
		if efsSG := efsConfig.SecurityGroup(); efsSG != nil {
			efsSG.AddIngressRule(
				sg,
				awsec2.Port_Tcp(jsii.Number(2049)),
				jsii.String("gobridge control task NFS access"),
				jsii.Bool(false),
			)
		}
	}

	// Fargate service: DesiredCount=1 (hard-coded) + 0/100
	// deployment strategy. ServiceName is optional — when nil CDK
	// generates a stable physical id.
	svcProps := &awsecs.FargateServiceProps{
		Cluster:              cluster,
		TaskDefinition:       built.TaskDefinition,
		DesiredCount:         jsii.Number(1),
		MinHealthyPercent:    jsii.Number(0),
		MaxHealthyPercent:    jsii.Number(100),
		SecurityGroups:       &[]awsec2.ISecurityGroup{sg},
		EnableExecuteCommand: jsii.Bool(false),
		CircuitBreaker:       &awsecs.DeploymentCircuitBreaker{Rollback: jsii.Bool(true)},
	}
	if props.VpcSubnets != nil {
		svcProps.VpcSubnets = props.VpcSubnets
	}
	if props.ServiceName != nil {
		svcProps.ServiceName = props.ServiceName
	}
	svc := awsecs.NewFargateService(c, jsii.String("Service"), svcProps)

	// Phase 2 — aggregated validation via CDK Annotations on the
	// base materialized config. We re-materialize for Phase 2 to
	// avoid keeping a Materialized alive across base construction.
	mat2, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic("GoBridgeSingle: Phase 2 re-materialize failed: " + err.Error())
	}
	defer func() { _ = mat2.Close() }()
	validation.RunPhase2(c, validation.Phase2Input{
		Cfg:              mat2.Config,
		QueueRegistry:    queues,
		SsmParamRegistry: secrets,
	})

	facade := &GoBridgeSingle{
		Construct: c,
		base:      built,
		service:   svc,
		cluster:   cluster,
		efsConfig: efsConfig,
		sg:        sg,
	}
	// Pass the inner construct (not the facade) because jsii rejects
	// re-embedding a constructs.Construct proxy in another Go value
	// when that value has not been registered with the jsii runtime.
	singleton.Register(c, "single")
	singleton.Enforce(c)
	return facade
}

// ControlService returns the underlying ECS Fargate service. The
// Single profile has no separate worker — see [GoBridgeCluster] for
// a control + worker pair. Returned as the [awsecs.IService]
// interface per design contract; consumers needing the concrete
// type can type-assert.
func (g *GoBridgeSingle) ControlService() awsecs.IService { return g.service }

// TaskDefinition returns the Fargate task definition built by the
// shared base.
func (g *GoBridgeSingle) TaskDefinition() awsecs.FargateTaskDefinition {
	return g.base.TaskDefinition
}

// Cluster returns the ECS cluster the service runs in (either the
// one supplied via SingleProps.Cluster or the auto-created one).
func (g *GoBridgeSingle) Cluster() awsecs.ICluster { return g.cluster }

// EfsConfig returns the EFS configuration used by the construct
// (either the supplied one or the auto-created default), or nil when neither
// the config source nor the parsed store paths require a filesystem.
func (g *GoBridgeSingle) EfsConfig() *cdkconstructs.GoBridgeEfsConfig { return g.efsConfig }

// SecurityGroup returns the security group attached to the Fargate
// service.
func (g *GoBridgeSingle) SecurityGroup() awsec2.ISecurityGroup { return g.sg }

// PortMappings returns the container port mappings derived by the
// shared base from the parsed BridgeConfig + Bootstrap.
func (g *GoBridgeSingle) PortMappings() []gobridgebase.PortMapping {
	return g.base.PortMappings
}

func validateSingleProps(p *SingleProps) {
	if p.Vpc == nil {
		panic("GoBridgeSingle: Vpc is required")
	}
	if p.BridgeConfig == nil {
		panic("GoBridgeSingle: BridgeConfig is required (use gobridge.ConfigFile / ConfigInline)")
	}
}
