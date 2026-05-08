package gobridgebase

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
//     runs in MODE=AbortDeploy so worker startup gates on config
//     drift without requiring write access.
type Mode string

const (
	ModeControl Mode = "control"
	ModeWorker  Mode = "worker"
)

// Default Fargate sizing. Picked to match the Single profile baseline
// in the legacy GoBridgeService construct (CPU=512, mem=1024 MiB).
// Both can be overridden via [Props.CPU] / [Props.MemoryMiB].
const (
	defaultCPU       = 512.0
	defaultMemoryMiB = 1024.0
)

// defaultMountPath is the container directory where EFS is mounted.
// "/var/lib/gobridge" is FHS-conformant for runtime state and matches
// the expectation in BridgeConfig.ConfigFilePath defaults.
const defaultMountPath = "/var/lib/gobridge"

// defaultBridgeYamlName is the file the seeder writes onto EFS and
// the runtime watches. Kept stable so admin tooling can reference a
// well-known path.
const defaultBridgeYamlName = "bridge.yaml"

// volumeName is the ECS task-def volume key. Stable so tests can
// assert mount points by name.
const volumeName = "gobridge-config"

// containerNameMain is the logical container name of the gobridge
// runtime container. Used in the log group prefix.
const containerNameMain = "gobridge"

// containerNameSeeder is the logical container name of the seeder
// init container.
const containerNameSeeder = "seeder"

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
	// (default "SeedOnce"). Ignored for ModeWorker, which always
	// uses "AbortDeploy".
	SeederMode *string
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

	mountPath := defaultMountPath
	if props.MountPath != nil && *props.MountPath != "" {
		mountPath = *props.MountPath
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
		LogGroupName:  jsii.String(logGroupPrefix(scopeID, containerNameMain)),
		Retention:     logRetention,
		RemovalPolicy: logRemoval,
	})
	seederLG := awslogs.NewLogGroup(c, jsii.String("SeederLogs"), &awslogs.LogGroupProps{
		LogGroupName:  jsii.String(logGroupPrefix(scopeID, containerNameSeeder)),
		Retention:     logRetention,
		RemovalPolicy: logRemoval,
	})

	// Seeder init container.
	seederMode := defaultSeederMode(props)
	seederImg := DefaultSeederImage()
	if props.SeederImage != nil && *props.SeederImage != "" {
		seederImg = *props.SeederImage
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
			"EFS_TARGET_PATH":   jsii.String(joinPath(mountPath, defaultBridgeYamlName)),
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
		// Seeder always mounts RW. AbortDeploy mode (worker) only
		// reads, but the script's mkdir/mktemp probe in
		// dirname(EFS_TARGET_PATH) needs write access. RO enforcement
		// for workers happens on the *main* container mount below
		// (and is doubly enforced by the GrantEFSWorker IAM scope —
		// no ClientWrite action is granted on the worker task role).
		ReadOnly: jsii.Bool(false),
	})

	// Main container.
	bootstrapJSON, err := json.Marshal(props.Bootstrap)
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: marshal bootstrap: %v", err))
	}
	main := taskDef.AddContainer(jsii.String("Main"), &awsecs.ContainerDefinitionOptions{
		ContainerName: jsii.String(containerNameMain),
		Image:         props.Image,
		Essential:     jsii.Bool(true),
		Environment: &map[string]*string{
			"GOBRIDGE_FILEBASED_BOOTSTRAP_JSON": jsii.String(string(bootstrapJSON)),
			"GOBRIDGE_NODE_ROLE":                jsii.String(string(modeToNodeRole(props.Mode))),
		},
		Logging: awsecs.LogDriver_AwsLogs(&awsecs.AwsLogDriverProps{
			LogGroup:     mainLG,
			StreamPrefix: jsii.String(containerNameMain),
		}),
	})
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
	portMappings := DerivePortMappings(mat.Config, props.Bootstrap)
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
		return "AbortDeploy"
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

func logGroupPrefix(scopeID, container string) string {
	if scopeID == "" {
		scopeID = "gobridge"
	}
	return "/gobridge/" + scopeID + "/" + container
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

func jsiiDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
