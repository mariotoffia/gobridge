//go:build integration_aws
// +build integration_aws

package integration

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	awssqs "github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"

	// Side-effect imports register the plugin kinds the YAML parser
	// needs to see when the bridge config is materialised at synth
	// time (admin api, sqs, sqlite stores).
	_ "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	_ "github.com/mariotoffia/gobridge/adapters/native/store"
)

// integrationFixture bundles the resolved CDK objects shared by
// scenarios so each test file can stay focused on its scenario.
type integrationFixture struct {
	Stack      awscdk.Stack
	Vpc        awsec2.IVpc
	Listener   elbv2.IApplicationListener
	InboundQ   awssqs.Queue
	OutboundQ  awssqs.Queue
	Attachment *gobridgealbattachment.GoBridgeALBAttachment
}

// lookupVpc imports concrete VPC/subnet/AZ attributes. Unlike Vpc.FromLookup,
// this emits a complete immutable cloud assembly in one source-safe synth pass.
func lookupVpc(stack awscdk.Stack, env SandboxEnv) awsec2.IVpc {
	zones := stringPointers(env.AvailabilityZones)
	privateSubnets := stringPointers(env.SubnetIDs)
	publicSubnets := stringPointers(env.PublicSubnetIDs)
	return awsec2.Vpc_FromVpcAttributes(stack, jsii.String("Vpc"), &awsec2.VpcAttributes{
		VpcId: jsii.String(env.VpcID), AvailabilityZones: &zones,
		PrivateSubnetIds: &privateSubnets, PublicSubnetIds: &publicSubnets,
		Region: jsii.String(env.Region),
	})
}

func stringPointers(values []string) []*string {
	out := make([]*string, 0, len(values))
	for _, value := range values {
		out = append(out, jsii.String(value))
	}
	return out
}

func subnetSelection(env SandboxEnv) *awsec2.SubnetSelection {
	subnetIDs := make([]*string, 0, len(env.SubnetIDs))
	for _, id := range env.SubnetIDs {
		subnetIDs = append(subnetIDs, jsii.String(id))
	}
	filters := []awsec2.SubnetFilter{awsec2.SubnetFilter_ByIds(&subnetIDs)}
	return &awsec2.SubnetSelection{SubnetFilters: &filters}
}

// newSingleFixture spins up a single-task GoBridge stack with one
// SQS receiver / one SQS sender and an internet-facing ALB listener
// in front. Returns the queues so the round-trip test can address
// them directly via the AWS SDK after the deploy completes.
func newSingleFixture(stack awscdk.Stack, env SandboxEnv) integrationFixture {
	vpc := lookupVpc(stack, env)

	inbound := awssqs.NewQueue(stack, jsii.String("InboundQ"), &awssqs.QueueProps{
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})
	outbound := awssqs.NewQueue(stack, jsii.String("OutboundQ"), &awssqs.QueueProps{
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	qr := registry.NewQueueRegistry()
	qr.AddQueue("inbound", inbound)
	qr.AddQueue("outbound", outbound)

	adminOpts := bridgecfg.AdminAPIDefaults()
	adminOpts.AdminAPIKey = "pms://gobridge/it/admin-key"

	cfg, err := bridgecfg.New("it-bridge").
		WithHTTPAdminAPI(adminOpts).
		WithSQSReceiver("inbound", qr.Ref("inbound")).
		WithSQSSender("outbound", qr.Ref("outbound")).
		WithRoute("inbound", "outbound").
		Build()
	if err != nil {
		panic("integration: build bridgecfg: " + err.Error())
	}

	src := gobridgecdk.BridgeYamlInline(cfg)

	bootstrap := infra.BootstrapConfig{
		BridgeID:         "it-bridge",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/gobridge/it/admin-key",
	}

	single := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Single"), &gobridgesingle.SingleProps{
		Vpc:           vpc,
		VpcSubnets:    subnetSelection(env),
		Image:         awsecs.ContainerImage_FromRegistry(jsii.String("ghcr.io/mariotoffia/gobridge:latest"), nil),
		Bootstrap:     bootstrap,
		BridgeConfig:  src,
		QueueRegistry: qr,
	})

	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{
		Vpc:            vpc,
		InternetFacing: jsii.Bool(true),
		VpcSubnets:     &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PUBLIC},
	})
	listener := alb.AddListener(jsii.String("L"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		Protocol:      elbv2.ApplicationProtocol_HTTP,
		Open:          jsii.Bool(true),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})

	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Attach"), &gobridgealbattachment.AttachmentProps{
		Single:       single,
		Listener:     listener,
		Vpc:          vpc,
		BridgeConfig: gobridgecdk.BridgeYamlInline(cfg),
	}).WithCfnOutputs("")

	return integrationFixture{
		Stack:      stack,
		Vpc:        vpc,
		Listener:   listener,
		InboundQ:   inbound,
		OutboundQ:  outbound,
		Attachment: att,
	}
}

