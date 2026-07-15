// Package gobridgedynamodbha provides the DynamoDB-coordinated active/warm-standby ECS profile.
package gobridgedynamodbha

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

const (
	expiryIndexName   = "ExpiryIndex"
	recordIDIndexName = "RecordIDIndex"
	claimIndexName    = "ClaimIndex"
)

type dataTableNames struct {
	lease                string
	outbox               string
	managedSubscriptions string
}

// DynamoDBHAData is the sole public data output for GoBridgeDynamoDBHA.
// It exposes the three owned tables and their deploy-time names and ARNs.
type DynamoDBHAData struct {
	lease                awsdynamodb.Table
	outbox               awsdynamodb.Table
	managedSubscriptions awsdynamodb.Table
}

func newDynamoDBHAData(scope constructs.Construct, names dataTableNames) *DynamoDBHAData {
	retainedTable := func(partitionKey string) *awsdynamodb.TableProps {
		return &awsdynamodb.TableProps{
			PartitionKey:       &awsdynamodb.Attribute{Name: jsii.String(partitionKey), Type: awsdynamodb.AttributeType_STRING},
			BillingMode:        awsdynamodb.BillingMode_PAY_PER_REQUEST,
			Encryption:         awsdynamodb.TableEncryption_AWS_MANAGED,
			RemovalPolicy:      awscdk.RemovalPolicy_RETAIN,
			DeletionProtection: jsii.Bool(true),
			PointInTimeRecoverySpecification: &awsdynamodb.PointInTimeRecoverySpecification{
				PointInTimeRecoveryEnabled: jsii.Bool(true),
			},
		}
	}

	leaseProps := retainedTable("PK")
	leaseProps.TableName = jsii.String(names.lease)
	// Deliberately do not set TimeToLiveAttribute. The lease row is the
	// monotonic fencing counter and must never be reaped.
	lease := awsdynamodb.NewTable(scope, jsii.String("LeaseTable"), leaseProps)

	outboxProps := retainedTable("PK")
	outboxProps.TableName = jsii.String(names.outbox)
	outboxProps.SortKey = &awsdynamodb.Attribute{Name: jsii.String("SK"), Type: awsdynamodb.AttributeType_STRING}
	outboxProps.TimeToLiveAttribute = jsii.String("ttl")
	outbox := awsdynamodb.NewTable(scope, jsii.String("OutboxTable"), outboxProps)
	outbox.AddGlobalSecondaryIndex(&awsdynamodb.GlobalSecondaryIndexProps{
		IndexName:      jsii.String(expiryIndexName),
		PartitionKey:   &awsdynamodb.Attribute{Name: jsii.String("has_expiry"), Type: awsdynamodb.AttributeType_STRING},
		SortKey:        &awsdynamodb.Attribute{Name: jsii.String("expires_at"), Type: awsdynamodb.AttributeType_NUMBER},
		ProjectionType: awsdynamodb.ProjectionType_KEYS_ONLY,
	})
	outbox.AddGlobalSecondaryIndex(&awsdynamodb.GlobalSecondaryIndexProps{
		IndexName:      jsii.String(recordIDIndexName),
		PartitionKey:   &awsdynamodb.Attribute{Name: jsii.String("record_id"), Type: awsdynamodb.AttributeType_STRING},
		ProjectionType: awsdynamodb.ProjectionType_KEYS_ONLY,
	})
	outbox.AddGlobalSecondaryIndex(&awsdynamodb.GlobalSecondaryIndexProps{
		IndexName:      jsii.String(claimIndexName),
		PartitionKey:   &awsdynamodb.Attribute{Name: jsii.String("PK"), Type: awsdynamodb.AttributeType_STRING},
		SortKey:        &awsdynamodb.Attribute{Name: jsii.String("claim_sort"), Type: awsdynamodb.AttributeType_STRING},
		ProjectionType: awsdynamodb.ProjectionType_ALL,
	})

	historyProps := retainedTable("storage_identity")
	historyProps.TableName = jsii.String(names.managedSubscriptions)
	history := awsdynamodb.NewTable(scope, jsii.String("ManagedSubscriptionsTable"), historyProps)

	return &DynamoDBHAData{lease: lease, outbox: outbox, managedSubscriptions: history}
}

// LeaseTable returns the monotonic-fencing lease table.
func (d *DynamoDBHAData) LeaseTable() awsdynamodb.ITable { //nolint:ireturn // Public CDK data output intentionally returns the L2 table interface.
	return d.lease
}

// LeaseTableName returns the lease table physical-name token.
func (d *DynamoDBHAData) LeaseTableName() *string { return d.lease.TableName() }

// LeaseTableARN returns the lease table ARN token.
func (d *DynamoDBHAData) LeaseTableARN() *string { return d.lease.TableArn() }

// OutboxTable returns the shared outbox table.
func (d *DynamoDBHAData) OutboxTable() awsdynamodb.ITable { //nolint:ireturn // Public CDK data output intentionally returns the L2 table interface.
	return d.outbox
}

// OutboxTableName returns the shared outbox table physical-name token.
func (d *DynamoDBHAData) OutboxTableName() *string { return d.outbox.TableName() }

// OutboxTableARN returns the shared outbox table ARN token.
func (d *DynamoDBHAData) OutboxTableARN() *string { return d.outbox.TableArn() }

// ManagedSubscriptionsTable returns the exact MQTT filter-history table.
func (d *DynamoDBHAData) ManagedSubscriptionsTable() awsdynamodb.ITable { //nolint:ireturn // Public CDK data output intentionally returns the L2 table interface.
	return d.managedSubscriptions
}

// ManagedSubscriptionsTableName returns the history table physical-name token.
func (d *DynamoDBHAData) ManagedSubscriptionsTableName() *string {
	return d.managedSubscriptions.TableName()
}

// ManagedSubscriptionsTableARN returns the history table ARN token.
func (d *DynamoDBHAData) ManagedSubscriptionsTableARN() *string {
	return d.managedSubscriptions.TableArn()
}
