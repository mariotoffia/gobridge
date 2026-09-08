package gobridgebase

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

//nolint:ireturn // CDK containers, log groups and assets are native jsii interfaces.
func addConfigSeeder(c constructs.Construct, props *Props, taskDef awsecs.FargateTaskDefinition,
	mat *source.Materialized, logProps *awslogs.LogGroupProps, mountPath string,
) (awsecs.ContainerDefinition, awslogs.LogGroup, awss3assets.Asset) {
	if props.Bootstrap.ConfigSource == infra.ConfigSourceDynamoDB {
		return addDynamoDBSeeder(c, props, taskDef, mat, logProps)
	}
	return addFileSeeder(c, props, taskDef, mat, logProps, mountPath)
}

// addDynamoDBSeeder ships the parser's validated wire projection, not a direct
// json.Marshal of BridgeConfig (which would silently omit typed plugin options).
// The task role is shared by all containers. Never grant a worker seeder writes.
//
//nolint:ireturn // CDK containers, log groups and assets are native jsii interfaces.
func addDynamoDBSeeder(c constructs.Construct, props *Props, taskDef awsecs.FargateTaskDefinition,
	mat *source.Materialized, logProps *awslogs.LogGroupProps,
) (awsecs.ContainerDefinition, awslogs.LogGroup, awss3assets.Asset) {
	mode := defaultSeederMode(props)
	if props.Mode == ModeWorker && mode != "AdoptValid" && mode != "AbortDeploy" {
		panic("gobridgebase: DynamoDB worker seeder mode must be AdoptValid or AbortDeploy")
	}
	switch mode {
	case "SeedOnce", "Overwrite", "AbortDeploy", "AdoptValid":
	default:
		panic(fmt.Sprintf("gobridgebase: invalid DynamoDB seeder mode %q", mode))
	}
	data, err := parser.MarshalBridgeConfigJSON(mat.Config)
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: marshal DynamoDB config asset: %v", err))
	}
	// S3 assets are opaque to CloudFormation. HA data-table names already use
	// resolved literals; fail closed for other deploy-time values instead of
	// uploading ${Token...} strings. The owned config table token belongs only
	// in the task environment, where CloudFormation can resolve it normally.
	if unresolved := awscdk.Token_IsUnresolved(string(data)); unresolved != nil && *unresolved {
		panic("gobridgebase: DynamoDB config asset contains unresolved CDK tokens; use resolved physical resource names")
	}
	const maxConfigBytes = 390 * 1024 // adapters/aws/config/dynamodb's data limit.
	if len(data) > maxConfigBytes {
		panic(fmt.Sprintf("gobridgebase: DynamoDB config asset exceeds the %d-byte data limit", maxConfigBytes))
	}
	dir, err := os.MkdirTemp("", "gobridgecdk-ddb-*")
	if err != nil {
		panic(fmt.Sprintf("gobridgebase: stage DynamoDB config asset: %v", err))
	}
	defer func() { _ = os.RemoveAll(dir) }()
	assetPath := filepath.Join(dir, "bridge.json")
	if err := os.WriteFile(assetPath, data, 0o600); err != nil {
		panic(fmt.Sprintf("gobridgebase: write DynamoDB config asset: %v", err))
	}
	asset := awss3assets.NewAsset(c, jsii.String("ConfigAsset"), &awss3assets.AssetProps{Path: jsii.String(assetPath)})
	seederLG := awslogs.NewLogGroup(c, jsii.String("SeederLogs"), logProps)
	seeder := taskDef.AddContainer(jsii.String("Seeder"), &awsecs.ContainerDefinitionOptions{
		ContainerName: jsii.String(containerNameSeeder),
		Image:         awsecs.ContainerImage_FromRegistry(jsii.String(configuredSeederImage(props)), nil),
		Essential:     jsii.Bool(false),
		EntryPoint:    jsii.Strings("/bin/bash", "-c"),
		Command:       jsii.Strings(dynamoDBSeederScript()),
		Environment: &map[string]*string{
			"MODE":              jsii.String(mode),
			"TABLE":             props.ConfigTable.TableName(),
			"PK":                jsii.String("config#" + props.Bootstrap.BridgeID),
			"EXPECTED_HASH":     jsii.String(sha256Hex(data)),
			"ITEM_S3_URI":       asset.S3ObjectUrl(),
			"LOG_STREAM_PREFIX": jsii.String(jsiiDeref(c.Node().Id()) + "/" + containerNameSeeder),
		},
		Logging: awsecs.LogDriver_AwsLogs(&awsecs.AwsLogDriverProps{
			LogGroup: seederLG, StreamPrefix: jsii.String(containerNameSeeder),
		}),
	})
	return seeder, seederLG, asset
}
