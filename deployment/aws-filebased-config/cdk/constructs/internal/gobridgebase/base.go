package gobridgebase

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// Mode selects which kind of GoBridge task definition the base will
// emit. Each Mode produces a different EFS mount stance and a
// different seeder behaviour:
//
//   - ModeControl: EFS mounted RW, seeder runs in MODE=SeedOnce (or
//     the operator-supplied [Props.SeederMode] override).
//   - ModeWorker:  EFS mounted RO at the ECS volume layer, seeder
//     runs in MODE=AdoptValid (default): worker startup gates on the
//     current EFS config being present + parseable, but tolerates
//     hash drift from the synth-time asset so Admin-API hot
//     reconfiguration and worker self-healing coexist. Override via
//     [Props.WorkerSeederMode] (e.g. "AbortDeploy" for strict
//     lock-step).
type Mode string

const (
	ModeControl Mode = "control"
	ModeWorker  Mode = "worker"
)

// Default Fargate sizing. Picked to match the Single profile baseline
// matching the historical defaults (CPU=512, mem=1024 MiB).
// Both can be overridden via [Props.CPU] / [Props.MemoryMiB].
const (
	defaultCPU       = 512.0
	defaultMemoryMiB = 1024.0
)

// defaultMountPath is the container directory where EFS is mounted.
// Derived from the single canonical infra.DefaultMountPath so the mount, the
// fast-fail store-path validator and ServiceProps normalization never disagree
// (a split path silently loses outbox/DLQ durability on task replacement).
const defaultMountPath = infra.DefaultMountPath

// defaultBridgeYamlName is the file the seeder writes onto EFS and
// the runtime watches. Kept stable so admin tooling can reference a
// well-known path.
const defaultBridgeYamlName = infra.DefaultBridgeYamlName

// volumeName is the ECS task-def volume key. Stable so tests can
// assert mount points by name.
const volumeName = "gobridge-config"

// ContainerNameMain is the logical container name of the gobridge
// runtime container. Used in the log group prefix and exported so
// downstream constructs (e.g. the ALB attachment) can refer to it
// via [awsecs.LoadBalancerTargetOptions.ContainerName] without
// duplicating the literal.
const ContainerNameMain = "gobridge"

// containerNameSeeder is the logical container name of the seeder
// init container.
const containerNameSeeder = "seeder"

// defaultBinaryPath is where the runtime container image installs the
// production binary. The container HEALTHCHECK invokes this same static binary
// with -healthcheck (the distroless image ships no curl/wget/shell), so it MUST
// match the Dockerfile install path (root Dockerfile: /usr/local/bin/...).
const defaultBinaryPath = "/usr/local/bin/gobridge-filebased"

// defaultContainerUser is the non-root uid:gid the main container runs as.
// Defense-in-depth beyond the EFS access-point POSIX enforcement (which only
// governs EFS I/O); a compromised process should not run as root in the task.
// Matches the "nonroot" user baked into the distroless base (65532).
const defaultContainerUser = "65532:65532"

// defaultStopTimeoutSeconds is the ECS StopTimeout for the main container.
// Fargate's default is 30s which exactly equals the drain budget, so an
// in-flight drain gets SIGKILLed mid-cleanup (outbox/DLQ flush lost). 60s =
// 30s drain + 30s margin; docs instruct 45s so this comfortably covers it.
const defaultStopTimeoutSeconds = 60.0

// Container health-check timings. The probe runs the static binary against the
// local monitor /live endpoint (which 503s once the runtime is terminal), so
// ECS marks the task unhealthy and replaces it instead of leaving a "running"
// task bridging nothing forever when there is no ALB target health check.
const (
	defaultHealthCheckIntervalSeconds    = 30.0
	defaultHealthCheckTimeoutSeconds     = 5.0
	defaultHealthCheckRetries            = 3.0
	defaultHealthCheckStartPeriodSeconds = 60.0
)

