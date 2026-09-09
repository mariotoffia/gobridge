package gobridgebase

import (
	"fmt"
	"path"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/jsii-runtime-go"
	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func stampConfigFilePath(bootstrap *infra.BootstrapConfig, mount string) {
	if bootstrap.ConfigSource != infra.ConfigSourceFile {
		return
	}
	if bootstrap.ConfigFilePath == "" {
		bootstrap.ConfigFilePath = path.Join(mount, infra.DefaultBridgeYamlName)
	}
	target := path.Clean(bootstrap.ConfigFilePath)
	prefix := strings.TrimSuffix(path.Clean(mount), "/") + "/"
	if !path.IsAbs(target) || !strings.HasPrefix(target, prefix) || target == path.Clean(mount) {
		panic(fmt.Sprintf("gobridgebase: Bootstrap.ConfigFilePath %q must be a file below MountPath %q",
			bootstrap.ConfigFilePath, mount))
	}
}

func addEfsVolume(taskDef awsecs.FargateTaskDefinition, props *Props) {
	if props.EfsConfig == nil {
		return
	}
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

func pickAccessPoint(efs *cdkconstructs.GoBridgeEfsConfig, mode Mode) awsefs.IAccessPoint {
	if mode == ModeWorker {
		return efs.WorkerAccessPoint()
	}
	return efs.ControlAccessPoint()
}
