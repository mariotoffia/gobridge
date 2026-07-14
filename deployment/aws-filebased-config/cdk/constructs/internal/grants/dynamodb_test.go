//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

func TestGrantDynamoDBStore(t *testing.T) {
	defer jsii.Close()

	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("T"),
		jsii.String("arn:aws:dynamodb:us-east-1:111122223333:table/bridge-outbox"))
	grants.GrantDynamoDBStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions,
		"dynamodb:GetItem",
		"dynamodb:PutItem",
		"dynamodb:UpdateItem",
		"dynamodb:DeleteItem",
		"dynamodb:Query",
		"dynamodb:Scan",
		"dynamodb:DescribeTable",
	)
}

func TestGrantDynamoDBStore_Idempotent(t *testing.T) {
	defer jsii.Close()

	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("T"),
		jsii.String("arn:aws:dynamodb:us-east-1:111122223333:table/bridge-outbox"))
	grants.GrantDynamoDBStore(role, table)
	grants.GrantDynamoDBStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "dynamodb:PutItem")
}

func TestGrantDynamoDBLeasePreflight_ExactActionAndResource(t *testing.T) {
	defer jsii.Close()

	stack, role := newTestStack(t)
	const tableARN = "arn:aws:dynamodb:us-east-1:111122223333:table/bridge-leases"
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("LeaseTable"), jsii.String(tableARN))
	grants.GrantDynamoDBStore(role, table)
	grants.GrantDynamoDBLeasePreflight(role, table)

	statement := findAllowStatement(t, stack, "dynamodb:DescribeTimeToLive")
	if statement == nil {
		t.Fatal("missing lease-table DescribeTimeToLive grant")
	}
	actions := normalizeTestActions(statement["Action"])
	if len(actions) != 1 || actions[0] != "dynamodb:DescribeTimeToLive" {
		t.Fatalf("lease TTL preflight actions = %v, want exact [dynamodb:DescribeTimeToLive]", actions)
	}
	if !resourceContains(statement["Resource"], tableARN) {
		t.Fatalf("lease TTL preflight resource = %v, want exact table ARN %q", statement["Resource"], tableARN)
	}
}

func TestGrantDynamoDBStore_DoesNotGrantLeaseTTLPreflight(t *testing.T) {
	defer jsii.Close()

	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("OutboxTable"),
		jsii.String("arn:aws:dynamodb:us-east-1:111122223333:table/bridge-outbox"))
	grants.GrantDynamoDBStore(role, table)

	mustNotHave(t, collectAllowActions(t, stack), "dynamodb:DescribeTimeToLive")
}

func normalizeTestActions(value any) []string {
	switch actions := value.(type) {
	case string:
		return []string{actions}
	case []any:
		out := make([]string, 0, len(actions))
		for _, action := range actions {
			if value, ok := action.(string); ok {
				out = append(out, value)
			}
		}
		return out
	default:
		return nil
	}
}
