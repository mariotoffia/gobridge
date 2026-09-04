//go:build integration_local
// +build integration_local

package integration

import (
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	awssqs "github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// The topologies the local deployment suite stands up, beyond the static
// member-slot cohort.
//
// Each is built from the SHIPPED facade constructs with the shipped config
// builder — the only local-specific values are the runtime image, the region
// and the broker address, which is exactly the set an operator supplies anyway.
// A topology that would be rejected at synth on AWS is rejected here too.

const (
	// The route ids and logical transport ids the data-plane proofs address.
	// They are the names in the deployed bridge config, so a test that names one
	// is naming the deployed thing rather than a stack-generated identifier.
	localRouteInbound  = "inbound"
	localRouteOutbound = "outbound"
	localRoutePoison   = "poison-in"
	localSenderDeadEnd = "dead-end"

	// localAdminURI is the admin key the deployed bridges resolve at boot. It
	// points at the parameter seedLocalParameters writes.
	localAdminURI = "pms://gobridge/local/admin-key"

	// localMetricsNamespace is where a deployed bridge publishes its runtime
	// metrics, and the namespace the rollup alarms read. They must be the same
	// value or an alarm sits in INSUFFICIENT_DATA forever.
	localMetricsNamespace = "GoBridge/Runtime"
)

// localQueueName is the physical SQS queue name for one logical queue of one
// topology.
//
// The name is deterministic and carries the topology, so a stack whose destroy
// left a queue behind cannot collide with a different topology in the same run,
// and a proof can address a queue without plumbing a stack output for it.
func localQueueName(topology, logical string) string {
	return fmt.Sprintf("gobridge-%s-%s", topology, logical)
}

// localBridgeQueue declares one SQS queue with a deterministic name.
func localBridgeQueue(stack awscdk.Stack, topology, logical string) awssqs.Queue {
	return awssqs.NewQueue(stack, jsii.String(strings.ToUpper(logical[:1])+logical[1:]+"Queue"),
		&awssqs.QueueProps{
			QueueName:     jsii.String(localQueueName(topology, logical)),
			RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
		})
}

// byQueueName steers an SQS receiver or sender to resolve its queue URL through
// GetQueueUrl at first use rather than carrying the URL the deploy resolved.
//
// The deployment is what owns the queue either way — the CDK handle is what
// grants the task role access to it — but the URL CloudFormation resolves names
// the emulator's own gateway host, which a container on the deployment network
// does not necessarily reach by that name. Resolving by name uses the endpoint
// the SDK chain already gives the container, which is the same code path an
// operator's own endpoint override takes.
func byQueueName(name string) func(*sqs.Config) {
	return func(c *sqs.Config) {
		c.QueueURL = ""
		c.QueueName = name
	}
}

// localBootstrap is the deployment-owned runtime configuration every local
// topology shares.
func localBootstrap(bridgeID string) infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         bridgeID,
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: localAdminParam,
		AWSRegion:        localRegion,
		// The rollup metrics the declarative alarms read are published only by
		// the CloudWatch exporter, so an observability proof that did not select
		// it would be asserting on an empty namespace.
		MetricsExporter:  infra.MetricsExporterCloudWatch,
		MetricsNamespace: localMetricsNamespace,
	}
}

// localAdminOptions is the admin/monitor HTTP surface every local topology
// exposes. The monitor key is the admin key: the deployment seeds one parameter
// and the monitor endpoints a proof reads are the authenticated ones.
func localAdminOptions() bridgecfg.HTTPAdminAPIOptions {
	opts := bridgecfg.AdminAPIDefaults()
	opts.AdminAPIKey = localAdminURI
	opts.MonitorAPIKey = localAdminURI
	return opts
}

// dlqOnPermanentFailure is the route policy the DLQ proof needs: a send that
// cannot succeed lands the message in the dead-letter store instead of being
// dropped, so it is still there to be redriven.
func dlqOnPermanentFailure() bridgecfg.RouteOption {
	return func(r *ports.RouteDef) {
		r.Policy.OnPermanentFailure = "dlq"
		r.Policy.OnExpired = "dlq"
		// A permanent failure must reach the DLQ promptly rather than after the
		// default retry ladder: the proof asserts the entry exists, and a long
		// backoff would make that a timing race rather than a contract.
		r.Policy.Backoff = ports.BackoffDef{InitialInterval: "200ms", MaxInterval: "1s"}
		r.Policy.MaxReplayAttempts = 2
	}
}

