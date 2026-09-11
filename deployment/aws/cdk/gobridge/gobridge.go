// Package gobridge is the one package a CDK app imports to run GoBridge on AWS.
//
// Pick a shape — NewSingle, NewCluster or NewHA — give it a VPC and a bridge
// config, and deploy:
//
//	gobridge.NewSingle(stack, "Bridge", &gobridge.SingleProps{
//		Vpc:          vpc,
//		Bootstrap:    gobridge.Bootstrap{BridgeID: "orders", AdminAPIKeyParam: "/orders/admin-key"},
//		BridgeConfig: gobridge.ConfigFile("bridge.yaml"),
//		Queues:       map[string]awssqs.IQueue{"orders-in": ordersIn},
//	})
//
// With Image left nil the construct builds GoBridge at the same version as this
// package, linking only the transports the config names. The tasks are granted
// exactly the queues and secrets listed in the props, and a config that names
// one that is not listed fails at cdk synth rather than in production.
package gobridge

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecr"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/ssmexports"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// Settings every shape takes.
type (
	// Bootstrap is the per-task runtime settings: bridge id, admin key
	// parameter, config source and the like.
	Bootstrap = infra.BootstrapConfig
	// ConfigDynamoDB configures the DynamoDB config source.
	ConfigDynamoDB = infra.ConfigDynamoDBSettings
	// Config is the bridge configuration a construct deploys. Make one with
	// ConfigFile or ConfigInline.
	Config = gobridgecdk.BridgeConfigSource
	// Image is the container image the tasks run. Make one with
	// ImageFromGoBuild, ImageFromRegistry or ImageFromEcr, or leave it nil.
	Image = gobridgecdk.BridgeImageSource
	// GoBuild configures ImageFromGoBuild. Every field is optional.
	GoBuild = gobridgecdk.ImageGoBuildProps
	// QueueTags selects a listed queue by its tags instead of its name.
	QueueTags = registry.QueueTags
)

// Config sources for Bootstrap.ConfigSource.
const (
	// ConfigSourceFile reads the bridge config from a file. It is the default.
	ConfigSourceFile = infra.ConfigSourceFile
	// ConfigSourceDynamoDB reads the bridge config from a DynamoDB table the
	// construct creates.
	ConfigSourceDynamoDB = infra.ConfigSourceDynamoDB
)

// The three deployment shapes.
type (
	// SingleProps configures NewSingle.
	SingleProps = gobridgesingle.SingleProps
	// Single is one Fargate task.
	Single = gobridgesingle.GoBridgeSingle
	// ClusterProps configures NewCluster.
	ClusterProps = gobridgecluster.ClusterProps
	// Cluster is one control task plus workers sharing one config file. It
	// scales throughput; it does not fail over.
	Cluster = gobridgecluster.GoBridgeCluster
	// AutoScaling scales the Cluster workers on CPU.
	AutoScaling = gobridgecluster.AutoScalingProps
	// HAProps configures NewHA.
	HAProps = gobridgedynamodbha.DynamoDBHAProps
	// HA is an active task with warm standbys that take over through a
	// DynamoDB lease.
	HA = gobridgedynamodbha.GoBridgeDynamoDBHA
	// MemberSlots gives each HA worker a fixed member id.
	MemberSlots = gobridgedynamodbha.MemberSlots
	// EfsConfigProps configures NewEfsConfig.
	EfsConfigProps = cdkconstructs.GoBridgeEfsConfigProps
	// EfsConfig is a shared file system for the config file and SQLite stores.
	// A shape creates its own when it needs one.
	EfsConfig = cdkconstructs.GoBridgeEfsConfig
)

