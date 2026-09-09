//go:build !race

package gobridgedynamodbha_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
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

func TestHA_NilImagePanicUnchanged(t *testing.T) {
	require.PanicsWithValue(t, "GoBridgeDynamoDBHA: Image is required", func() {
		newHAHarness(t, func(props *ha.DynamoDBHAProps) { props.Image = nil })
	})
}
