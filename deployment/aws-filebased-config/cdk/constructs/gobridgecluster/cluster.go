// Package gobridgecluster exports the GoBridgeCluster facade
// construct — one ECS Fargate control task (RW EFS) plus N worker
// tasks (RO EFS) sharing a single EFS filesystem and ECS cluster,
// built on top of the shared gobridgebase. Lives in its own
// sub-package (rather than directly under cdk/constructs) to avoid
// the import cycle constructs → gobridgebase → constructs (for
// GoBridgeEfsConfig type access). The top-level cdk/gobridgecdk
// facade re-exports the public surface; consumers should normally
// use that re-export rather than importing this package directly.
//
// NOT HIGH AVAILABILITY. Despite the name, GoBridgeCluster is a
// filesystem-replicated SCALE-OUT topology, not coordinated-failover
// HA: the replicas independently read one shared EFS config to scale
// throughput. It forces topology=filesystem_replicated, under which
// the runtime REJECTS the cross-instance coordination features
// (shared_outbox, route.session leases), so there is no single-active
// lease owner and no 30-60s failover path. Coordinated failover
// requires DynamoDB-backed lease/outbox stores, which are out of this
// reference construct's scope. See the
// GoBridgeCluster type doc for the full non-HA advisory.
package gobridgecluster

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsapplicationautoscaling"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// Non-HA honesty markers. GoBridgeCluster is a
// filesystem-replicated SCALE-OUT topology, not coordinated-failover
// HA. The tag keys/values below are stamped onto every taggable
// resource the construct synthesizes and DO reach the deployed stack,
// so an operator inspecting the deployed resources (not just this
// source) can see it is scale-out with no 30-60s coordinated-failover
// path. nonHAAdvisory is additionally surfaced as a synth-time info
// annotation — CloudAssembly metadata only, visible in verbose
// `cdk synth`/`deploy` output; it does NOT reach the deployed stack.
const (
	// tagKeyTopology / tagValueTopology label the deployment shape.
	tagKeyTopology   = "gobridge:topology"
	tagValueTopology = "filesystem-replicated-scale-out"
	// tagKeyHA / tagValueHA spell out that this is NOT HA. CloudFormation
	// tag values disallow spaces-only oddities but accept '-'; the value
	// is intentionally self-describing.
	tagKeyHA   = "gobridge:ha"
	tagValueHA = "none-scale-out-not-coordinated-failover"

	// nonHAAdvisory is the synth-time info annotation. It states, in one
	// place, exactly what this construct is and is not.
	nonHAAdvisory = "GoBridgeCluster is a filesystem-replicated SCALE-OUT topology " +
		"(N replicas share one EFS) and is NOT coordinated-failover HA: it forces " +
		"topology=filesystem_replicated, under which the runtime rejects shared_outbox " +
		"and route.session, so there is no single-active lease owner and no 30-60s " +
		"failover path. Coordinated-failover HA requires DynamoDB-backed lease/outbox " +
		"stores, which are out of this reference construct's scope."
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

	// Image is the gobridge runtime container image used by both
	// services. Required.
	Image awsecs.ContainerImage

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

	// SeederImage overrides the pinned aws-cli seeder image used
	// by BOTH services.
	SeederImage *string

	// ControlSeederMode overrides the control seeder MODE
	// (default "SeedOnce").
	ControlSeederMode *string

	// WorkerSeederMode overrides the worker seeder MODE (default
	// "AdoptValid" — workers adopt the current valid EFS bridge.yaml,
	// whether written by CDK seed or an Admin-API config-txn commit,
	// rather than aborting on hash drift from the synth-time asset).
	// Set "AbortDeploy" for strict lock-step deployments where every
	// worker must match the synth-time asset exactly. See the profile
	// README "Reconfiguration paths" section for the coexistence
	// semantics.
	WorkerSeederMode *string

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

// GoBridgeCluster is the L2 facade construct that deploys the
// clustered profile of gobridge: one control Fargate task with RW
// EFS mount plus N worker Fargate tasks with RO EFS mount, sharing
// a single EFS filesystem (one control access point, one worker
// access point) and a single ECS cluster. It is a thin wrapper
// over two [gobridgebase] instances — all task-def, EFS, IAM,
// asset and seeder machinery is owned by the shared base; this
// construct only adds the surrounding ECS services, security
// groups, EFS ingress rules and runs the Phase 1 / Phase 2 tier-B
// validators on the resolved BridgeConfig.
//
// ⚠️ SCALE-OUT, NOT HIGH AVAILABILITY. "Cluster" here means
// filesystem-replicated SCALE-OUT — N replicas independently reading
// one shared EFS config to scale throughput — NOT coordinated-failover
// HA. This construct FORCES topology=filesystem_replicated, and under
// that topology the runtime REJECTS the cross-instance coordination
// features (shared_outbox and route.session leases) via
// validateFilesystemProfile. Consequences an operator MUST understand:
//
//   - There is NO single-active lease owner and NO coordinated failover.
//     The replicas do not fail over for each other; they are independent
//     readers of the same config.
//   - There is NO 30-60s failover path. This deployment does NOT meet a
//     30-60s (or any bounded) coordinated-failover target.
//   - A config that declares shared_outbox or route.session is REJECTED
//     at synth (Phase 1) or by the runtime guard — deploying it here
//     yields config rejection, not the coordinated behaviour the names
//     imply.
//
// Coordinated-failover HA requires DynamoDB-backed lease/outbox stores
// (see ARCHITECTURE.md "clustered deployment for high availability"),
// which are OUT OF SCOPE for this reference construct. To keep this
// honest at the deployed-resource level (not just in source), the
// construct stamps gobridge:topology / gobridge:ha tags onto every
// taggable resource and emits a synth-time info annotation stating the
// non-HA nature.
//
// The control DesiredCount is hard-coded to 1 and NOT exposed as a
// prop: it is a runtime invariant of the single-LeaseStore-writer
// semantics documented in the design. The control deployment
// strategy is MinHealthyPercent=0 / MaxHealthyPercent=100 so the
// previous control task is fully drained before the new one starts
// — eliminating concurrent EFS RW writers during rolling deploys.
// Worker deployments use the standard CDK rolling defaults.
type GoBridgeCluster struct {
	constructs.Construct

	controlBase *gobridgebase.Built
	workerBase  *gobridgebase.Built
	control     awsecs.FargateService
	worker      awsecs.FargateService
	cluster     awsecs.ICluster
	efsConfig   *cdkconstructs.GoBridgeEfsConfig
	controlSG   awsec2.ISecurityGroup
	workerSG    awsec2.ISecurityGroup
}

// NewGoBridgeCluster constructs a [GoBridgeCluster] under scope/id.
//
// Construction order:
//
//  1. Validate required props (panic with a clear message on miss).
//  2. Materialize the BridgeConfig source once and run Phase 1
//     fast-fail validation against it, using the worker NodeRole so
//     worker-specific checks fire. On failure: panic.
//  3. Build (or reuse) the EFS config and ECS cluster — both
//     shared between control and worker services.
//  4. Delegate task-def + IAM + asset + seeder construction to two
//     separate [gobridgebase.New] calls — one in CONTROL mode, one
//     in WORKER mode — sharing the same EFS config (different
//     access points selected per mode).
//  5. Create the control FargateService (DesiredCount=1, 0/100
//     deployment) and the worker FargateService (DesiredCount=2
//     default, standard rolling deployment, optional CPU
//     autoscaling).
//  6. Wire EFS NFS ingress from each task SG to the EFS SG.
//  7. Run Phase 2 aggregated validation via CDK Annotations so a
//     single synth surfaces every missing registry reference.
//
// TODO: synth-time scope scan to enforce singleton
// constraint — error if multiple GoBridgeSingle / GoBridgeCluster
// siblings exist in the same Stack tree.
func NewGoBridgeCluster(scope constructs.Construct, id *string, props *ClusterProps) *GoBridgeCluster {
	if props == nil {
		panic("GoBridgeCluster: props must not be nil")
	}
	validateClusterProps(props)

	c := constructs.NewConstruct(scope, id)

	// Bootstrap copies — never mutate props.Bootstrap in place.
	bootstrapControl := props.Bootstrap
	bootstrapControl.NodeRole = infra.NodeRoleControl
	bootstrapWorker := props.Bootstrap
	bootstrapWorker.NodeRole = infra.NodeRoleWorker

	// A GoBridgeCluster is, by construction, a multi-instance topology:
	// one control + N workers share a single EFS filesystem. Force
	// Topology=filesystem_replicated on both copies (regardless of what the
	// caller left in props.Bootstrap, whose zero value normalizes to
	// "single"). This is load-bearing: both the fast-fail synth validator and
	// the runtime guard (lib/bootstrap.validateFilesystemProfile) return
	// early on the "single" topology, so leaving the default in place would
	// silently permit shared_outbox routes and route.session leases on
	// SQLite-over-EFS — the exact cross-instance corruption those guards
	// exist to prevent. Forcing it here makes the guards actually fire.
	bootstrapControl.Topology = infra.TopologyFilesystemReplicated
	bootstrapWorker.Topology = infra.TopologyFilesystemReplicated

	// Be HONEST about what this construct is. The name
	// "Cluster" can read as "HA / coordinated failover", but forcing
	// filesystem_replicated above makes this a SCALE-OUT topology: N
	// replicas independently read one EFS config, there is no single-active
	// lease owner, and there is no 30-60s failover path. Stamp that truth
	// onto the synthesized stack — tags on every taggable child plus a
	// synth-time info annotation — so an operator reading the deployed
	// resources (not just this source) does not deploy expecting a
	// coordinated failover that does not exist here.
	awscdk.Tags_Of(c).Add(jsii.String(tagKeyTopology), jsii.String(tagValueTopology), nil)
	awscdk.Tags_Of(c).Add(jsii.String(tagKeyHA), jsii.String(tagValueHA), nil)
	awscdk.Annotations_Of(c).AddInfo(jsii.String(nonHAAdvisory))

	// Phase 1 — fast-fail tier-B validation on the resolved config.
	// Use the worker NodeRole so worker-specific checks fire (a
	// strict superset for our purposes; the control task is gated
	// by the same yaml).
	mat, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic(fmt.Sprintf("GoBridgeCluster: materialize bridge config: %v", err))
	}
	mountPath := ""
	if props.MountPath != nil {
		mountPath = *props.MountPath
	}
	if err := validation.Phase1(validation.Phase1Input{
		Materialized: mat,
		Bootstrap:    bootstrapWorker,
		MountPath:    mountPath,
		NodeRole:     infra.NodeRoleWorker,
	}); err != nil {
		_ = mat.Close()
		panic(fmt.Sprintf("GoBridgeCluster: Phase 1 validation failed: %v", err))
	}
	_ = mat.Close()

	// Shared EFS config — auto-create when not supplied. Used by
	// BOTH services (control access point for control, worker
	// access point for worker). An auto-created config gets
	// props.VpcSubnets verbatim, so its mount targets always cover the
	// ECS placement; a SUPPLIED one may not, and a task in an AZ without
	// a mount target fails at container start (matrix row 14).
	efsConfig := props.EfsConfig
	if efsConfig == nil {
		efsConfig = cdkconstructs.NewGoBridgeEfsConfig(c, jsii.String("Efs"), &cdkconstructs.GoBridgeEfsConfigProps{
			Vpc:        props.Vpc,
			VpcSubnets: props.VpcSubnets,
			EfsKmsKey:  props.EfsKmsKey,
		})
	} else {
		cdkconstructs.AssertEfsSubnetParity("GoBridgeCluster", props.Vpc, props.VpcSubnets, efsConfig)
	}

	// Shared ECS cluster — auto-create when not supplied.
	cluster := props.Cluster
	if cluster == nil {
		cluster = awsecs.NewCluster(c, jsii.String("Cluster"), &awsecs.ClusterProps{
			Vpc:                 props.Vpc,
			ContainerInsightsV2: awsecs.ContainerInsights_ENABLED,
		})
	}

	// Control base (CONTROL mode → RW EFS mount, SeedOnce seeder).
	controlBuilt := gobridgebase.New(c, jsii.String("ControlBase"), &gobridgebase.Props{
		Mode:             gobridgebase.ModeControl,
		Vpc:              props.Vpc,
		EfsConfig:        efsConfig,
		EfsKmsKey:        props.EfsKmsKey,
		Image:            props.Image,
		Bootstrap:        bootstrapControl,
		Source:           props.BridgeConfig,
		QueueRegistry:    props.QueueRegistry,
		SsmRegistry:      props.SsmParamRegistry,
		CPU:              props.CPU,
		MemoryMiB:        props.MemoryMiB,
		MountPath:        props.MountPath,
		LogRetention:     props.LogRetention,
		LogRemovalPolicy: props.LogRemovalPolicy,
		SeederImage:      props.SeederImage,
		SeederMode:       props.ControlSeederMode,
	})

	// Worker base (WORKER mode → RO EFS mount, AdoptValid seeder).
	workerBuilt := gobridgebase.New(c, jsii.String("WorkerBase"), &gobridgebase.Props{
		Mode:             gobridgebase.ModeWorker,
		Vpc:              props.Vpc,
		EfsConfig:        efsConfig,
		EfsKmsKey:        props.EfsKmsKey,
		Image:            props.Image,
		Bootstrap:        bootstrapWorker,
		Source:           props.BridgeConfig,
		QueueRegistry:    props.QueueRegistry,
		SsmRegistry:      props.SsmParamRegistry,
		CPU:              props.CPU,
		MemoryMiB:        props.MemoryMiB,
		MountPath:        props.MountPath,
		LogRetention:     props.LogRetention,
		LogRemovalPolicy: props.LogRemovalPolicy,
		SeederImage:      props.SeederImage,
		WorkerSeederMode: props.WorkerSeederMode,
	})

	// Per-service security groups.
	controlSG := props.ControlSecurityGroup
	if controlSG == nil {
		controlSG = awsec2.NewSecurityGroup(c, jsii.String("ControlSG"), &awsec2.SecurityGroupProps{
			Vpc:         props.Vpc,
			Description: jsii.String("gobridge control task"),
		})
	}
	workerSG := props.WorkerSecurityGroup
	if workerSG == nil {
		workerSG = awsec2.NewSecurityGroup(c, jsii.String("WorkerSG"), &awsec2.SecurityGroupProps{
			Vpc:         props.Vpc,
			Description: jsii.String("gobridge worker task"),
		})
	}

	// EFS NFS ingress from each task SG to the EFS SG (only when
	// the EFS construct owns its SG — consumer-supplied SGs are
	// out of scope).
	if efsSG := efsConfig.SecurityGroup(); efsSG != nil {
		efsSG.AddIngressRule(
			controlSG,
			awsec2.Port_Tcp(jsii.Number(2049)),
			jsii.String("gobridge control task NFS access"),
			jsii.Bool(false),
		)
		efsSG.AddIngressRule(
			workerSG,
			awsec2.Port_Tcp(jsii.Number(2049)),
			jsii.String("gobridge worker task NFS access"),
			jsii.Bool(false),
		)
	}

	// Control service: DesiredCount=1 hard-coded + 0/100 deployment.
	controlSvcProps := &awsecs.FargateServiceProps{
		Cluster:              cluster,
		TaskDefinition:       controlBuilt.TaskDefinition,
		DesiredCount:         jsii.Number(1),
		MinHealthyPercent:    jsii.Number(0),
		MaxHealthyPercent:    jsii.Number(100),
		SecurityGroups:       &[]awsec2.ISecurityGroup{controlSG},
		EnableExecuteCommand: jsii.Bool(false),
		CircuitBreaker:       &awsecs.DeploymentCircuitBreaker{Rollback: jsii.Bool(true)},
	}
	if props.VpcSubnets != nil {
		controlSvcProps.VpcSubnets = props.VpcSubnets
	}
	if props.ControlServiceName != nil {
		controlSvcProps.ServiceName = props.ControlServiceName
	}
	controlSvc := awsecs.NewFargateService(c, jsii.String("ControlService"), controlSvcProps)

	// Worker service: DesiredCount=2 default, standard rolling
	// deployment (no MinHealthyPercent/MaxHealthyPercent overrides).
	workerDesired := 2.0
	if props.WorkerDesiredCount != nil {
		workerDesired = *props.WorkerDesiredCount
	}
	workerSvcProps := &awsecs.FargateServiceProps{
		Cluster:              cluster,
		TaskDefinition:       workerBuilt.TaskDefinition,
		DesiredCount:         jsii.Number(workerDesired),
		SecurityGroups:       &[]awsec2.ISecurityGroup{workerSG},
		EnableExecuteCommand: jsii.Bool(false),
		CircuitBreaker:       &awsecs.DeploymentCircuitBreaker{Rollback: jsii.Bool(true)},
	}
	if props.VpcSubnets != nil {
		workerSvcProps.VpcSubnets = props.VpcSubnets
	}
	if props.WorkerServiceName != nil {
		workerSvcProps.ServiceName = props.WorkerServiceName
	}
	workerSvc := awsecs.NewFargateService(c, jsii.String("WorkerService"), workerSvcProps)

	// Optional worker autoscaling.
	if props.AutoScaling != nil {
		minCap := props.AutoScaling.Min
		maxCap := props.AutoScaling.Max
		target := props.AutoScaling.TargetCPU
		if target == 0 {
			target = 70
		}
		scaling := workerSvc.AutoScaleTaskCount(&awsapplicationautoscaling.EnableScalingProps{
			MinCapacity: jsii.Number(minCap),
			MaxCapacity: jsii.Number(maxCap),
		})
		scaling.ScaleOnCpuUtilization(jsii.String("CpuScaling"), &awsecs.CpuUtilizationScalingProps{
			TargetUtilizationPercent: jsii.Number(target),
		})
	}

	// Phase 2 — aggregated validation via CDK Annotations on the
	// re-materialized config.
	mat2, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic("GoBridgeCluster: Phase 2 re-materialize failed: " + err.Error())
	}
	defer func() { _ = mat2.Close() }()
	validation.RunPhase2(c, validation.Phase2Input{
		Cfg:              mat2.Config,
		QueueRegistry:    props.QueueRegistry,
		SsmParamRegistry: props.SsmParamRegistry,
	})

	facade := &GoBridgeCluster{
		Construct:   c,
		controlBase: controlBuilt,
		workerBase:  workerBuilt,
		control:     controlSvc,
		worker:      workerSvc,
		cluster:     cluster,
		efsConfig:   efsConfig,
		controlSG:   controlSG,
		workerSG:    workerSG,
	}
	// Pass the inner construct (not the facade) because jsii rejects
	// re-embedding a constructs.Construct proxy in another Go value
	// when that value has not been registered with the jsii runtime.
	singleton.Register(c, "cluster")
	singleton.Enforce(c)
	return facade
}