// Optional extras.
type (
	// ALBAttachmentProps configures NewALBAttachment.
	ALBAttachmentProps = gobridgealbattachment.AttachmentProps
	// HealthCheckProps tunes the ALB target group health checks.
	HealthCheckProps = gobridgealbattachment.HealthCheckProps
	// ALBAttachment routes an existing ALB listener to a shape.
	ALBAttachment = gobridgealbattachment.GoBridgeALBAttachment
	// AlarmsProps configures NewAlarms.
	AlarmsProps = gobridgealarms.AlarmsProps
	// Alarms is the CloudWatch alarm set for a shape.
	Alarms = gobridgealarms.GoBridgeAlarms
	// BridgeRef reads another stack's bridge through the SSM parameters its
	// ALBAttachment exported.
	BridgeRef = gobridgecdk.BridgeRef
	// ExportOption chooses what an ALBAttachment exports to SSM.
	ExportOption = ssmexports.Option
)

// NewSingle deploys one Fargate task.
func NewSingle(scope constructs.Construct, id string, props *SingleProps) *Single {
	return gobridgesingle.NewGoBridgeSingle(scope, jsii.String(id), props)
}

// NewCluster deploys a control task plus workers.
func NewCluster(scope constructs.Construct, id string, props *ClusterProps) *Cluster {
	return gobridgecluster.NewGoBridgeCluster(scope, jsii.String(id), props)
}

// NewHA deploys an active task with warm standbys.
func NewHA(scope constructs.Construct, id string, props *HAProps) *HA {
	return gobridgedynamodbha.NewGoBridgeDynamoDBHA(scope, jsii.String(id), props)
}

// NewEfsConfig creates a file system to share between constructs.
func NewEfsConfig(scope constructs.Construct, id string, props *EfsConfigProps) *EfsConfig {
	return cdkconstructs.NewGoBridgeEfsConfig(scope, jsii.String(id), props)
}

// NewALBAttachment routes an existing ALB listener to a shape.
func NewALBAttachment(scope constructs.Construct, id string, props *ALBAttachmentProps) *ALBAttachment {
	return gobridgealbattachment.NewGoBridgeALBAttachment(scope, jsii.String(id), props)
}

// NewAlarms creates the CloudWatch alarms for a shape.
func NewAlarms(scope constructs.Construct, id string, props *AlarmsProps) *Alarms {
	return gobridgealarms.NewGoBridgeAlarms(scope, jsii.String(id), props)
}

// ConfigFile reads the bridge config from a YAML or JSON file at synth time.
//
//nolint:ireturn // Config is sealed; see gobridgecdk.
func ConfigFile(path string) Config { return gobridgecdk.BridgeYamlAsset(path) }

// ConfigInline uses a bridge config built in Go, for example with bridgecfg.
//
//nolint:ireturn // Config is sealed; see gobridgecdk.
func ConfigInline(cfg *ports.BridgeConfig) Config { return gobridgecdk.BridgeYamlInline(cfg) }

// ImageFromGoBuild builds GoBridge during cdk deploy. Leaving Version empty
// builds the same version as this package.
//
//nolint:ireturn // Image is sealed; see gobridgecdk.
func ImageFromGoBuild(props GoBuild) Image { return gobridgecdk.ImageFromGoBuild(props) }

// ImageFromRegistry runs a digest-pinned image you built yourself.
//
//nolint:ireturn // Image is sealed; see gobridgecdk.
func ImageFromRegistry(ref string) Image { return gobridgecdk.ImageFromRegistry(ref) }

// ImageFromEcr runs an image from your own ECR repository.
//
//nolint:ireturn // Image is sealed; see gobridgecdk.
func ImageFromEcr(repo awsecr.IRepository, tag string) Image {
	return gobridgecdk.ImageFromEcrRepository(repo, tag)
}

// Lookup reads a bridge that another stack exported under prefix.
func Lookup(scope constructs.Construct, id, prefix string, opts ...ExportOption) *BridgeRef {
	return gobridgecdk.LookupBridge(scope, id, prefix, opts...)
}

// IncludeARNs also exports the ALB and ECS cluster ARNs.
func IncludeARNs() ExportOption { return ssmexports.IncludeARNs() }