// Props configures the shared base. The fields mirror what the
// public GoBridgeSingle / GoBridgeCluster constructs need to forward.
//
// Required: Vpc, EfsConfig, Image, Bootstrap, Source, Mode.
type Props struct {
	// Mode selects control vs worker task shape.
	Mode Mode

	// Vpc is the VPC the Fargate task runs in.
	Vpc awsec2.IVpc

	// EfsConfig provides the file system + access points. The base
	// uses [GoBridgeEfsConfig.ControlAccessPoint] for ModeControl
	// and [GoBridgeEfsConfig.WorkerAccessPoint] for ModeWorker.
	EfsConfig *cdkconstructs.GoBridgeEfsConfig

	// EfsKmsKey, when non-nil, triggers a KMS grant on the task role
	// (kms:Decrypt + GenerateDataKey + DescribeKey) scoped to the
	// CMK encrypting the file system.
	EfsKmsKey awskms.IKey

	// Image is the gobridge runtime container image.
	Image awsecs.ContainerImage

	// Bootstrap is the deployment-owned runtime configuration. The
	// base serializes it as the GOBRIDGE_FILEBASED_BOOTSTRAP_JSON
	// env var on the main container.
	Bootstrap infra.BootstrapConfig

	// Source is the sealed BridgeConfigSource (see top-level
	// gobridgecdk facade). The base materializes it once at synth
	// time, parses the resulting bridge.yaml, derives port mappings
	// from it, and uploads its bytes as an S3 asset for the seeder
	// to download.
	Source source.Source

	// QueueRegistry resolves SQS queue names referenced by the
	// parsed BridgeConfig. Kinds present in the config but missing
	// from the registry are surfaced via CDK Annotations elsewhere
	// (Phase 2 validator) — the base only consumes resolved refs to
	// emit per-adapter grants.
	QueueRegistry *registry.QueueRegistry

	// SsmRegistry resolves SSM parameter URIs referenced by the
	// parsed BridgeConfig (admin/monitor/HTTP receiver/sender API
	// keys, plugin credentials).
	SsmRegistry *registry.SsmParamRegistry

	// CPU overrides the default Fargate CPU units (defaultCPU).
	CPU *float64

	// MemoryMiB overrides the default Fargate memory (defaultMemoryMiB).
	MemoryMiB *float64

	// TaskRole, when non-nil, replaces the auto-generated task role.
	TaskRole awsiam.IRole

	// ExecutionRole, when non-nil, replaces the auto-generated
	// execution role.
	ExecutionRole awsiam.IRole

	// MountPath overrides defaultMountPath.
	MountPath *string

	// LogRetention overrides the default CloudWatch log retention
	// (awslogs.RetentionDays_ONE_MONTH). Applies to both the main
	// and seeder log groups.
	LogRetention awslogs.RetentionDays

	// LogRemovalPolicy overrides the default RemovalPolicy.RETAIN
	// applied to log groups.
	LogRemovalPolicy awscdk.RemovalPolicy

	// SeederImage overrides the pinned aws-cli image returned by
	// [DefaultSeederImage].
	SeederImage *string

	// SeederMode overrides the default seeder MODE for ModeControl
	// (default "SeedOnce"). Ignored for ModeWorker — use WorkerSeederMode.
	SeederMode *string

	// WorkerSeederMode overrides the ModeWorker seeder MODE. Default
	// "AdoptValid": a worker adopts whatever valid bridge.yaml the control
	// node last wrote (CDK seed OR Admin-API config-txn commit) instead of
	// aborting on hash drift, so hot reconfiguration and worker self-healing
	// coexist. Set "AbortDeploy" for strict lock-step (workers refuse to
	// start on any drift from the synth-time asset). Ignored for ModeControl.
	WorkerSeederMode *string

	// StopTimeout overrides the main container StopTimeout
	// (defaultStopTimeoutSeconds). Must exceed the runtime drain budget with
	// margin or in-flight drains are SIGKILLed.
	StopTimeout awscdk.Duration

	// HealthCheckCommand overrides the main container health-check command.
	// Default: the static binary probing its own monitor /live endpoint
	// ([]string{"CMD", defaultBinaryPath, "-healthcheck"}). Supply an explicit
	// command only when a mirror image installs the binary elsewhere.
	HealthCheckCommand *[]*string

	// DisableHealthCheck removes the container health check. Off by default;
	// set true only when an ALB target-group health check fully replaces it.
	DisableHealthCheck *bool

	// ContainerUser overrides the non-root uid:gid the main container runs as
	// (defaultContainerUser). Set to "" to run as the image default.
	ContainerUser *string
}

