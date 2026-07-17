package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"
)

const (
	outboxExpiryIndex   = "ExpiryIndex"
	outboxRecordIDIndex = "RecordIDIndex"
	outboxClaimIndex    = "ClaimIndex"
	dlqRouteIndex       = "RouteIndex"
	dlqCategoryIndex    = "CategoryIndex"
)

// GrantDynamoDBLeaseStore grants only the calls made by the running lease
// adapter plus its fail-closed schema and TTL preflights.
func GrantDynamoDBLeaseStore(role awsiam.IGrantable, table awsdynamodb.ITable) {
	grant(role, []string{"dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem"}, table.TableArn())
	grant(role, []string{"dynamodb:DescribeTable", "dynamodb:DescribeTimeToLive"}, table.TableArn())
}

// GrantDynamoDBOutboxStore grants the base-table calls and Query on the three
// exact adapter indexes. Provisioning and TTL mutation remain deployment-only.
func GrantDynamoDBOutboxStore(role awsiam.IGrantable, table awsdynamodb.ITable) {
	grant(role, []string{
		"dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem",
		"dynamodb:Query", "dynamodb:TransactWriteItems",
	}, table.TableArn())
	grant(role, []string{"dynamodb:Query"}, indexARNs(table,
		outboxExpiryIndex, outboxRecordIDIndex, outboxClaimIndex)...)
	grant(role, []string{"dynamodb:DescribeTable"}, table.TableArn())
}

// GrantDynamoDBManagedSubscriptionsStore grants exact-filter history reads,
// atomic updates, and schema preflight.
func GrantDynamoDBManagedSubscriptionsStore(role awsiam.IGrantable, table awsdynamodb.ITable) {
	grant(role, []string{"dynamodb:GetItem", "dynamodb:UpdateItem"}, table.TableArn())
	grant(role, []string{"dynamodb:DescribeTable"}, table.TableArn())
}

// GrantDynamoDBDLQStore grants only the running DLQ adapter calls and Query on
// its two exact indexes. Scan is required by the adapter fallback path.
func GrantDynamoDBDLQStore(role awsiam.IGrantable, table awsdynamodb.ITable) {
	grant(role, []string{
		"dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem",
		"dynamodb:Query", "dynamodb:Scan",
	}, table.TableArn())
	grant(role, []string{"dynamodb:Query"}, indexARNs(table, dlqRouteIndex, dlqCategoryIndex)...)
	grant(role, []string{"dynamodb:DescribeTable"}, table.TableArn())
}

func grant(role awsiam.IGrantable, actions []string, resources ...*string) {
	if role == nil || len(actions) == 0 || len(resources) == 0 {
		return
	}
	actionPtrs := make([]*string, 0, len(actions))
	for _, action := range actions {
		actionPtrs = append(actionPtrs, jsii.String(action))
	}
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee:      role,
		Actions:      &actionPtrs,
		ResourceArns: &resources,
	})
}

func indexARNs(table awsdynamodb.ITable, names ...string) []*string {
	arns := make([]*string, 0, len(names))
	for _, name := range names {
		arns = append(arns, awscdk.Fn_Join(jsii.String(""), &[]*string{
			table.TableArn(), jsii.String("/index/" + name),
		}))
	}
	return arns
}
