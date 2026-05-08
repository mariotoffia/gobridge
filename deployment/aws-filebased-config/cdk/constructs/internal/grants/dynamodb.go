package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
)

// GrantDynamoDBStore grants the principal full read/write data
// access on table — sufficient for the file-based-config bridge's
// outbox, lease and DLQ stores backed by DynamoDB. Stream actions
// are intentionally omitted: the bridge does not consume DynamoDB
// streams.
//
// Idempotent: repeated calls collapse into a single IAM statement
// because awsdynamodb.ITable.GrantReadWriteData is itself a CDK
// grant aggregator.
func GrantDynamoDBStore(role awsiam.IGrantable, table awsdynamodb.ITable) {
	table.GrantReadWriteData(role)
}