// newLocalSQSFixture is the single-task SQS↔SQS topology, with an ALB
// attachment in front and the alarm bundle beside it.
//
// It carries two routes deliberately. The first is the plain data plane —
// everything sent to the inbound queue must appear on the outbound one. The
// second targets a queue the proof DELETES before using it, so its sends fail
// permanently and land in the dead-letter store, which is the only way to
// exercise redrive on a deployed bridge.
func newLocalSQSFixture(stack awscdk.Stack, env SandboxEnv, topology string) {
	vpc := lookupVpc(stack, env)
	inbound := localBridgeQueue(stack, topology, localRouteInbound)
	outbound := localBridgeQueue(stack, topology, localRouteOutbound)
	poison := localBridgeQueue(stack, topology, localRoutePoison)
	deadEnd := localBridgeQueue(stack, topology, localSenderDeadEnd)

	// The registry is keyed by the PHYSICAL queue name, because that is what the
	// synthesized yaml references once the transports resolve their queue by name
	// — and the construct's validator resolves the reference through this map to
	// decide what the task role is granted.
	queues := registry.NewQueueRegistry()
	queues.AddQueue(localQueueName(topology, localRouteInbound), inbound)
	queues.AddQueue(localQueueName(topology, localRouteOutbound), outbound)
	queues.AddQueue(localQueueName(topology, localRoutePoison), poison)
	queues.AddQueue(localQueueName(topology, localSenderDeadEnd), deadEnd)
	ref := func(logical string) registry.QueueRef {
		return queues.Ref(localQueueName(topology, logical))
	}

	cfg, err := bridgecfg.New("gobridge-local-dataplane").
		WithHTTPAdminAPI(localAdminOptions()).
		WithMemoryDLQ().
		WithSQSReceiver(localRouteInbound, ref(localRouteInbound),
			byQueueName(localQueueName(topology, localRouteInbound))).
		WithSQSSender(localRouteOutbound, ref(localRouteOutbound),
			byQueueName(localQueueName(topology, localRouteOutbound))).
		WithRoute(localRouteInbound, localRouteOutbound).
		WithSQSReceiver(localRoutePoison, ref(localRoutePoison),
			byQueueName(localQueueName(topology, localRoutePoison))).
		WithSQSSender(localSenderDeadEnd, ref(localSenderDeadEnd),
			byQueueName(localQueueName(topology, localSenderDeadEnd))).
		WithRouteOpts(localRoutePoison, []string{localSenderDeadEnd},
			[]bridgecfg.RouteOption{dlqOnPermanentFailure()}).
		Build()
	if err != nil {
		panic("integration: build the local data-plane config: " + err.Error())
	}

	src := gobridgecdk.BridgeYamlInline(cfg)
	single := newLocalSingleService(stack, vpc, env, "gobridge-local-dataplane", src, queues)

	attachment := localALBAttachment(stack, vpc, env, &gobridgealbattachment.AttachmentProps{
		Single: single, BridgeConfig: src,
	})
	topic := awssns.NewTopic(stack, jsii.String("AlarmTopic"), &awssns.TopicProps{
		TopicName: jsii.String(localQueueName(topology, "alarms")),
	})
	gobridgealarms.NewGoBridgeAlarms(stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		Single: single, Efs: single.EfsConfig(), Attachment: attachment, AlarmTopic: topic,
		// The dead-letter alarm is the one an operator is woken by, and this
		// topology produces real dead-letter volume, so it is the alarm the
		// metric-math replay can assert against actual datapoints.
		EnableRollupAlarms:     true,
		RollupMetricsNamespace: jsii.String(localMetricsNamespace),
	})

	localOutputs(stack, map[string]*string{
		"ClusterArn":         single.Cluster().ClusterArn(),
		"ControlServiceName": single.ControlService().ServiceName(),
		"AlarmTopicArn":      topic.TopicArn(),
		"MetricsNamespace":   jsii.String(localMetricsNamespace),
	})
}

// newLocalSingleService is the one-task deployment every single-node topology
// stands on. opts let a topology add what only it needs — a durable session's
// baseline attestation, say — without every single-task shape carrying it.
func newLocalSingleService(
	stack awscdk.Stack,
	vpc awsec2.IVpc,
	env SandboxEnv,
	bridgeID string,
	src source.Source,
	queues *registry.QueueRegistry,
	opts ...func(*gobridgesingle.SingleProps),
) *gobridgesingle.GoBridgeSingle {
	props := &gobridgesingle.SingleProps{
		Vpc:           vpc,
		VpcSubnets:    subnetSelection(env),
		Image:         awsecs.ContainerImage_FromRegistry(jsii.String(localBridgeImage()), nil),
		Bootstrap:     localBootstrap(bridgeID),
		BridgeConfig:  src,
		QueueRegistry: queues,
	}
	for _, opt := range opts {
		opt(props)
	}
	return gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Single"), props)
}

// localALBAttachment builds the load balancer the profile puts in front of a
// deployment and attaches it.
//
// The emulator does not route an ALB to an ECS task, so no local proof sends
// traffic through it. It is deployed anyway for two reasons: the attachment is
// part of the shipped shape and a stack that omitted it would not be the stack
// that deploys, and the target group it creates carries the health-check path
// the deployment expects every task to answer — which a local run CAN check
// against the container directly.
func localALBAttachment(
	stack awscdk.Stack,
	vpc awsec2.IVpc,
	env SandboxEnv,
	props *gobridgealbattachment.AttachmentProps,
) *gobridgealbattachment.GoBridgeALBAttachment {
	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{
		Vpc: vpc, InternetFacing: jsii.Bool(true),
		VpcSubnets: &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PUBLIC},
	})
	listener := alb.AddListener(jsii.String("L"), &elbv2.BaseApplicationListenerProps{
		Port: jsii.Number(80), Protocol: elbv2.ApplicationProtocol_HTTP, Open: jsii.Bool(true),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	props.Listener = listener
	// The fixture's listener is plaintext HTTP:80, so the published
	// URLs have to say http — the construct cannot read the protocol
	// off an imported listener.
	props.ListenerScheme = "http"
	props.Vpc = vpc
	_ = env
	return gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Attach"), props).
		WithCfnOutputs("")
}

// localOutputs publishes the stack outputs a local proof addresses the
// deployment through, with stable logical ids.
func localOutputs(stack awscdk.Stack, values map[string]*string) {
	for name, value := range values {
		awscdk.NewCfnOutput(stack, jsii.String(name), &awscdk.CfnOutputProps{Value: value}).
			OverrideLogicalId(jsii.String(name))
	}
}
