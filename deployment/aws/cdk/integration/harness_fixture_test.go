//go:build integration_aws
// +build integration_aws

package integration

import (
	"strings"
	"testing"
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
