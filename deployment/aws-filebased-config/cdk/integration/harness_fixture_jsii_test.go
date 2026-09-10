//go:build integration_aws && !race
// +build integration_aws,!race

package integration

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"
)

func TestLookupVpc_ExplicitAttributesProduceCompleteAssembly(t *testing.T) {
	values := completeSandboxValues()
	env, err := sandboxEnvFrom(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("sandboxEnvFrom: %v", err)
	}
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("ExplicitVpcStack"), &awscdk.StackProps{Env: StackEnv(env)})
	vpc := lookupVpc(stack, env)
	selected := vpc.SelectSubnets(subnetSelection(env))
	if selected.IsPendingLookup != nil && *selected.IsPendingLookup {
		t.Fatal("explicit VPC attributes unexpectedly produced pending lookup subnets")
	}
	assembly := app.Synth(nil)
	manifest := assembly.Manifest()
	if manifest.Missing != nil && len(*manifest.Missing) != 0 {
		t.Fatalf("explicit VPC fixture produced missing context: %v", *manifest.Missing)
	}
}
