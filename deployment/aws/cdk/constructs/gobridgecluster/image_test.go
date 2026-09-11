package gobridgecluster_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestCluster_GoBuildImageMatchesBothTaskPlatforms(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
	stack := awscdk.NewStack(app, jsii.String("ClusterImage"), nil)
	gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
		Vpc: awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
		Image: gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{
			Version: "v0.4.0", Platform: "linux/arm64", BuildTags: []string{},
		}),
		Bootstrap: infra.BootstrapConfig{BridgeID: "image", AdminAPIKeyParam: "/test/admin"},
		BridgeConfig: gobridgecdk.BridgeYamlInline(&ports.BridgeConfig{
			Bridge: ports.BridgeSettings{ID: "image"},
		}),
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourcePropertiesCountIs(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"RuntimePlatform": map[string]any{"CpuArchitecture": "ARM64", "OperatingSystemFamily": "LINUX"},
	}, jsii.Number(2))
	app.Synth(nil)
}

// A nil Image builds the published command at the version of this module the
// app depends on. A test binary IS this module rather than an app depending on
// a release of it, so the default has no version to take and says so.
func TestCluster_NilImageBuildsAtTheAppsModuleVersion(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("NilClusterImage"), nil)
	require.PanicsWithValue(t, "gobridgecdk: ImageFromGoBuild Version is empty and cannot be taken from the app: "+
		"github.com/mariotoffia/gobridge/deployment/aws/cdk is not a dependency of this program; "+
		"set Version to a published release such as v0.4.0", func() {
		gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
			Vpc:       awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
			Bootstrap: infra.BootstrapConfig{BridgeID: "image", AdminAPIKeyParam: "/test/admin"},
			BridgeConfig: gobridgecdk.BridgeYamlInline(&ports.BridgeConfig{
				Bridge: ports.BridgeSettings{ID: "image"},
			}),
		})
	})
}
