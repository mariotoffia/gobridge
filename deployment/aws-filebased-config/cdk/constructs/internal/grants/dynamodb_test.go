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
