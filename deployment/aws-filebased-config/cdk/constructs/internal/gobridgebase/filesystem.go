package gobridgebase

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

// addFileSeeder preserves the file-source asset, startup gate and drift policy.
//
//nolint:ireturn // CDK containers, log groups and assets are native jsii interfaces.
func addFileSeeder(c constructs.Construct, props *Props, taskDef awsecs.FargateTaskDefinition,
	mat *source.Materialized, logProps *awslogs.LogGroupProps, mountPath string,
) (awsecs.ContainerDefinition, awslogs.LogGroup, awss3assets.Asset) {
	scopeID := jsiiDeref(c.Node().Id())
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

	seederLG := awslogs.NewLogGroup(c, jsii.String("SeederLogs"), logProps)
	// Seeder init container.
	seederMode := defaultSeederMode(props)
	seederImg := configuredSeederImage(props)
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

	return seeder, seederLG, asset
}

func addEfsVolume(taskDef awsecs.FargateTaskDefinition, props *Props) {
	if props.EfsConfig == nil {
		return
	}
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}
