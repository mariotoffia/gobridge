//go:build !race

package gobridgedynamodbha_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
	ha "github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridgecdk"
	"github.com/stretchr/testify/require"
)

func TestHA_GoBuildImageMatchesEveryTaskPlatform(t *testing.T) {
	h := newHAHarness(t, func(props *ha.DynamoDBHAProps) {
		props.Image = gobridgecdk.ImageFromGoBuild(gobridgecdk.ImageGoBuildProps{
			Version: "v0.4.0", Platform: "linux/arm64",
		})
	})
	tpl := assertions.Template_FromStack(h.stack, nil)
	tpl.ResourcePropertiesCountIs(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"RuntimePlatform": map[string]any{"CpuArchitecture": "ARM64", "OperatingSystemFamily": "LINUX"},
	}, jsii.Number(2))
	h.app.Synth(nil)
}

// A nil Image builds the published command at the version of this module the
// app depends on. A test binary IS this module rather than an app depending on
// a release of it, so the default has no version to take and says so.
func TestHA_NilImageBuildsAtTheAppsModuleVersion(t *testing.T) {
	require.PanicsWithValue(t, "gobridgecdk: ImageFromGoBuild Version is empty and cannot be taken from the app: "+
		"github.com/mariotoffia/gobridge/deployment/aws/cdk is not a dependency of this program; "+
		"set Version to a published release such as v0.4.0", func() {
		newHAHarness(t, func(props *ha.DynamoDBHAProps) { props.Image = nil })
	})
}