// ControlService returns the underlying control ECS Fargate service
// as the [awsecs.IService] interface per design contract; consumers
// needing the concrete type can type-assert.
func (g *GoBridgeCluster) ControlService() awsecs.IService { return g.control }

// WorkerService returns the underlying worker ECS Fargate service
// as the [awsecs.IService] interface per design contract; consumers
// needing the concrete type can type-assert.
func (g *GoBridgeCluster) WorkerService() awsecs.IService { return g.worker }

// ControlTaskDefinition returns the control Fargate task definition
// built by the shared base.
func (g *GoBridgeCluster) ControlTaskDefinition() awsecs.FargateTaskDefinition {
	return g.controlBase.TaskDefinition
}

// WorkerTaskDefinition returns the worker Fargate task definition
// built by the shared base.
func (g *GoBridgeCluster) WorkerTaskDefinition() awsecs.FargateTaskDefinition {
	return g.workerBase.TaskDefinition
}

// Cluster returns the ECS cluster both services run in (either the
// one supplied via ClusterProps.Cluster or the auto-created one).
func (g *GoBridgeCluster) Cluster() awsecs.ICluster { return g.cluster }

// EfsConfig returns the EFS configuration shared by both services
// (either the supplied one or the auto-created default).
func (g *GoBridgeCluster) EfsConfig() *cdkconstructs.GoBridgeEfsConfig { return g.efsConfig }