// Built is the result of constructing the base. The public facades
// embed it (or hold it as a field) and re-expose selected accessors.
type Built struct {
	constructs.Construct

	TaskDefinition  awsecs.FargateTaskDefinition
	MainContainer   awsecs.ContainerDefinition
	SeederContainer awsecs.ContainerDefinition
	MainLogGroup    awslogs.LogGroup
	SeederLogGroup  awslogs.LogGroup
	ConfigAsset     awss3assets.Asset
	TaskRole        awsiam.IRole
	ExecutionRole   awsiam.IRole
	PortMappings    []PortMapping
	Mode            Mode
}

// New constructs a shared GoBridge task definition + log groups +
// asset + IAM scaffolding under scope/id. Panics on invalid props
// (missing required fields, unknown Mode, materialization errors).
//
// New is jsii-bound: callers must run inside a CDK App scope with
// jsii initialised. Tests that need to assert template output should
// use awscdk/assertions like the sibling constructs/* tests.
func New(scope constructs.Construct, id *string, props *Props) *Built {
	if props == nil {
		panic("gobridgebase: props must not be nil")
	}
	validateProps(props)

	c := constructs.NewConstruct(scope, id)
	scopeID := jsiiDeref(id)
	// Log-group names are account-and-region wide, so they are scoped to the
	// stack that owns them. A facade always builds its base under a fixed
	// construct id, so without the stack a staging bridge deployed beside a
	// production one in the same account wants the same log group and the
	// second stack fails at CREATE.
	stackName := jsiiDeref(awscdk.Stack_Of(c).StackName())

	mat, err := props.Source.Materialize()
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: materialize bridge config source: %v", err))
	}
	defer func() { _ = mat.Close() }()
	// Asset upload + EXPECTED_HASH must come from the same bytes
	// that the parser saw — read the file once.
	yamlBytes, err := os.ReadFile(mat.AssetPath)
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: read materialized yaml: %v", err))
	}
	expectedHash := sha256Hex(yamlBytes)

	asset := awss3assets.NewAsset(c, jsii.String("ConfigAsset"), &awss3assets.AssetProps{
		Path: jsii.String(mat.AssetPath),
	})

	cpu := jsii.Number(defaultCPU)
	if props.CPU != nil {
		cpu = props.CPU
	}
	mem := jsii.Number(defaultMemoryMiB)
	if props.MemoryMiB != nil {
		mem = props.MemoryMiB
	}
	containerMemoryBytes, err := memoryBytesFromMiB(*mem)
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: invalid MemoryMiB: %v", err))
	}
	bootstrap := props.Bootstrap.Normalized()
	// The task definition is authoritative. Never trust a separately supplied
	// bootstrap byte limit that could drift from the actual Fargate hard limit.
	bootstrap.ContainerMemoryBytes = containerMemoryBytes

	mountPath := defaultMountPath
	if props.MountPath != nil && *props.MountPath != "" {
		mountPath = *props.MountPath
	}

	// seederTarget is the EFS path the seeder writes and the runtime watches.
	// Cross-check it against the runtime's ConfigFilePath: on mismatch the
	// container still starts and passes /health, but the runtime's
	// optionalFileSource never finds the seeded file and silently falls back
	// to an empty default config — a live-but-bridging-nothing task. Fail
	// fast at synth instead. Empty ConfigFilePath is left to Bootstrap
	// validation at container start.
	seederTarget := joinPath(mountPath, defaultBridgeYamlName)
	if cfp := props.Bootstrap.ConfigFilePath; cfp != "" && path.Clean(cfp) != path.Clean(seederTarget) {
		panic(fmt.Sprintf(
			"gobridgebase: Bootstrap.ConfigFilePath %q does not match the seeder EFS target %q "+
				"(mount %q + %q); the runtime would watch a path the seeder never writes and bridge "+
				"nothing while still reporting healthy. Set ConfigFilePath to the seeder target (or "+
				"override MountPath consistently on both).",
			cfp, seederTarget, mountPath, defaultBridgeYamlName))
	}

	taskDef := awsecs.NewFargateTaskDefinition(c, jsii.String("TaskDef"), &awsecs.FargateTaskDefinitionProps{
		Cpu:            cpu,
		MemoryLimitMiB: mem,
		TaskRole:       props.TaskRole,
		ExecutionRole:  props.ExecutionRole,
	})

	// EFS volume — single mount, access point chosen by Mode.
	ap := pickAccessPoint(props.EfsConfig, props.Mode)
	taskDef.AddVolume(&awsecs.Volume{
		Name: jsii.String(volumeName),
		EfsVolumeConfiguration: &awsecs.EfsVolumeConfiguration{
			FileSystemId:      props.EfsConfig.FileSystem().FileSystemId(),
			TransitEncryption: jsii.String("ENABLED"),
			AuthorizationConfig: &awsecs.AuthorizationConfig{
				AccessPointId: ap.AccessPointId(),
				Iam:           jsii.String("ENABLED"),
			},
		},
	})

	// Log groups.
	logRetention := awslogs.RetentionDays_ONE_MONTH
	if props.LogRetention != "" {
		logRetention = props.LogRetention
	}
	logRemoval := awscdk.RemovalPolicy_RETAIN
	if props.LogRemovalPolicy != "" {
		logRemoval = props.LogRemovalPolicy
	}
	mainLG := awslogs.NewLogGroup(c, jsii.String("MainLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String(logGroupPrefix(stackName, scopeID, ContainerNameMain)),
		Retention:     logRetention,
		RemovalPolicy: logRemoval,
	})
	seederLG := awslogs.NewLogGroup(c, jsii.String("SeederLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String(logGroupPrefix(stackName, scopeID, containerNameSeeder)),
		Retention:     logRetention,
		RemovalPolicy: logRemoval,
	})

	// Seeder init container.
	seederMode := defaultSeederMode(props)
	seederImg := DefaultSeederImage()
	if props.SeederImage != nil && *props.SeederImage != "" {
		seederImg = *props.SeederImage
		// An operator-supplied mirror must still be fully pinned — an
		// unpinned or placeholder override is the same dead-on-arrival
		// failure as the default (main container gates on seeder SUCCESS).
		if err := validateSeederImageRef(seederImg); err != nil {
			panic(err.Error())
		}
	}
	seeder := taskDef.AddContainer(jsii.String("Seeder"), &awsecs.ContainerDefinitionOptions{
		ContainerName: jsii.String(containerNameSeeder),
		Image:         awsecs.ContainerImage_FromRegistry(jsii.String(seederImg), nil),
		Essential:     jsii.Bool(false),
		EntryPoint:    jsii.Strings("/bin/bash", "-c"),
		Command:       jsii.Strings(SeederScript()),
		Environment: &map[string]*string{
			"MODE":              jsii.String(seederMode),
			"EXPECTED_HASH":     jsii.String(expectedHash),
			"ASSET_S3_URI":      asset.S3ObjectUrl(),
			"EFS_TARGET_PATH":   jsii.String(seederTarget),
			"LOG_STREAM_PREFIX": jsii.String(scopeID + "/" + containerNameSeeder),
		},
		Logging: awsecs.LogDriver_AwsLogs(&awsecs.AwsLogDriverProps{
			LogGroup:     seederLG,
			StreamPrefix: jsii.String(containerNameSeeder),
		}),
	})
	seeder.AddMountPoints(&awsecs.MountPoint{
		SourceVolume:  jsii.String(volumeName),
		ContainerPath: jsii.String(mountPath),
		// Seeder always mounts RW. Read-only worker modes (AdoptValid /
		// AbortDeploy) never write EFS — the script stages under /tmp and
		// only reads dirname(EFS_TARGET_PATH). RO enforcement for workers
		// happens on the *main* container mount below (and is doubly
		// enforced by the GrantEFSWorker IAM scope — no ClientWrite action
		// is granted on the worker task role).
		ReadOnly: jsii.Bool(false),
	})

	// Main container.
	bootstrapJSON, err := json.Marshal(bootstrap)
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: marshal bootstrap: %v", err))
	}

	// StopTimeout: default 60s so the runtime drain (30s budget) plus cleanup
	// completes before SIGKILL. Fargate's 30s default would kill mid-drain.
	stopTimeout := props.StopTimeout
	if stopTimeout == nil {
		stopTimeout = awscdk.Duration_Seconds(jsii.Number(defaultStopTimeoutSeconds))
	}

	// User: non-root uid:gid by default (defense in depth beyond the EFS AP).
	containerUser := jsii.String(defaultContainerUser)
	if props.ContainerUser != nil {
		if *props.ContainerUser == "" {
			containerUser = nil
		} else {
			containerUser = props.ContainerUser
		}
	}

	// HealthCheck: the static binary probes its own monitor /live endpoint,
	// which returns 503 once the runtime is terminal — ECS then replaces the
	// task instead of leaving it "running" but bridging nothing. The image is
	// distroless (no curl/wget/shell) so the probe reuses the binary itself.
	mainOpts := &awsecs.ContainerDefinitionOptions{
		ContainerName: jsii.String(ContainerNameMain),
		Image:         props.Image,
		Essential:     jsii.Bool(true),
		User:          containerUser,
		StopTimeout:   stopTimeout,
		Environment: &map[string]*string{
			"GOBRIDGE_FILEBASED_BOOTSTRAP_JSON": jsii.String(string(bootstrapJSON)),
			"GOBRIDGE_NODE_ROLE":                jsii.String(string(modeToNodeRole(props.Mode))),
		},
		Logging: awsecs.LogDriver_AwsLogs(&awsecs.AwsLogDriverProps{
			LogGroup:     mainLG,
			StreamPrefix: jsii.String(ContainerNameMain),
		}),
	}
	if props.DisableHealthCheck == nil || !*props.DisableHealthCheck {
		hcCommand := props.HealthCheckCommand
		if hcCommand == nil {
			hcCommand = jsii.Strings("CMD", defaultBinaryPath, "-healthcheck")
		}
		mainOpts.HealthCheck = &awsecs.HealthCheck{
			Command:     hcCommand,
			Interval:    awscdk.Duration_Seconds(jsii.Number(defaultHealthCheckIntervalSeconds)),
			Timeout:     awscdk.Duration_Seconds(jsii.Number(defaultHealthCheckTimeoutSeconds)),
			Retries:     jsii.Number(defaultHealthCheckRetries),
			StartPeriod: awscdk.Duration_Seconds(jsii.Number(defaultHealthCheckStartPeriodSeconds)),
		}
	}
	main := taskDef.AddContainer(jsii.String("Main"), mainOpts)
	main.AddMountPoints(&awsecs.MountPoint{
		SourceVolume:  jsii.String(volumeName),
		ContainerPath: jsii.String(mountPath),
		ReadOnly:      jsii.Bool(props.Mode == ModeWorker),
	})
	main.AddContainerDependencies(&awsecs.ContainerDependency{
		Container: seeder,
		Condition: awsecs.ContainerDependencyCondition_SUCCESS,
	})

	// Port mappings derived from yaml + bootstrap.
	portMappings := DerivePortMappings(mat.Config, bootstrap)
	for _, pm := range portMappings {
		port := pm.Port
		main.AddPortMappings(&awsecs.PortMapping{
			ContainerPort: jsii.Number(port),
			Protocol:      awsecs.Protocol_TCP,
		})
	}

	// IAM grants.
	taskRole := taskDef.TaskRole()
	asset.GrantRead(taskRole)
	applyEfsGrants(props, taskRole)
	applyKmsGrant(props, taskRole)
	applyMetricsGrant(props, taskRole)
	grants.GrantLogsWrite(taskRole, mainLG)
	grants.GrantLogsWrite(taskRole, seederLG)
	applyAdapterGrants(c, props, taskRole, mat)

	return &Built{
		Construct:       c,
		TaskDefinition:  taskDef,
		MainContainer:   main,
		SeederContainer: seeder,
		MainLogGroup:    mainLG,
		SeederLogGroup:  seederLG,
		ConfigAsset:     asset,
		TaskRole:        taskRole,
		ExecutionRole:   taskDef.ExecutionRole(),
		PortMappings:    portMappings,
		Mode:            props.Mode,
	}
}

