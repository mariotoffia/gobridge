package gobridgebase

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// NewConfigTable provisions the config source once under the owning facade.
// All of its task definitions receive this same table through Props.ConfigTable.
// A file source has no config table. The config item is durable, so it has no TTL.
//
//nolint:ireturn // CDK exposes table resources only through jsii interfaces.
func NewConfigTable(scope constructs.Construct, bootstrap infra.BootstrapConfig) awsdynamodb.Table {
	if bootstrap.ConfigSource != infra.ConfigSourceDynamoDB {
		return nil
	}
	props := &awsdynamodb.TableProps{
		PartitionKey:  &awsdynamodb.Attribute{Name: jsii.String("PK"), Type: awsdynamodb.AttributeType_STRING},
		SortKey:       &awsdynamodb.Attribute{Name: jsii.String("SK"), Type: awsdynamodb.AttributeType_STRING},
		BillingMode:   awsdynamodb.BillingMode_PAY_PER_REQUEST,
		Encryption:    awsdynamodb.TableEncryption_AWS_MANAGED,
		RemovalPolicy: awscdk.RemovalPolicy_RETAIN,
		PointInTimeRecoverySpecification: &awsdynamodb.PointInTimeRecoverySpecification{
			PointInTimeRecoveryEnabled: jsii.Bool(true),
		},
	}
	if bootstrap.ConfigDynamoDB != nil && bootstrap.ConfigDynamoDB.WatchMode == "streams" {
		props.Stream = awsdynamodb.StreamViewType_KEYS_ONLY
	}
	return awsdynamodb.NewTable(scope, jsii.String("ConfigTable"), props)
}

// stampConfigTable makes the deployment authoritative without retaining or
// modifying caller-owned settings. Poll is the default watch mode.
func stampConfigTable(bootstrap *infra.BootstrapConfig, table awsdynamodb.ITable) {
	if bootstrap.ConfigSource != infra.ConfigSourceDynamoDB {
		return
	}
	if table == nil {
		panic("gobridgebase: ConfigTable is required for config_source dynamodb")
	}
	settings := infra.ConfigDynamoDBSettings{}
	if bootstrap.ConfigDynamoDB != nil {
		settings = *bootstrap.ConfigDynamoDB
	}
	settings.TableName = *table.TableName()
	if settings.WatchMode == "" {
		settings.WatchMode = "poll"
	}
	bootstrap.ConfigDynamoDB = &settings
}
