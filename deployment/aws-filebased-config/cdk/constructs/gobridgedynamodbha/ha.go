package gobridgedynamodbha

import (
	"fmt"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/bridge"
	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const (
	tagKeyTopology   = "gobridge:topology"
	tagValueTopology = "dynamodb-coordinated-ha"
	tagKeyHA         = "gobridge:ha"
	tagValueHA       = "active-warm-standby"
	// tagKeyConfigRollout advertises HOW a config change reaches this fleet, so an
	// operator reading the deployed resources — not this source — knows the profile
	// replaces the whole cohort and has no coordinated live rollout.
	tagKeyConfigRollout   = "gobridge:config-rollout"
	tagValueConfigRollout = "whole-cohort-replacement"
)

// DynamoDBHAProps configures GoBridgeDynamoDBHA. The supplied bridge config is
// one shared artifact for every task and must already declare clustered mode,
// the three DynamoDB stores, stable Exclusive MQTT identity, shared_outbox, and
// an explicit Task 9-admissible failover objective.
type DynamoDBHAProps struct {
	Vpc        awsec2.IVpc
	VpcSubnets *awsec2.SubnetSelection
	Cluster    awsecs.ICluster

	EfsConfig *cdkconstructs.GoBridgeEfsConfig
	EfsKmsKey awskms.IKey

	Image        awsecs.ContainerImage
	Bootstrap    infra.BootstrapConfig
	BridgeConfig source.Source
	// ManagedSubscriptionBaselines attests the known broker-side filters for
	// every Exclusive receiver session. An explicit empty slice means the
	// broker identity is new and has no historical subscriptions.
	ManagedSubscriptionBaselines map[string][]string

	QueueRegistry    *registry.QueueRegistry
	SsmParamRegistry *registry.SsmParamRegistry

	ControlSecurityGroup awsec2.ISecurityGroup
	WorkerSecurityGroup  awsec2.ISecurityGroup

	CPU       *float64
	MemoryMiB *float64
	MountPath *string

	LogRetention     awslogs.RetentionDays
	LogRemovalPolicy awscdk.RemovalPolicy
	SeederImage      *string

	ControlSeederMode *string
	WorkerSeederMode  *string

	ControlServiceName *string
	WorkerServiceName  *string

	// MemberSlots opts this deployment into the static member-slot profile: one
	// single-task ECS service per coordinated-rollout cohort member, each carrying
	// a restart-stable member_id. It is the only shipped shape that can host the
	// coordinated rollout barrier. Nil (the default) deploys interchangeable
	// autoscaled workers, which reject a coordinated config at synth.
	//
	// It is mutually exclusive with WorkerDesiredCount: the roster IS the slot
	// count, and scaling a slot's service past one task would give one member_id to
	// two running processes.
	MemberSlots *MemberSlots

	// WorkerDesiredCount defaults to two and may never be less than two. With
	// the one control task this leaves at least one continuously polling warm
	// standby after any single task loss. It applies only to the autoscaled
	// profile; see MemberSlots for the static member-slot alternative.
	//
	// The warm-standby invariant covers task LOSS, not deployment: config changes
	// deploy by whole-cohort replacement (MinHealthyPercent=0), so the worker
	// cohort is expected to reach zero running tasks during every deploy, and the
	// warm-standby alarm breaches for that window by design. Availability Zone
	// rebalancing is disabled for the same reason, so AZ spread is best-effort at
	// launch rather than continuously maintained.
	WorkerDesiredCount *float64
}

// GoBridgeDynamoDBHA deploys one config-control task and at least two workers
// as a coordinated active/warm-standby fleet. All tasks run the same clustered
// runtime and may hold the DynamoDB lease; control versus worker only governs
// EFS config-write authority. The task definitions are built exclusively by
// internal/gobridgebase.New.
type GoBridgeDynamoDBHA struct {
	constructs.Construct

	controlBase *gobridgebase.Built
	workerBases []*gobridgebase.Built
	control     awsecs.FargateService
	workers     []awsecs.FargateService
	cluster     awsecs.ICluster
	efsConfig   *cdkconstructs.GoBridgeEfsConfig
	data        *DynamoDBHAData
	controlSG   awsec2.ISecurityGroup
	workerSG    awsec2.ISecurityGroup
	memberSlots []string

	rolloutTableName string

	failoverObjective time.Duration
	metricsNamespace  string
}

// NewGoBridgeDynamoDBHA creates the DynamoDB-coordinated HA facade.
func NewGoBridgeDynamoDBHA(scope constructs.Construct, id *string, props *DynamoDBHAProps) *GoBridgeDynamoDBHA {
	validateProps(props)
	c := constructs.NewConstruct(scope, id)

	workerDesired := 2.0
	if props.WorkerDesiredCount != nil {
		workerDesired = *props.WorkerDesiredCount
	}

	bootstrapControl := props.Bootstrap
	bootstrapControl.NodeRole = infra.NodeRoleControl
	bootstrapControl.Topology = infra.TopologyDynamoDBCoordinatedHA
	bootstrapControl.MetricsExporter = infra.MetricsExporterCloudWatch
	bootstrapControl.InstanceID = ""
	// The cohort identity is the DEPLOYMENT's to assign, never the caller's: it is
	// stamped per slot below, and must stay empty on every task that is not one.
	// Scrubbing it here (like InstanceID) keeps a caller-supplied value from
	// reaching the control task of an autoscaled deployment, where it would name a
	// cohort seat that does not exist.
	bootstrapControl.MemberID = ""
	bootstrapWorker := bootstrapControl
	bootstrapWorker.NodeRole = infra.NodeRoleWorker

	mat, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic(fmt.Sprintf("GoBridgeDynamoDBHA: materialize bridge config: %v", err))
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
		panic(fmt.Sprintf("GoBridgeDynamoDBHA: Phase 1 validation failed: %v", err))
	}
	inspected, err := inspectHAConfig(mat.Config, props.ManagedSubscriptionBaselines, props.MemberSlots)
	if err != nil {
		_ = mat.Close()
		panic(fmt.Sprintf("GoBridgeDynamoDBHA: invalid coordinated HA config: %v", err))
	}
	// Two distinct identities are stamped from the SAME materialized document.
	//
	// The deployment-profile fingerprint covers only the fields this construct
	// PROVISIONS (topology, cohort shape, the deployment-owned store identities).
	// It must survive every later config change an operator commits,
	// which is why it is not a hash of the whole document: doing that made every
	// real change fail admission on every member after the cohort committed it.
	//
	// The baseline digest is the full content identity of THIS document, and only
	// this one. A coordinated member uses it to seed the cohort's generation-zero
	// committed artifact at boot, so a restart before the first rollout recovers to
	// the config this deployment admitted rather than to whatever the mutable EFS
	// document happens to hold.
	fingerprint := bridge.DeploymentProfileFingerprint(mat.Config)
	baseline, err := bridge.ConfigArtifactDigest(mat.Config)
	_ = mat.Close()
	if err != nil {
		panic(fmt.Sprintf("GoBridgeDynamoDBHA: digest coordinated HA config: %v", err))
	}
	bootstrapControl.DynamoDBHALeaseTableName = inspected.tables.lease
	bootstrapControl.DynamoDBHAOutboxTableName = inspected.tables.outbox
	bootstrapControl.DynamoDBHAManagedSubscriptionsTableName = inspected.tables.managedSubscriptions
	bootstrapControl.DynamoDBHAConfigFingerprint = fingerprint
	bootstrapControl.DynamoDBHABaselineConfigDigest = baseline
	// The rollout coordination table and the slot identity are stamped only on the
	// static member-slot profile. On the autoscaled profile both stay empty, so an
	// interchangeable worker cannot even name a cohort to join.
	bootstrapControl.DynamoDBHARolloutTableName = inspected.tables.rollout
	if props.MemberSlots != nil {
		bootstrapControl.MemberID = props.MemberSlots.ControlMemberID
	}
	bootstrapWorker = bootstrapControl
	bootstrapWorker.NodeRole = infra.NodeRoleWorker
	bootstrapWorker.MemberID = ""

	selectedSubnets := props.VpcSubnets
	if selectedSubnets == nil {
		selectedSubnets = &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PRIVATE_WITH_EGRESS}
	}
	requireTwoAvailabilityZones(props.Vpc, selectedSubnets)

	efsConfig := props.EfsConfig
	if efsConfig == nil {
		efsConfig = cdkconstructs.NewGoBridgeEfsConfig(c, jsii.String("Efs"), &cdkconstructs.GoBridgeEfsConfigProps{
			Vpc:        props.Vpc,
			VpcSubnets: selectedSubnets,
			EfsKmsKey:  props.EfsKmsKey,
		})
	}

	cluster := props.Cluster
	if cluster == nil {
		cluster = awsecs.NewCluster(c, jsii.String("Cluster"), &awsecs.ClusterProps{
			Vpc:                 props.Vpc,
			ContainerInsightsV2: awsecs.ContainerInsights_ENABLED,
		})
	}

	data := newDynamoDBHAData(c, inspected.tables)
	data.managedSubscriptionInitializers = newManagedSubscriptionInitializers(
		c,
		data.managedSubscriptions,
		inspected.managedSubscriptionBaselines,
	)

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
	// One worker-side deployment unit per slot. The autoscaled profile has exactly
	// one, with no member identity; the static member-slot profile has one per
	// roster worker, each pinned to a single task and stamped with its own
	// restart-stable member_id — which is why each needs its OWN task definition.
	slotSpecs := workerSlotSpecs(props.MemberSlots, workerDesired)
	workerBases := make([]*gobridgebase.Built, 0, len(slotSpecs))
	for _, spec := range slotSpecs {
		slotBootstrap := bootstrapWorker
		slotBootstrap.MemberID = spec.memberID
		workerBases = append(workerBases, gobridgebase.New(c, jsii.String("WorkerBase"+spec.scopeSuffix), &gobridgebase.Props{
			Mode:             gobridgebase.ModeWorker,
			Vpc:              props.Vpc,
			EfsConfig:        efsConfig,
			EfsKmsKey:        props.EfsKmsKey,
			Image:            props.Image,
			Bootstrap:        slotBootstrap,
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
		}))
	}

	controlSG := props.ControlSecurityGroup
	if controlSG == nil {
		controlSG = awsec2.NewSecurityGroup(c, jsii.String("ControlSG"), &awsec2.SecurityGroupProps{
			Vpc:         props.Vpc,
			Description: jsii.String("gobridge DynamoDB HA control task"),
		})
	}
	workerSG := props.WorkerSecurityGroup
	if workerSG == nil {
		workerSG = awsec2.NewSecurityGroup(c, jsii.String("WorkerSG"), &awsec2.SecurityGroupProps{
			Vpc:         props.Vpc,
			Description: jsii.String("gobridge DynamoDB HA worker task"),
		})
	}
	if efsSG := efsConfig.SecurityGroup(); efsSG != nil {
		efsSG.AddIngressRule(controlSG, awsec2.Port_Tcp(jsii.Number(2049)), jsii.String("gobridge HA control NFS"), jsii.Bool(false))
		efsSG.AddIngressRule(workerSG, awsec2.Port_Tcp(jsii.Number(2049)), jsii.String("gobridge HA worker NFS"), jsii.Bool(false))
	}

	controlProps := &awsecs.FargateServiceProps{
		Cluster:                     cluster,
		TaskDefinition:              controlBuilt.TaskDefinition,
		DesiredCount:                jsii.Number(1),
		MinHealthyPercent:           jsii.Number(0),
		MaxHealthyPercent:           jsii.Number(100),
		VpcSubnets:                  selectedSubnets,
		SecurityGroups:              &[]awsec2.ISecurityGroup{controlSG},
		AvailabilityZoneRebalancing: awsecs.AvailabilityZoneRebalancing_DISABLED,
		EnableExecuteCommand:        jsii.Bool(false),
		CircuitBreaker:              &awsecs.DeploymentCircuitBreaker{Rollback: jsii.Bool(true)},
	}
	if props.ControlServiceName != nil {
		controlProps.ServiceName = props.ControlServiceName
	}
	control := awsecs.NewFargateService(c, jsii.String("ControlService"), controlProps)

	// Whole-cohort replacement (ADR 0012). The ECS rolling-update default (100/200)
	// runs a complete second cohort beside the first, so a task-definition change
	// that alters durable MQTT session identity or store targets would put two
	// mutually incompatible generations on the same broker and the same outbox
	// partitions at once — split ownership, or backlog stranded under keys nobody
	// drains. 0/100 removes the lower bound and caps total tasks at the desired
	// count, so that second cohort cannot exist.
	//
	// It is NOT by itself a non-overlap proof: the property constrains counts, not
	// order, so at a desired count of two or more the scheduler may still replace
	// in batches. An identity- or store-incompatible revision therefore keeps the
	// operator-run scale-to-zero procedure (docs/runbooks/cluster-config-rollout.md);
	// this narrows the window, the runbook closes it. wholeCohortAdvisory states
	// both halves at synth time.
	//
	// AvailabilityZoneRebalancing must be DISABLED with it: rebalancing replaces a
	// task by starting its replacement first, which needs headroom above the
	// desired count that MaxHealthyPercent=100 does not grant. That costs
	// continuous AZ redistribution — spread becomes best-effort at launch — on top
	// of an ingress gap for the length of a worker replacement.
	workers := make([]awsecs.FargateService, 0, len(slotSpecs))
	for i, spec := range slotSpecs {
		workerProps := &awsecs.FargateServiceProps{
			Cluster:                     cluster,
			TaskDefinition:              workerBases[i].TaskDefinition,
			DesiredCount:                jsii.Number(spec.desired),
			MinHealthyPercent:           jsii.Number(0),
			MaxHealthyPercent:           jsii.Number(100),
			VpcSubnets:                  selectedSubnets,
			SecurityGroups:              &[]awsec2.ISecurityGroup{workerSG},
			AvailabilityZoneRebalancing: awsecs.AvailabilityZoneRebalancing_DISABLED,
			EnableExecuteCommand:        jsii.Bool(false),
			CircuitBreaker:              &awsecs.DeploymentCircuitBreaker{Rollback: jsii.Bool(true)},
		}
		if props.WorkerServiceName != nil {
			// A caller-pinned physical name can only ever name ONE service. With static
			// member slots there are several, so each takes the pinned name as a prefix
			// and its own slot id as the suffix, keeping the names both stable and
			// distinct.
			workerProps.ServiceName = jsii.String(*props.WorkerServiceName + spec.scopeSuffix)
		}
		workers = append(workers, awsecs.NewFargateService(c, jsii.String("WorkerService"+spec.scopeSuffix), workerProps))
	}

	// Order the worker replacement AFTER the control service reaches steady state.
	// Only the control task's seeder writes bridge.yaml onto EFS, and every task
	// refuses to boot a config whose fingerprint does not match the one stamped
	// into its own task definition (lib/bootstrap.validateDynamoDBHAProfile). With
	// both services updating concurrently, new workers would boot against the
	// still-old EFS config, fail the fingerprint check, and trip the deployment
	// circuit breaker — while the new control task seeded the NEW config, so the
	// rolled-back old workers would fail their fingerprint too. The overlapping
	// deployment policy used to hide that race behind a surviving old cohort; at
	// 0/100 it does not, so this ordering is what keeps a failed deploy
	// recoverable.
	services := append([]awsecs.FargateService{control}, workers...)
	for i, worker := range workers {
		worker.Node().AddDependency(control)
		if i > 0 {
			// Chain the slots. Every slot runs ONE task at MinimumHealthyPercent=0, so
			// it stops that task before starting its replacement. CloudFormation
			// updates independent resources in parallel, so without this chain a
			// task-definition change (an image bump, a store identity, the roster)
			// would take every slot down at the same instant — the same full ingress
			// gap the autoscaled profile has, from a profile whose whole point is that
			// members are individually addressable. Chained, at most one slot is down
			// at a time; the price is a deploy that is linear in roster size.
			worker.Node().AddDependency(workers[i-1])
		}
	}

	for _, service := range services {
		service.Node().AddDependency(data.lease)
		service.Node().AddDependency(data.outbox)
		service.Node().AddDependency(data.managedSubscriptions)
		if data.rollout != nil {
			// A slot that boots before its coordination store exists fails boot
			// resolution: the joiner's read is the authoritative gate on the rollout
			// store, so an absent table is a refusal to start, not a degraded start.
			service.Node().AddDependency(data.rollout)
		}
		for _, initializer := range data.managedSubscriptionInitializers {
			service.Node().AddDependency(initializer)
		}
	}

	// Exactly the calls the rollout store makes, on exactly the deployment-owned
	// table, for every task role in the cohort. The barrier runs on every member —
	// control included — because any of them can be elected coordinator.
	if data.rollout != nil {
		grants.GrantDynamoDBRolloutStore(controlBuilt.TaskDefinition.TaskRole(), data.rollout)
		for _, base := range workerBases {
			grants.GrantDynamoDBRolloutStore(base.TaskDefinition.TaskRole(), data.rollout)
		}
	}

	mat2, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic("GoBridgeDynamoDBHA: Phase 2 re-materialize failed: " + err.Error())
	}
	defer func() { _ = mat2.Close() }()
	validation.RunPhase2(c, validation.Phase2Input{
		Cfg:              mat2.Config,
		QueueRegistry:    props.QueueRegistry,
		SsmParamRegistry: props.SsmParamRegistry,
	})

	rolloutCapability, advisory := tagValueConfigRollout, wholeCohortAdvisory
	if props.MemberSlots != nil {
		rolloutCapability, advisory = tagValueStaticSlotRollout, staticSlotAdvisory
	}
	awscdk.Tags_Of(c).Add(jsii.String(tagKeyTopology), jsii.String(tagValueTopology), nil)
	awscdk.Tags_Of(c).Add(jsii.String(tagKeyHA), jsii.String(tagValueHA), nil)
	awscdk.Tags_Of(c).Add(jsii.String(tagKeyConfigRollout), jsii.String(rolloutCapability), nil)
	awscdk.Annotations_Of(c).AddInfo(jsii.String(advisory))

	facade := &GoBridgeDynamoDBHA{
		Construct:         c,
		controlBase:       controlBuilt,
		workerBases:       workerBases,
		control:           control,
		workers:           workers,
		memberSlots:       props.MemberSlots.memberSlotIDs(),
		rolloutTableName:  inspected.tables.rollout,
		cluster:           cluster,
		efsConfig:         efsConfig,
		data:              data,
		controlSG:         controlSG,
		workerSG:          workerSG,
		failoverObjective: inspected.failoverObjective,
		metricsNamespace:  bootstrapControl.EffectiveMetricsNamespace(),
	}
	singleton.Register(c, "dynamodb-coordinated-ha")
	singleton.Enforce(c)
	return facade
}
