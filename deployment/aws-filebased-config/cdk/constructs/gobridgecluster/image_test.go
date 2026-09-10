package gobridgecluster_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
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

func TestCluster_NilImagePanicUnchanged(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("NilClusterImage"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	require.PanicsWithValue(t, "GoBridgeCluster: Image is required", func() {
		gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{Vpc: vpc})
	})
}
