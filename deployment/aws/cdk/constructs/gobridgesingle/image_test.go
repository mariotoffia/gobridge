package gobridgesingle_test

import (
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
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

// A nil Image builds the published command at the version of this module the
// app depends on. A test binary IS this module rather than an app depending on
// a release of it, so the default has no version to take and says so.
func TestSingle_NilImageBuildsAtTheAppsModuleVersion(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("NilSingleImage"), nil)
	require.PanicsWithValue(t, "gobridgecdk: ImageFromGoBuild Version is empty and cannot be taken from the app: "+
		"github.com/mariotoffia/gobridge/deployment/aws/cdk is not a dependency of this program; "+
		"set Version to a published release such as v0.4.0", func() {
		gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
			Vpc:       awsec2.NewVpc(stack, jsii.String("Vpc"), nil),
			Bootstrap: infra.BootstrapConfig{BridgeID: "image", AdminAPIKeyParam: "/test/admin"},
			BridgeConfig: gobridgecdk.BridgeYamlInline(&ports.BridgeConfig{
				Bridge: ports.BridgeSettings{ID: "image"},
			}),
		})
	})
}