// ControlSecurityGroup returns the security group attached to the
// control Fargate service.
func (g *GoBridgeCluster) ControlSecurityGroup() awsec2.ISecurityGroup { return g.controlSG }

// WorkerSecurityGroup returns the security group attached to the
// worker Fargate service.
func (g *GoBridgeCluster) WorkerSecurityGroup() awsec2.ISecurityGroup { return g.workerSG }

// ControlPortMappings returns the container port mappings derived
// by the shared base for the control task.
func (g *GoBridgeCluster) ControlPortMappings() []gobridgebase.PortMapping {
	return g.controlBase.PortMappings
}

// WorkerPortMappings returns the container port mappings derived
// by the shared base for the worker task.
func (g *GoBridgeCluster) WorkerPortMappings() []gobridgebase.PortMapping {
	return g.workerBase.PortMappings
}

func validateClusterProps(p *ClusterProps) {
	if p.Vpc == nil {
		panic("GoBridgeCluster: Vpc is required")
	}
	if p.Image == nil {
		panic("GoBridgeCluster: Image is required")
	}
	if p.BridgeConfig == nil {
		panic("GoBridgeCluster: BridgeConfig is required (use gobridgecdk.BridgeYamlAsset / BridgeYamlInline)")
	}
	if p.WorkerDesiredCount != nil && *p.WorkerDesiredCount < 1 {
		panic("GoBridgeCluster: WorkerDesiredCount must be >= 1 when set")
	}
	if p.AutoScaling != nil {
		if p.AutoScaling.Min < 1 {
			panic("GoBridgeCluster: AutoScaling.Min must be >= 1")
		}
		if p.AutoScaling.Max < p.AutoScaling.Min {
			panic("GoBridgeCluster: AutoScaling.Max must be >= AutoScaling.Min")
		}
	}
}
