package gobridgesingle_test

import (
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestSingle_GoBuildImageMatchesTaskPlatform(t *testing.T) {
	out := t.TempDir()
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(out)})
	stack := awscdk.NewStack(app, jsii.String("SingleImage"), nil)
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc: awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
		Image: gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{
			Version: "v0.4.0", Platform: "linux/arm64",
		}),
		Bootstrap: infra.BootstrapConfig{BridgeID: "image", AdminAPIKeyParam: "/test/admin"},
		BridgeConfig: gobridgecdk.BridgeYamlInline(&ports.BridgeConfig{
			Bridge: ports.BridgeSettings{ID: "image"},
		}),
	})
	assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"RuntimePlatform": map[string]any{"CpuArchitecture": "ARM64", "OperatingSystemFamily": "LINUX"},
	})
	app.Synth(nil)
	files, err := filepath.Glob(filepath.Join(out, "asset.*", "Dockerfile"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestSingle_NilImagePanicUnchanged(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("NilSingleImage"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	require.PanicsWithValue(t, "GoBridgeSingle: Image is required", func() {
		gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{Vpc: vpc})
	})
}
