//go:build !race

package grants_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

const tableARN = "arn:aws:dynamodb:us-east-1:111122223333:table/bridge-store"

func TestGrantDynamoDBLeaseStore_ExactRuntimeActions(t *testing.T) {
	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("Lease"), jsii.String(tableARN))
	grants.GrantDynamoDBLeaseStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DescribeTable", "dynamodb:DescribeTimeToLive")
	requireNoDynamoMutations(t, actions)
	mustNotHave(t, actions, "dynamodb:Query", "dynamodb:Scan", "dynamodb:DeleteItem", "dynamodb:TransactWriteItems")
	requireExactTableResource(t, stack, "dynamodb:DescribeTimeToLive")
}

func TestGrantDynamoDBOutboxStore_ExactRuntimeActionsAndIndexes(t *testing.T) {
	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("Outbox"), jsii.String(tableARN))
	grants.GrantDynamoDBOutboxStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:Query", "dynamodb:TransactWriteItems", "dynamodb:DescribeTable")
	requireNoDynamoMutations(t, actions)
	mustNotHave(t, actions, "dynamodb:Scan", "dynamodb:DeleteItem", "dynamodb:DescribeTimeToLive")
	rendered := renderedTemplate(t, stack)
	for _, index := range []string{"ExpiryIndex", "RecordIDIndex", "ClaimIndex"} {
		if !strings.Contains(rendered, "/index/"+index) {
			t.Errorf("missing exact index ARN for %s", index)
		}
	}
	if strings.Contains(rendered, "/index/*") {
		t.Fatal("outbox grant must not wildcard index ARNs")
	}
}

func TestGrantDynamoDBManagedSubscriptionsStore_ExactRuntimeActions(t *testing.T) {
	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("History"), jsii.String(tableARN))
	grants.GrantDynamoDBManagedSubscriptionsStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "dynamodb:GetItem", "dynamodb:UpdateItem", "dynamodb:DescribeTable")
	requireNoDynamoMutations(t, actions)
	mustNotHave(t, actions, "dynamodb:PutItem", "dynamodb:DeleteItem", "dynamodb:Query", "dynamodb:Scan", "dynamodb:TransactWriteItems", "dynamodb:DescribeTimeToLive")
}

func TestGrantDynamoDBDLQStore_ExactRuntimeActionsAndIndexes(t *testing.T) {
	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("DLQ"), jsii.String(tableARN))
	grants.GrantDynamoDBDLQStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem", "dynamodb:Query", "dynamodb:Scan", "dynamodb:DescribeTable")
	requireNoDynamoMutations(t, actions)
	mustNotHave(t, actions, "dynamodb:UpdateItem", "dynamodb:TransactWriteItems", "dynamodb:DescribeTimeToLive")
	rendered := renderedTemplate(t, stack)
	for _, index := range []string{"RouteIndex", "CategoryIndex"} {
		if !strings.Contains(rendered, "/index/"+index) {
			t.Errorf("missing exact index ARN for %s", index)
		}
	}
}

func requireNoDynamoMutations(t *testing.T, actions map[string]bool) {
	t.Helper()
	mustNotHave(t, actions,
		"dynamodb:CreateTable", "dynamodb:UpdateTable", "dynamodb:DeleteTable",
		"dynamodb:UpdateTimeToLive", "dynamodb:CreateBackup", "dynamodb:RestoreTableFromBackup",
		"dynamodb:TagResource", "dynamodb:UntagResource", "dynamodb:*",
	)
}

func requireExactTableResource(t *testing.T, stack awscdk.Stack, action string) {
	t.Helper()
	statement := findAllowStatement(t, stack, action)
	if statement == nil || !resourceContains(statement["Resource"], tableARN) {
		t.Fatalf("%s resource = %v, want exact table ARN %q", action, statement, tableARN)
	}
}

func renderedTemplate(t *testing.T, stack awscdk.Stack) string {
	t.Helper()
	app := awscdk.App_Of(stack)
	assembly := app.Synth(nil)
	template := assembly.GetStackByName(stack.StackName()).Template()
	raw, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	return string(raw)
}

// The coordinated cluster rollout store is one row, read with GetItem and
// rewritten with a revision-gated conditional PutItem. It never updates in place,
// never queries, never scans, and — unlike every other store adapter — has no
// schema or TTL preflight, so it needs no DescribeTable either. Anything beyond
// those two actions is privilege the running barrier cannot use. CreateTable in
// particular stays out: the rollout table is deployment-owned and retained, so a
// task role that could create it could also recreate a table an operator
// deliberately deleted.
func TestGrantDynamoDBRolloutStore_ExactRuntimeActions(t *testing.T) {
	stack, role := newTestStack(t)
	table := awsdynamodb.Table_FromTableArn(stack, jsii.String("Rollout"), jsii.String(tableARN))
	grants.GrantDynamoDBRolloutStore(role, table)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "dynamodb:GetItem", "dynamodb:PutItem")
	requireNoDynamoMutations(t, actions)
	mustNotHave(t, actions,
		"dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:Query", "dynamodb:Scan",
		"dynamodb:TransactWriteItems", "dynamodb:DescribeTable", "dynamodb:DescribeTimeToLive")
	requireExactTableResource(t, stack, "dynamodb:PutItem")

	rendered := renderedTemplate(t, stack)
	if strings.Contains(rendered, "/index/") {
		t.Fatal("rollout grant must not reference any index: the store is a single-row aggregate with no GSI")
	}
}