func validateProps(p *Props) {
	switch p.Mode {
	case ModeControl, ModeWorker:
	default:
		panic(fmt.Sprintf("gobridgebase: unsupported Mode %q (want %q or %q)", p.Mode, ModeControl, ModeWorker))
	}
	if p.Vpc == nil {
		panic("gobridgebase: Vpc is required")
	}
	if p.EfsConfig == nil {
		panic("gobridgebase: EfsConfig is required")
	}
	if p.Image == nil {
		panic("gobridgebase: Image is required")
	}
	if p.Source == nil {
		panic("gobridgebase: Source is required")
	}
}

func pickAccessPoint(efs *cdkconstructs.GoBridgeEfsConfig, m Mode) awsefs.IAccessPoint {
	if m == ModeWorker {
		return efs.WorkerAccessPoint()
	}
	return efs.ControlAccessPoint()
}

func defaultSeederMode(p *Props) string {
	if p.Mode == ModeWorker {
		if p.WorkerSeederMode != nil && *p.WorkerSeederMode != "" {
			return *p.WorkerSeederMode
		}
		return "AdoptValid"
	}
	if p.SeederMode != nil && *p.SeederMode != "" {
		return *p.SeederMode
	}
	return "SeedOnce"
}

func modeToNodeRole(m Mode) infra.NodeRole {
	if m == ModeWorker {
		return infra.NodeRoleWorker
	}
	return infra.NodeRoleControl
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func logGroupPrefix(stackName, scopeID, container string) string {
	if scopeID == "" {
		scopeID = "gobridge"
	}
	if stackName == "" {
		return "/gobridge/" + scopeID + "/" + container
	}
	return "/gobridge/" + stackName + "/" + scopeID + "/" + container
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

func memoryBytesFromMiB(memoryMiB float64) (uint64, error) {
	if math.IsNaN(memoryMiB) || math.IsInf(memoryMiB, 0) || memoryMiB <= 0 {
		return 0, fmt.Errorf("must be a finite positive value, got %v", memoryMiB)
	}
	if math.Trunc(memoryMiB) != memoryMiB {
		return 0, fmt.Errorf("must be a whole MiB value, got %v", memoryMiB)
	}
	const bytesPerMiB = uint64(1 << 20)
	if memoryMiB > float64(math.MaxUint64/bytesPerMiB) {
		return 0, fmt.Errorf("%v MiB overflows bytes", memoryMiB)
	}
	return uint64(memoryMiB) * bytesPerMiB, nil
}

func jsiiDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
