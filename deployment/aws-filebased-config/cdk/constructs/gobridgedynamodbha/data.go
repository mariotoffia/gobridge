// Package gobridgedynamodbha provides the DynamoDB-coordinated active/warm-standby ECS profile.
package gobridgedynamodbha

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/customresources"
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
	lease                           awsdynamodb.Table
	outbox                          awsdynamodb.Table
	managedSubscriptions            awsdynamodb.Table
	managedSubscriptionInitializers []constructs.Construct
}

type managedSubscriptionBaseline struct {
	sessionID       string
	storageIdentity string
	filters         []string
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

func newManagedSubscriptionInitializers(
	scope constructs.Construct,
	table awsdynamodb.Table,
	baselines []managedSubscriptionBaseline,
) []constructs.Construct {
	initializers := make([]constructs.Construct, 0, len(baselines))
	for i := range baselines {
		baseline := baselines[i]
		expressionAttributeNames := map[string]interface{}{
			"#baseline": jsii.String("baseline"),
		}
		expressionAttributeValues := map[string]interface{}{
			":true": map[string]interface{}{"BOOL": jsii.Bool(true)},
		}
		updateExpression := "SET #baseline = :true"
		if len(baseline.filters) > 0 {
			expressionAttributeNames["#filters"] = jsii.String("filters")
			expressionAttributeValues[":filters"] = map[string]interface{}{
				"SS": jsii.Strings(baseline.filters...),
			}
			updateExpression += " ADD #filters :filters"
		}

		initializer := customresources.NewAwsCustomResource(
			scope,
			jsii.String(
				"ManagedSubscriptionBaseline-"+
					baseline.sessionID+
					"-"+
					baseline.storageIdentity,
			),
			&customresources.AwsCustomResourceProps{
				OnCreate: &customresources.AwsSdkCall{
					Service: jsii.String("DynamoDB"),
					Action:  jsii.String("updateItem"),
					Parameters: map[string]interface{}{
						"TableName": table.TableName(),
						"Key": map[string]interface{}{
							"storage_identity": map[string]interface{}{
								"S": jsii.String(baseline.storageIdentity),
							},
						},
						"UpdateExpression":          jsii.String(updateExpression),
						"ExpressionAttributeNames":  expressionAttributeNames,
						"ExpressionAttributeValues": expressionAttributeValues,
					},
					PhysicalResourceId: customresources.PhysicalResourceId_Of(
						jsii.String("managed-subscription-baseline-" + baseline.storageIdentity),
					),
					Logging: customresources.Logging_WithDataHidden(),
				},
				Policy: customresources.AwsCustomResourcePolicy_FromSdkCalls(
					&customresources.SdkCallsPolicyOptions{
						Resources: &[]*string{table.TableArn()},
					},
				),
				InstallLatestAwsSdk: jsii.Bool(false),
			},
		)
		initializer.Node().AddDependency(table)
		initializers = append(initializers, initializer)
	}
	return initializers
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
