//go:build !race

package gobridgesingle_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
)

// optionsStack builds a file-config Single (so EFS is auto-created) after mutate adjusts its props.
func optionsStack(t *testing.T, mutate func(awscdk.Stack, awsec2.Vpc, *gobridgesingle.SingleProps)) (awscdk.Stack, assertions.Template) {
	t.Helper()
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("OptionsStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	props := &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        imgsource.NewRegistry("gobridge@sha256:" + strings.Repeat("a", 64)),
		Bootstrap:    singleBootstrap(),
		BridgeConfig: source.NewAsset(writeSingleYAML(t, singleSampleYAML)),
	}
	if mutate != nil {
		mutate(stack, vpc, props)
	}
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), props)
	return stack, assertions.Template_FromStack(stack, nil)
}

// ecsService returns the one AWS::ECS::Service resource (Properties, DependsOn, ...).
func ecsService(t *testing.T, tpl assertions.Template) map[string]any {
	t.Helper()
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	require.Len(t, *svcs, 1)
	for _, raw := range *svcs {
		return *raw
	}
	return nil
}

func awsvpcConfig(t *testing.T, svc map[string]any) map[string]any {
	t.Helper()
	return svc["Properties"].(map[string]any)["NetworkConfiguration"].(map[string]any)["AwsvpcConfiguration"].(map[string]any)
}

func refs(t *testing.T, values []any) []string {
	t.Helper()
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, v.(map[string]any)["Ref"].(string))
	}
	return out
}

// TestSingle_AssignPublicIp verifies the prop reaches the service and the default stays private.
func TestSingle_AssignPublicIp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		assign *bool
		want   string
	}{
		{name: "default", want: "DISABLED"},
		{name: "enabled", assign: jsii.Bool(true), want: "ENABLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tpl := optionsStack(t, func(_ awscdk.Stack, _ awsec2.Vpc, p *gobridgesingle.SingleProps) {
				p.AssignPublicIp = tc.assign
			})
			assert.Equal(t, tc.want, awsvpcConfig(t, ecsService(t, tpl))["AssignPublicIp"])
		})
	}
}

// TestSingle_AssignPublicIp_TaskAndEfsShareThePublicSubnets verifies an auto-created EFS
// gets a mount target in every subnet the public task can land in.
func TestSingle_AssignPublicIp_TaskAndEfsShareThePublicSubnets(t *testing.T) {
	var vpc awsec2.Vpc
	stack, tpl := optionsStack(t, func(_ awscdk.Stack, v awsec2.Vpc, p *gobridgesingle.SingleProps) {
		vpc = v
		p.AssignPublicIp = jsii.Bool(true)
	})
	var public []string
	for _, s := range *vpc.PublicSubnets() {
		public = append(public, stack.Resolve(s.SubnetId()).(map[string]any)["Ref"].(string))
	}
	require.NotEmpty(t, public)
	assert.ElementsMatch(t, public, refs(t, awsvpcConfig(t, ecsService(t, tpl))["Subnets"].([]any)))
	var mounts []any
	for _, raw := range *tpl.FindResources(jsii.String("AWS::EFS::MountTarget"), nil) {
		mounts = append(mounts, (*raw)["Properties"].(map[string]any)["SubnetId"])
	}
	assert.ElementsMatch(t, public, refs(t, mounts))
}
