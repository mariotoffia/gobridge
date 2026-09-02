//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Calling AWS as the deployment's own task role, and reading back what
// CloudFormation granted it.
//
// The role is assumed from the test process rather than exercised inside a
// container, because the containers are handed static credentials so that they
// reach the emulated endpoints — the role they declare is never the one they
// present.
//
// What can and cannot be proved here, measured rather than assumed: the
// emulator does NOT evaluate IAM. A call the assumed role has no grant for
// succeeds, so a "this is denied" assertion would pass against any policy at
// all, including an empty one. The granted half is executed; the denied half
// rests on AWS's own IAM, and what stands in for it locally is reading the
// policy CloudFormation attached to the deployed role and requiring every grant
// in it to name this deployment's own resources.

// assertQueueGrantsAreScoped fails unless every SQS statement in every policy
// attached to the deployed role names a resource belonging to this deployment.
//
// It reads the policy back through the IAM API rather than out of the template,
// so what is asserted is what CloudFormation actually attached to the role the
// tasks run as. queuePrefix is the deployment's own queue-name prefix; a
// statement naming anything else — a wildcard above all — is a grant the
// workload did not need.
func assertQueueGrantsAreScoped(t *testing.T, ctx context.Context, roleARN, queuePrefix string) {
	t.Helper()
	client := iam.NewFromConfig(localAWSConfig(t))
	roleName := roleARN[strings.LastIndex(roleARN, "/")+1:]
	listed, err := client.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName)})
	if err != nil {
		t.Fatalf("list the inline policies of the deployed task role %s: %v", roleName, err)
	}
	if len(listed.PolicyNames) == 0 {
		t.Fatalf("the deployed task role %s carries no inline policy, so nothing grants it the queues "+
			"its own transports are bound to", roleName)
	}
	checked := 0
	for _, name := range listed.PolicyNames {
		policy, err := client.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
			RoleName: aws.String(roleName), PolicyName: aws.String(name),
		})
		if err != nil {
			t.Fatalf("read policy %s of role %s: %v", name, roleName, err)
		}
		document, err := url.QueryUnescape(aws.ToString(policy.PolicyDocument))
		if err != nil {
			document = aws.ToString(policy.PolicyDocument)
		}
		for _, statement := range policyStatements(t, name, document) {
			if !mentionsSQS(statement.Action) {
				continue
			}
			checked++
			for _, resource := range statement.Resource {
				if resource == "*" {
					t.Errorf("policy %s grants %v on every resource; the deployment's transports need "+
						"only its own queues", name, statement.Action)
					continue
				}
				if !strings.Contains(resource, queuePrefix) {
					t.Errorf("policy %s grants %v on %q, which is not a queue this deployment declared",
						name, statement.Action, resource)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatalf("no policy attached to the deployed task role %s grants any SQS action, so the "+
			"deployment's transports could not be doing what they are doing", roleName)
	}
}

// iamStatement is the part of an IAM policy statement this proof reads. Action
// and Resource are each either a string or a list of strings in the wire form,
// so both are decoded through a tolerant list.
type iamStatement struct {
	Effect   string
	Action   []string
	Resource []string
}

func policyStatements(t *testing.T, policyName, document string) []iamStatement {
	t.Helper()
	var parsed struct {
		Statement []struct {
			Effect   string          `json:"Effect"`
			Action   json.RawMessage `json:"Action"`
			Resource json.RawMessage `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("parse policy %s: %v", policyName, err)
	}
	out := make([]iamStatement, 0, len(parsed.Statement))
	for _, statement := range parsed.Statement {
		out = append(out, iamStatement{
			Effect:   statement.Effect,
			Action:   stringOrList(statement.Action),
			Resource: stringOrList(statement.Resource),
		})
	}
	return out
}

// stringOrList decodes an IAM field that is either one string or a list of them.
func stringOrList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

func mentionsSQS(actions []string) bool {
	for _, action := range actions {
		if strings.HasPrefix(action, "sqs:") {
			return true
		}
	}
	return false
}

// assumeTaskRole returns a config whose credentials are the named role's.
func assumeTaskRole(t *testing.T, ctx context.Context, roleARN string) aws.Config {
	t.Helper()
	base := localAWSConfig(t)
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), roleARN,
		func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = "gobridge-least-privilege-proof" })
	if _, err := provider.Retrieve(ctx); err != nil {
		t.Fatalf("assume the deployment's task role %s: %v", roleARN, err)
	}
	assumed := base.Copy()
	assumed.Credentials = aws.NewCredentialsCache(provider)
	return assumed
}
