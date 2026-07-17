package gobridgedynamodbha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	tagKeyTopology   = "gobridge:topology"
	tagValueTopology = "dynamodb-coordinated-ha"
	tagKeyHA         = "gobridge:ha"
	tagValueHA       = "active-warm-standby"
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

	// WorkerDesiredCount defaults to two and may never be less than two. With
	// the one control task this leaves at least one continuously polling warm
	// standby after any single task loss.
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
	workerBase  *gobridgebase.Built
	control     awsecs.FargateService
	worker      awsecs.FargateService
	cluster     awsecs.ICluster
	efsConfig   *cdkconstructs.GoBridgeEfsConfig
	data        *DynamoDBHAData
	controlSG   awsec2.ISecurityGroup
	workerSG    awsec2.ISecurityGroup

	failoverObjective time.Duration
	metricsNamespace  string
}

type inspectedHAConfig struct {
	tables                       dataTableNames
	failoverObjective            time.Duration
	managedSubscriptionBaselines []managedSubscriptionBaseline
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
	inspected, err := inspectHAConfig(mat.Config, props.ManagedSubscriptionBaselines)
	if err != nil {
		_ = mat.Close()
		panic(fmt.Sprintf("GoBridgeDynamoDBHA: invalid coordinated HA config: %v", err))
	}
	fingerprint, err := profileConfigFingerprint(mat.Config)
	_ = mat.Close()
	if err != nil {
		panic(fmt.Sprintf("GoBridgeDynamoDBHA: fingerprint coordinated HA config: %v", err))
	}
	bootstrapControl.DynamoDBHALeaseTableName = inspected.tables.lease
	bootstrapControl.DynamoDBHAOutboxTableName = inspected.tables.outbox
	bootstrapControl.DynamoDBHAManagedSubscriptionsTableName = inspected.tables.managedSubscriptions
	bootstrapControl.DynamoDBHAConfigFingerprint = fingerprint
	bootstrapWorker = bootstrapControl
	bootstrapWorker.NodeRole = infra.NodeRoleWorker

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

	workerProps := &awsecs.FargateServiceProps{
		Cluster:                     cluster,
		TaskDefinition:              workerBuilt.TaskDefinition,
		DesiredCount:                jsii.Number(workerDesired),
		MinHealthyPercent:           jsii.Number(100),
		MaxHealthyPercent:           jsii.Number(200),
		VpcSubnets:                  selectedSubnets,
		SecurityGroups:              &[]awsec2.ISecurityGroup{workerSG},
		AvailabilityZoneRebalancing: awsecs.AvailabilityZoneRebalancing_ENABLED,
		EnableExecuteCommand:        jsii.Bool(false),
		CircuitBreaker:              &awsecs.DeploymentCircuitBreaker{Rollback: jsii.Bool(true)},
	}
	if props.WorkerServiceName != nil {
		workerProps.ServiceName = props.WorkerServiceName
	}
	worker := awsecs.NewFargateService(c, jsii.String("WorkerService"), workerProps)

	for _, service := range []awsecs.FargateService{control, worker} {
		service.Node().AddDependency(data.lease)
		service.Node().AddDependency(data.outbox)
		service.Node().AddDependency(data.managedSubscriptions)
		for _, initializer := range data.managedSubscriptionInitializers {
			service.Node().AddDependency(initializer)
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

	awscdk.Tags_Of(c).Add(jsii.String(tagKeyTopology), jsii.String(tagValueTopology), nil)
	awscdk.Tags_Of(c).Add(jsii.String(tagKeyHA), jsii.String(tagValueHA), nil)

	facade := &GoBridgeDynamoDBHA{
		Construct:         c,
		controlBase:       controlBuilt,
		workerBase:        workerBuilt,
		control:           control,
		worker:            worker,
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

func inspectHAConfig(
	cfg *ports.BridgeConfig,
	declaredBaselines map[string][]string,
) (inspectedHAConfig, error) {
	if cfg == nil {
		return inspectedHAConfig{}, fmt.Errorf("bridge config is nil")
	}
	if err := config.Validate(cfg); err != nil {
		return inspectedHAConfig{}, err
	}
	if cfg.Bridge.DeploymentMode != "clustered" {
		return inspectedHAConfig{}, fmt.Errorf("bridge.deployment_mode must be clustered")
	}
	if cfg.Bridge.Cluster != nil && len(cfg.Bridge.Cluster.Endpoints) > 0 {
		return inspectedHAConfig{}, fmt.Errorf("bridge.cluster.endpoints must be omitted so every task advertises its ECS-resolved endpoint")
	}

	lease, err := requiredDynamoDBStore("lease", cfg.Stores.Lease)
	if err != nil {
		return inspectedHAConfig{}, err
	}
	outbox, err := requiredDynamoDBStore("outbox", cfg.Stores.Outbox)
	if err != nil {
		return inspectedHAConfig{}, err
	}
	history, err := requiredDynamoDBStore("managed_subscriptions", cfg.Stores.ManagedSubscriptions)
	if err != nil {
		return inspectedHAConfig{}, err
	}
	names := dataTableNames{lease: lease, outbox: outbox, managedSubscriptions: history}
	if lease == outbox || lease == history || outbox == history {
		return inspectedHAConfig{}, fmt.Errorf("lease, outbox, and managed_subscriptions must use three distinct tables")
	}

	exclusive := map[string]string{}
	for i := range cfg.Sessions {
		session := &cfg.Sessions[i]
		if session.SessionMode != string(connectivity.SessionExclusive) {
			continue
		}
		mqtt, ok := session.Config.(*paho.Config)
		if !ok || mqtt == nil || !paho.IsKind(session.Transport) {
			return inspectedHAConfig{}, fmt.Errorf("exclusive session %q must use the MQTT Paho config", session.ID)
		}
		if err := mqtt.ValidateEffectiveSession(connectivity.SessionExclusive); err != nil {
			return inspectedHAConfig{}, fmt.Errorf("exclusive session %q is not an effective stable MQTT session: %w", session.ID, err)
		}
		storageIdentity, err := mqtt.DurableSessionIdentity(connectivity.SessionExclusive)
		if err != nil {
			return inspectedHAConfig{}, fmt.Errorf(
				"exclusive session %q durable identity: %w",
				session.ID,
				err,
			)
		}
		exclusive[session.ID] = storageIdentity
	}
	if len(exclusive) == 0 {
		return inspectedHAConfig{}, fmt.Errorf("at least one Exclusive MQTT session is required")
	}
	managedSubscriptionBaselines, err := validateManagedSubscriptionBaselines(
		exclusive,
		declaredBaselines,
	)
	if err != nil {
		return inspectedHAConfig{}, err
	}

	var objective time.Duration
	managedRoutes := 0
	coveredSessions := map[string]struct{}{}
	receiverSessions := make(map[string]string, len(cfg.Receivers))
	for i := range cfg.Receivers {
		receiverSessions[cfg.Receivers[i].ID] = cfg.Receivers[i].SessionID
	}
	bindingSessions := make(map[string]string, len(cfg.Bindings))
	for i := range cfg.Bindings {
		bindingSessions[cfg.Bindings[i].ID] = cfg.Bindings[i].SessionID
	}
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		referencedExclusive := ""
		if sessionID := receiverSessions[route.ReceiverID]; sessionID != "" {
			if _, ok := exclusive[sessionID]; ok {
				referencedExclusive = sessionID
			}
		}
		for _, bindingID := range route.Bindings {
			sessionID := bindingSessions[bindingID]
			if _, ok := exclusive[sessionID]; !ok {
				continue
			}
			if referencedExclusive != "" && referencedExclusive != sessionID {
				return inspectedHAConfig{}, fmt.Errorf("route %q references multiple Exclusive sessions", route.ID)
			}
			referencedExclusive = sessionID
		}
		if route.Session == nil {
			if referencedExclusive != "" {
				return inspectedHAConfig{}, fmt.Errorf("route %q references Exclusive session %q but has no explicit route.session failover budget", route.ID, referencedExclusive)
			}
			continue
		}
		if _, ok := exclusive[route.Session.SessionID]; !ok {
			return inspectedHAConfig{}, fmt.Errorf("route %q session %q is not an Exclusive MQTT session", route.ID, route.Session.SessionID)
		}
		if referencedExclusive != "" && referencedExclusive != route.Session.SessionID {
			return inspectedHAConfig{}, fmt.Errorf("route %q route.session %q diverges from referenced Exclusive session %q", route.ID, route.Session.SessionID, referencedExclusive)
		}
		coveredSessions[route.Session.SessionID] = struct{}{}
		managedRoutes++
		if route.DeliveryMode != "shared_outbox" {
			return inspectedHAConfig{}, fmt.Errorf("route %q must use delivery_mode: shared_outbox", route.ID)
		}
		if route.Policy.AckAfter != "outbox_persist" {
			return inspectedHAConfig{}, fmt.Errorf("route %q must use policy.ack_after: outbox_persist", route.ID)
		}
		if route.Session.FailoverSLO == "" || route.Session.StartupAllowance == "" {
			return inspectedHAConfig{}, fmt.Errorf("route %q requires explicit failover_slo and startup_allowance", route.ID)
		}
		parsed, parseErr := time.ParseDuration(route.Session.FailoverSLO)
		if parseErr != nil || parsed <= 0 {
			return inspectedHAConfig{}, fmt.Errorf("route %q has invalid failover_slo %q", route.ID, route.Session.FailoverSLO)
		}
		if objective == 0 {
			objective = parsed
		} else if parsed != objective {
			return inspectedHAConfig{}, fmt.Errorf("all coordinated routes must declare the same profile failover_slo")
		}
	}
	if managedRoutes == 0 {
		return inspectedHAConfig{}, fmt.Errorf("at least one lease-managed shared_outbox route is required")
	}
	for sessionID := range exclusive {
		if _, ok := coveredSessions[sessionID]; !ok {
			return inspectedHAConfig{}, fmt.Errorf("exclusive MQTT session %q is not covered by a lease-managed route.session", sessionID)
		}
	}

	if err := validateTask9Admission(cfg); err != nil {
		return inspectedHAConfig{}, err
	}
	return inspectedHAConfig{
		tables:                       names,
		failoverObjective:            objective,
		managedSubscriptionBaselines: managedSubscriptionBaselines,
	}, nil
}

func validateManagedSubscriptionBaselines(
	required map[string]string,
	declared map[string][]string,
) ([]managedSubscriptionBaseline, error) {
	for sessionID := range declared {
		if _, ok := required[sessionID]; !ok {
			return nil, fmt.Errorf(
				"managed subscription baseline references unknown or unmanaged session %q",
				sessionID,
			)
		}
	}

	sessionIDs := make([]string, 0, len(required))
	for sessionID := range required {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)

	baselines := make([]managedSubscriptionBaseline, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		filters, ok := declared[sessionID]
		if !ok {
			return nil, fmt.Errorf(
				"managed subscription baseline for Exclusive MQTT session %q is required",
				sessionID,
			)
		}
		seen := make(map[string]struct{}, len(filters))
		validatedFilters := make([]string, 0, len(filters))
		for _, filter := range filters {
			if filter == "" {
				return nil, fmt.Errorf(
					"managed subscription baseline for session %q contains an empty filter",
					sessionID,
				)
			}
			if err := paho.ValidateMQTTTopicFilter(filter); err != nil {
				return nil, fmt.Errorf(
					"managed subscription baseline for session %q contains invalid filter %q: %w",
					sessionID,
					filter,
					err,
				)
			}
			if _, duplicate := seen[filter]; duplicate {
				continue
			}
			seen[filter] = struct{}{}
			validatedFilters = append(validatedFilters, filter)
		}
		sort.Strings(validatedFilters)
		baselines = append(baselines, managedSubscriptionBaseline{
			sessionID:       sessionID,
			storageIdentity: required[sessionID],
			filters:         validatedFilters,
		})
	}
	return baselines, nil
}

func requiredDynamoDBStore(role string, store *ports.StoreConfig) (string, error) {
	if store == nil || !strings.EqualFold(store.Type, awsstore.DynamoDBKind) {
		return "", fmt.Errorf("stores.%s must use type: dynamodb", role)
	}
	cfg, ok := store.Config.(*awsstore.DynamoDBConfig)
	if !ok || cfg == nil {
		return "", fmt.Errorf("stores.%s has an incompatible DynamoDB plugin config", role)
	}
	name, err := awsstore.ResolveDynamoDBTableName(role, cfg.TableName)
	if err != nil {
		return "", err
	}
	if unresolved := awscdk.Token_IsUnresolved(name); unresolved != nil && *unresolved {
		return "", fmt.Errorf("stores.%s table_name must be a resolved physical table_name; deploy-time tokens cannot be embedded safely in the immutable config asset", role)
	}
	return name, nil
}

// validateTask9Admission reuses the runtime composition preflight rather than
// duplicating its checked budget formula. A nil SDK client keeps this synth-time
// pass source-safe: store construction and schema preflight perform no calls.
func validateTask9Admission(cfg *ports.BridgeConfig) error {
	mqttFactory := paho.NewFactory(nil, nil)
	builder := bridge.NewBuilder(cfg, bridge.WithBlueprintValidator(config.Validate)).
		RegisterTransportFactory("mqtt", mqttFactory).
		RegisterTransportFactory("mqtt.paho", mqttFactory).
		RegisterStoreFactory(awsstore.DynamoDBKind, awsstore.NewDynamoDBStoreFactory(nil))
	plan, err := builder.Plan(context.Background())
	if err != nil {
		return fmt.Errorf("task 9 failover admission rejected the profile: %w", err)
	}
	plan.Close()
	return nil
}

func profileConfigFingerprint(cfg *ports.BridgeConfig) (string, error) {
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

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

func (g *GoBridgeDynamoDBHA) ControlService() awsecs.IService { return g.control }
func (g *GoBridgeDynamoDBHA) WorkerService() awsecs.IService  { return g.worker }
func (g *GoBridgeDynamoDBHA) ControlTaskDefinition() awsecs.FargateTaskDefinition {
	return g.controlBase.TaskDefinition
}
func (g *GoBridgeDynamoDBHA) WorkerTaskDefinition() awsecs.FargateTaskDefinition {
	return g.workerBase.TaskDefinition
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
	return g.workerBase.PortMappings
}
func (g *GoBridgeDynamoDBHA) FailoverObjective() time.Duration { return g.failoverObjective }
func (g *GoBridgeDynamoDBHA) MetricsNamespace() string         { return g.metricsNamespace }
