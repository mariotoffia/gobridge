//go:build !race

package gobridgesingle_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsservicediscovery"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs"
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

// TestSingle_AssignPublicIp_SuppliedEfsInPublicOnlyVpcPassesParity verifies the parity check uses the public subnets the task runs in.
func TestSingle_AssignPublicIp_SuppliedEfsInPublicOnlyVpcPassesParity(t *testing.T) {
	var tpl assertions.Template
	require.NotPanics(t, func() {
		_, tpl = optionsStack(t, func(stack awscdk.Stack, _ awsec2.Vpc, p *gobridgesingle.SingleProps) {
			publicVpc := awsec2.NewVpc(stack, jsii.String("PublicVpc"), &awsec2.VpcProps{
				NatGateways: jsii.Number(0),
				SubnetConfiguration: &[]*awsec2.SubnetConfiguration{
					{Name: jsii.String("Public"), SubnetType: awsec2.SubnetType_PUBLIC},
				},
			})
			p.Vpc = publicVpc
			p.AssignPublicIp = jsii.Bool(true)
			p.EfsConfig = cdkconstructs.NewGoBridgeEfsConfig(stack, jsii.String("SuppliedEfs"), &cdkconstructs.GoBridgeEfsConfigProps{
				Vpc:        publicVpc,
				VpcSubnets: &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PUBLIC},
			})
		})
	})
	assert.Equal(t, "ENABLED", awsvpcConfig(t, ecsService(t, tpl))["AssignPublicIp"])
}

// TestSingle_CapacityProviderStrategies_SpotReplacesLaunchTypeAndWaitsForProviders verifies Spot replaces launch type FARGATE and the service waits for the cluster's providers.
func TestSingle_CapacityProviderStrategies_SpotReplacesLaunchTypeAndWaitsForProviders(t *testing.T) {
	_, tpl := optionsStack(t, func(_ awscdk.Stack, _ awsec2.Vpc, p *gobridgesingle.SingleProps) {
		p.CapacityProviderStrategies = []*awsecs.CapacityProviderStrategy{
			{CapacityProvider: jsii.String("FARGATE_SPOT"), Weight: jsii.Number(1)},
		}
	})
	svc := ecsService(t, tpl)
	props := svc["Properties"].(map[string]any)
	assert.NotContains(t, props, "LaunchType")
	assert.Equal(t, []any{map[string]any{"CapacityProvider": "FARGATE_SPOT", "Weight": 1.0}}, props["CapacityProviderStrategy"])
	assocs := tpl.FindResources(jsii.String("AWS::ECS::ClusterCapacityProviderAssociations"), nil)
	require.Len(t, *assocs, 1)
	for id, raw := range *assocs {
		assert.ElementsMatch(t, []any{"FARGATE", "FARGATE_SPOT"}, (*raw)["Properties"].(map[string]any)["CapacityProviders"])
		assert.Contains(t, svc["DependsOn"], id)
	}
}

// TestSingle_CapacityProviderStrategies_NoneKeepsFargateLaunchType verifies a nil or empty strategy keeps launch type FARGATE and adds no provider association.
func TestSingle_CapacityProviderStrategies_NoneKeepsFargateLaunchType(t *testing.T) {
	for _, tc := range []struct {
		name       string
		strategies []*awsecs.CapacityProviderStrategy
	}{
		{name: "nil"},
		{name: "empty", strategies: []*awsecs.CapacityProviderStrategy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tpl := optionsStack(t, func(_ awscdk.Stack, _ awsec2.Vpc, p *gobridgesingle.SingleProps) {
				p.CapacityProviderStrategies = tc.strategies
			})
			props := ecsService(t, tpl)["Properties"].(map[string]any)
			assert.Equal(t, "FARGATE", props["LaunchType"])
			assert.NotContains(t, props, "CapacityProviderStrategy")
			tpl.ResourceCountIs(jsii.String("AWS::ECS::ClusterCapacityProviderAssociations"), jsii.Number(0))
		})
	}
}

// TestSingle_CapacityProviderStrategies_UnknownProviderOnOwnClusterPanics verifies the construct's own cluster refuses a provider it does not have.
func TestSingle_CapacityProviderStrategies_UnknownProviderOnOwnClusterPanics(t *testing.T) {
	require.PanicsWithValue(t, "GoBridgeSingle: capacity provider \"FARGATE_SPOTT\" is not on the construct's own cluster, "+
		"which has only FARGATE and FARGATE_SPOT; pass a Cluster that has it", func() {
		optionsStack(t, func(_ awscdk.Stack, _ awsec2.Vpc, p *gobridgesingle.SingleProps) {
			p.CapacityProviderStrategies = []*awsecs.CapacityProviderStrategy{
				{CapacityProvider: jsii.String("FARGATE_SPOTT"), Weight: jsii.Number(1)},
			}
		})
	})
}

// TestSingle_CloudMapOptions_RegistersService verifies the service registers in Cloud Map, from the options' namespace or the supplied cluster's default.
func TestSingle_CloudMapOptions_RegistersService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(awscdk.Stack, awsec2.Vpc, *gobridgesingle.SingleProps)
	}{
		{name: "options namespace", mutate: func(stack awscdk.Stack, vpc awsec2.Vpc, p *gobridgesingle.SingleProps) {
			ns := awsservicediscovery.NewPrivateDnsNamespace(stack, jsii.String("Ns"), &awsservicediscovery.PrivateDnsNamespaceProps{
				Name: jsii.String("bridge.local"), Vpc: vpc,
			})
			p.CloudMapOptions = &awsecs.CloudMapOptions{CloudMapNamespace: ns, Name: jsii.String("gobridge")}
		}},
		{name: "supplied cluster default namespace", mutate: func(stack awscdk.Stack, vpc awsec2.Vpc, p *gobridgesingle.SingleProps) {
			p.Cluster = awsecs.NewCluster(stack, jsii.String("Cl"), &awsecs.ClusterProps{
				Vpc:                      vpc,
				DefaultCloudMapNamespace: &awsecs.CloudMapNamespaceOptions{Name: jsii.String("bridge.local")},
			})
			p.CloudMapOptions = &awsecs.CloudMapOptions{Name: jsii.String("gobridge")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tpl := optionsStack(t, tc.mutate)
			tpl.ResourceCountIs(jsii.String("AWS::ServiceDiscovery::Service"), jsii.Number(1))
			assert.Len(t, ecsService(t, tpl)["Properties"].(map[string]any)["ServiceRegistries"], 1)
		})
	}
}

// TestSingle_CloudMapOptions_WithoutNamespaceOnOwnClusterPanics verifies options with no namespace fail with the construct's own message.
func TestSingle_CloudMapOptions_WithoutNamespaceOnOwnClusterPanics(t *testing.T) {
	require.PanicsWithValue(t, "GoBridgeSingle: CloudMapOptions.CloudMapNamespace is required when Cluster is nil; "+
		"the construct's own cluster has no default Cloud Map namespace", func() {
		optionsStack(t, func(_ awscdk.Stack, _ awsec2.Vpc, p *gobridgesingle.SingleProps) {
			p.CloudMapOptions = &awsecs.CloudMapOptions{}
		})
	})
}
