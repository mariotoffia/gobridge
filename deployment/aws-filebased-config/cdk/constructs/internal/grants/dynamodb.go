package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"
)

// GrantDynamoDBStore grants the principal full read/write data
// access on table — sufficient for the file-based-config bridge's
// outbox, lease, DLQ and managed-subscription stores backed by DynamoDB. Stream actions
// are intentionally omitted: the bridge does not consume DynamoDB
// streams.
//
// Idempotent: repeated calls collapse into a single IAM statement
// because awsdynamodb.ITable.GrantReadWriteData is itself a CDK
// grant aggregator.
func GrantDynamoDBStore(role awsiam.IGrantable, table awsdynamodb.ITable) {
	table.GrantReadWriteData(role)
	// Every DynamoDB store factory fails closed on schema preflight. Data-plane
	// grants omit DescribeTable, so grant that exact control-plane read on the
	// same table ARN rather than widening to dynamodb:*.
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee: role, Actions: jsii.Strings("dynamodb:DescribeTable"),
		ResourceArns: jsii.Strings(*table.TableArn()),
	})
}

// GrantDynamoDBLeasePreflight grants the additional lease-only control-plane
// read required to verify that DynamoDB TTL is disabled on the fencing table.
// TTL on that table can delete the monotonic fence row and reset its version.
func GrantDynamoDBLeasePreflight(role awsiam.IGrantable, table awsdynamodb.ITable) {
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee: role, Actions: jsii.Strings("dynamodb:DescribeTimeToLive"),
		ResourceArns: jsii.Strings(*table.TableArn()),
	})
}