// newClusterFixture spins up a clustered GoBridge stack with one
// control + WorkerDesiredCount=2 worker tasks behind an ALB.
func newClusterFixture(stack awscdk.Stack, env SandboxEnv) integrationFixture {
	vpc := lookupVpc(stack, env)

	inbound := awssqs.NewQueue(stack, jsii.String("InboundQ"), &awssqs.QueueProps{
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})
	outbound := awssqs.NewQueue(stack, jsii.String("OutboundQ"), &awssqs.QueueProps{
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})

	qr := registry.NewQueueRegistry()
	qr.AddQueue("inbound", inbound)
	qr.AddQueue("outbound", outbound)

	adminOpts := bridgecfg.AdminAPIDefaults()
	adminOpts.AdminAPIKey = "pms://gobridge/it/admin-key"

	cfg, err := bridgecfg.New("it-bridge-cluster").
		WithHTTPAdminAPI(adminOpts).
		WithSQSReceiver("inbound", qr.Ref("inbound")).
		WithSQSSender("outbound", qr.Ref("outbound")).
		WithRoute("inbound", "outbound").
		Build()
	if err != nil {
		panic("integration: build bridgecfg: " + err.Error())
	}

	src := gobridgecdk.BridgeYamlInline(cfg)

	bootstrap := infra.BootstrapConfig{
		BridgeID:         "it-bridge-cluster",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/gobridge/it/admin-key",
	}

	desired := float64(2)
	cluster := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Cluster"), &gobridgecluster.ClusterProps{
		Vpc:                vpc,
		VpcSubnets:         subnetSelection(env),
		Image:              awsecs.ContainerImage_FromRegistry(jsii.String("ghcr.io/mariotoffia/gobridge:latest"), nil),
		Bootstrap:          bootstrap,
		BridgeConfig:       src,
		QueueRegistry:      qr,
		WorkerDesiredCount: &desired,
	})

	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{
		Vpc:            vpc,
		InternetFacing: jsii.Bool(true),
		VpcSubnets:     &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PUBLIC},
	})
	listener := alb.AddListener(jsii.String("L"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		Protocol:      elbv2.ApplicationProtocol_HTTP,
		Open:          jsii.Bool(true),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})

	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Attach"), &gobridgealbattachment.AttachmentProps{
		Cluster:      cluster,
		Listener:     listener,
		Vpc:          vpc,
		BridgeConfig: gobridgecdk.BridgeYamlInline(cfg),
	}).WithCfnOutputs("")

	// Also surface the worker service name as a CFN output so the
	// scale/kill scenario can target it via the ECS SDK without
	// guessing.
	awscdk.NewCfnOutput(stack, jsii.String("WorkerServiceName"), &awscdk.CfnOutputProps{
		Value: cluster.WorkerService().ServiceName(),
	}).OverrideLogicalId(jsii.String("WorkerServiceName"))
	awscdk.NewCfnOutput(stack, jsii.String("ClusterArn"), &awscdk.CfnOutputProps{
		Value: cluster.Cluster().ClusterArn(),
	}).OverrideLogicalId(jsii.String("ClusterArn"))

	return integrationFixture{
		Stack:      stack,
		Vpc:        vpc,
		Listener:   listener,
		InboundQ:   inbound,
		OutboundQ:  outbound,
		Attachment: att,
	}
}
