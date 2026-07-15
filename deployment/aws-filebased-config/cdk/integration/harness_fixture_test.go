//go:build integration_aws
// +build integration_aws

package integration

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"
)

func completeSandboxValues() map[string]string {
	return map[string]string{
		"GOBRIDGE_INT_AWS_ACCOUNT":        "111122223333",
		"GOBRIDGE_INT_AWS_REGION":         "eu-west-1",
		"GOBRIDGE_INT_VPC_ID":             "vpc-123456",
		"GOBRIDGE_INT_AVAILABILITY_ZONES": "eu-west-1a,eu-west-1b",
		"GOBRIDGE_INT_SUBNET_IDS":         "subnet-private-a,subnet-private-b",
		"GOBRIDGE_INT_PUBLIC_SUBNET_IDS":  "subnet-public-a,subnet-public-b",
	}
}

func TestSandboxEnvFrom_RequiresConcreteVpcSubnetAndAZAttributes(t *testing.T) {
	for _, missing := range []string{"GOBRIDGE_INT_AVAILABILITY_ZONES", "GOBRIDGE_INT_PUBLIC_SUBNET_IDS"} {
		values := completeSandboxValues()
		delete(values, missing)
		_, err := sandboxEnvFrom(func(name string) string { return values[name] })
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("missing %s error = %v", missing, err)
		}
	}
}

func TestSandboxEnvFrom_RejectsSubnetAZCardinalityMismatch(t *testing.T) {
	values := completeSandboxValues()
	values["GOBRIDGE_INT_SUBNET_IDS"] = "subnet-private-a"
	_, err := sandboxEnvFrom(func(name string) string { return values[name] })
	if err == nil || !strings.Contains(err.Error(), "same number") {
		t.Fatalf("cardinality mismatch error = %v", err)
	}
}

func TestLookupVpc_ExplicitAttributesProduceCompleteAssembly(t *testing.T) {
	defer jsii.Close()
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
