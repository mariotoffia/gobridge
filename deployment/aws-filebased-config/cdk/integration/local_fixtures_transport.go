//go:build integration_local
// +build integration_local

package integration

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// The two topologies that are not a single SQS↔SQS task: the MQTT bridge, and
// the control/worker cluster that shares one config document.

const (
	// The MQTT topics the deployed bridge subscribes to and publishes on. The
	// publish topic is the binding's Address — the transport destination — while
	// the logical Subject travels beside it in the message, which is the
	// distinction the mapping proof exists to pin.
	localMQTTInboundFilter = "gobridge/local/in/#"
	localMQTTInboundTopic  = "gobridge/local/in/sensor"
	localMQTTOutboundTopic = "gobridge/local/out"

	// Two sessions, not one. The ingress session is a durable session that the
	// receiver alone manages: it holds no lease and owns no outbox partition, and
	// its route holds the broker delivery until the queue has accepted it. The
	// egress session only publishes, and connects only once a route holds its
	// lease. A single shared session would have to be both at once, which the
	// runtime refuses — and rightly, because the two have different failover
	// semantics.
	localMQTTIngress  = "mqtt-ingress"
	localMQTTEgress   = "mqtt-egress"
	localMQTTReceiver = "mqtt-in"
	localMQTTSender   = "mqtt-out"
	localSQSInbound   = "sqs-in"
	localSQSOutbound  = "sqs-out"

	// localMQTTHistoryPath is where the durable ingress session keeps the exact
	// filters it installed on the broker. It is on the config mount — the only
	// durable storage a single task has — in a directory of its own, because
	// the store owns its final parent (0700) while the mount itself is 0755.
	localMQTTHistoryDir  = "managed-subscriptions"
	localMQTTHistoryPath = "/var/lib/gobridge/" + localMQTTHistoryDir + "/managed-subscriptions.db"
)

// newLocalMQTTFixture is the single-task MQTT↔SQS topology.
//
// It bridges in both directions on purpose. One route proves an MQTT ingress
// keeps the producer's Subject and delivers to the queue its binding Address
// names; the other proves the reverse, that an SQS ingress Subject survives to
// an MQTT publish on the topic its binding Address names. Address and Subject
// are separate fields with separate jobs, and a bridge that conflated them would
// still pass a one-directional round trip.
func newLocalMQTTFixture(stack awscdk.Stack, env SandboxEnv, topology string) {
	vpc := lookupVpc(stack, env)
	inbound := localBridgeQueue(stack, topology, localSQSInbound)
	outbound := localBridgeQueue(stack, topology, localSQSOutbound)

	queues := registry.NewQueueRegistry()
	queues.AddQueue(localQueueName(topology, localSQSInbound), inbound)
	queues.AddQueue(localQueueName(topology, localSQSOutbound), outbound)

	cfg, err := bridgecfg.New("gobridge-local-mqtt").
		WithHTTPAdminAPI(localAdminOptions()).
		WithMemoryDLQ().
		// The SQS→MQTT route settles through the outbox and its egress session is
		// lease-held, so the deployment has to provide both. They are in process
		// memory because this topology proves a field mapping on one task; a real
		// MQTT deployment uses a durable store. The MQTT→SQS route uses neither.
		WithMemoryOutbox().
		WithMemoryLease().
		// The durable ingress session owes the broker an exact record of the
		// filters it installed (ADR 0003); it is the only store that route needs.
		WithSQLiteManagedSubscriptions(localMQTTHistoryPath).
		WithSQSReceiver(localSQSInbound, queues.Ref(localQueueName(topology, localSQSInbound)),
			byQueueName(localQueueName(topology, localSQSInbound))).
		WithSQSSender(localSQSOutbound, queues.Ref(localQueueName(topology, localSQSOutbound)),
			byQueueName(localQueueName(topology, localSQSOutbound))).
		WithMQTTBroker(localMQTTIngress, fmt.Sprintf("tcp://%s:%d", localBrokerHost, localBrokerPort)).
		WithMQTTBroker(localMQTTEgress, fmt.Sprintf("tcp://%s:%d", localBrokerHost, localBrokerPort)).
		Build()
	if err != nil {
		panic("integration: build the local MQTT config: " + err.Error())
	}
	// The ingress session survives the process: that is what lets the broker
	// redeliver a delivery the bridge never settled, and so what admits the
	// route to direct_hold.
	for i := range cfg.Sessions {
		if cfg.Sessions[i].ID == localMQTTIngress {
			cfg.Sessions[i].SessionMode = "persistent"
		}
	}
	addMQTTLegs(cfg, topology)

	src := gobridgecdk.BridgeYamlInline(cfg)
	single := newLocalSingleService(stack, vpc, env, "gobridge-local-mqtt", src, queues,
		func(props *gobridgesingle.SingleProps) {
			// The ingress session's broker identity is new — this stack is the
			// first thing to connect with it — and attesting that is what lets the
			// durable session start.
			props.ManagedSubscriptionBaselines = map[string][]string{localMQTTIngress: {}}
		})
	localALBAttachment(stack, vpc, env, &gobridgealbattachment.AttachmentProps{
		Single: single, BridgeConfig: src,
	})
	localOutputs(stack, map[string]*string{
		"ClusterArn":         single.Cluster().ClusterArn(),
		"ControlServiceName": single.ControlService().ServiceName(),
	})
}

// addMQTTLegs adds the MQTT receiver, sender, bindings and both routes.
//
// They are assembled by hand because the config builder's fluent surface covers
// SQS transports and MQTT sessions but not MQTT receivers and senders. The
// shapes are the ones the parser round-trips, so the deployed yaml is the same
// document an operator would write.
func addMQTTLegs(cfg *ports.BridgeConfig, topology string) {
	receiver := ports.ReceiverDef{
		ID: localMQTTReceiver, Transport: paho.ShortKind, SessionID: localMQTTIngress,
		Topics: []ports.SubscriptionDef{{Topic: localMQTTInboundFilter, QoS: 1}},
	}
	receiver.SetDecoded(&paho.Config{}, nil)
	sender := ports.SenderDef{ID: localMQTTSender, Transport: paho.ShortKind, SessionID: localMQTTEgress}
	sender.SetDecoded(&paho.Config{}, nil)
	mqttBinding := ports.BindingDef{
		ID: localMQTTSender + "-binding", SenderID: localMQTTSender, SessionID: localMQTTEgress,
		Address: localMQTTOutboundTopic,
	}
	mqttBinding.SetDecoded(&paho.Config{}, nil)
	// The binding names no session: a direct_hold route persists no records, so
	// there is no outbox partition for them to live in.
	sqsBinding := ports.BindingDef{
		ID: localSQSOutbound + "-binding", SenderID: localSQSOutbound,
		Address: localQueueName(topology, localSQSOutbound),
	}
	sqsBinding.SetDecoded(cfg.Senders[0].Config, nil)

	cfg.Receivers = append(cfg.Receivers, receiver)
	cfg.Senders = append(cfg.Senders, sender)
	cfg.Bindings = append(cfg.Bindings, sqsBinding, mqttBinding)
	cfg.Routes = append(cfg.Routes,
		// The ingress holds the broker delivery until the queue has accepted it.
		// A persistent session on a QoS 1 subscription redelivers a delivery the
		// bridge never settled, which is the precondition direct_hold rests on,
		// so the route needs no outbox, no lease and no outbox partition. Nothing
		// but the receiver names the ingress session: the receiver's own binding
		// to it is what connects it and reconciles its subscription.
		ports.RouteDef{
			ID: "mqtt-to-sqs", ReceiverID: localMQTTReceiver, DeliveryMode: "direct_hold",
			Bindings: []string{sqsBinding.ID},
			Policy: ports.PolicyDef{
				MaxInFlight: 10, OnExpired: "dlq", OnPermanentFailure: "dlq",
				// One task, one subscriber. The fence exists to stop two consumers
				// of the same MQTT subscription racing the same destination, and
				// this topology has no second consumer to race.
				AllowUnfenced: true,
			},
		},
		// The egress session has to be ACTIVATED by something, and a session only
		// connects when a route owns it. Without this block the sender's session
		// never connects, every publish fails, and even the dead-letter write is
		// refused because nothing holds the session's lease — which is what a
		// sender-only session looks like when nobody owns it.
		// Shared-outbox here too: the target session is lease-held, and the
		// runtime refuses hold-then-settle into a session that can hand its lease
		// over mid-flight — the held source message would be settled by an owner
		// that no longer owns the destination.
		ports.RouteDef{
			ID: "sqs-to-mqtt", ReceiverID: localSQSInbound, DeliveryMode: "shared_outbox",
			Bindings: []string{mqttBinding.ID},
			Policy: ports.PolicyDef{
				AckAfter: "outbox_persist", MaxInFlight: 10, MaxOutboxDepth: 1000,
				OnExpired: "dlq", OnPermanentFailure: "dlq",
			},
			Session: &ports.RouteSessionDef{
				SessionID: localMQTTEgress, SenderID: localMQTTSender,
				LeaseTTL: "10s", RenewInterval: "2s", RenewJitter: "500ms",
				MaxRenewFails: 3, StepDownGrace: "2s", AcquirePollInterval: "1s",
				// The objective must exceed the budget this session's own connect,
				// reconnect and reconcile timeouts add up to, and this session takes
				// the config builder's defaults for those rather than the tightened
				// values a real HA deployment sets. The runtime refuses the config
				// when the two disagree, which is that check working.
				RenewCallTimeout: "1s", FailoverSLO: "10m", StartupAllowance: "30s",
				DrainInterval: "500ms", DrainBatchSize: 10,
				// A declared objective must say what a node-local broker outage
				// does. One task has no standby to fail over to, so it is recorded
				// as accepted rather than left undeclared.
				BrokerHealthStepDown: "off",
			},
		},
	)
}

// newLocalClusterFixture is the control + worker topology.
//
// The control task mounts the shared config filesystem read-write and the
// workers mount it read-only, which is the arrangement the whole profile is
// built around: one writer of the document, many readers of it. A config change
// committed on the control has to become visible to a worker with no redeploy
// and no restart, and every worker has to compete for the same source queue
// without any message being delivered twice.
func newLocalClusterFixture(stack awscdk.Stack, env SandboxEnv, topology string, workers float64) {
	vpc := lookupVpc(stack, env)
	inbound := localBridgeQueue(stack, topology, localRouteInbound)
	outbound := localBridgeQueue(stack, topology, localRouteOutbound)

	queues := registry.NewQueueRegistry()
	queues.AddQueue(localQueueName(topology, localRouteInbound), inbound)
	queues.AddQueue(localQueueName(topology, localRouteOutbound), outbound)

	cfg, err := bridgecfg.New("gobridge-local-cluster").
		WithHTTPAdminAPI(localAdminOptions()).
		WithMemoryDLQ().
		WithSQSReceiver(localRouteInbound, queues.Ref(localQueueName(topology, localRouteInbound)),
			byQueueName(localQueueName(topology, localRouteInbound))).
		WithSQSSender(localRouteOutbound, queues.Ref(localQueueName(topology, localRouteOutbound)),
			byQueueName(localQueueName(topology, localRouteOutbound))).
		WithRoute(localRouteInbound, localRouteOutbound).
		Build()
	if err != nil {
		panic("integration: build the local cluster config: " + err.Error())
	}

	src := gobridgecdk.BridgeYamlInline(cfg)
	cluster := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Cluster"), &gobridgecluster.ClusterProps{
		Vpc:                vpc,
		VpcSubnets:         subnetSelection(env),
		Image:              awsecs.ContainerImage_FromRegistry(jsii.String(localBridgeImage()), nil),
		Bootstrap:          localBootstrap("gobridge-local-cluster"),
		BridgeConfig:       src,
		QueueRegistry:      queues,
		WorkerDesiredCount: &workers,
	})
	localALBAttachment(stack, vpc, env, &gobridgealbattachment.AttachmentProps{
		Cluster: cluster, BridgeConfig: src,
	})
	localOutputs(stack, map[string]*string{
		"ClusterArn":         cluster.Cluster().ClusterArn(),
		"ControlServiceName": cluster.ControlService().ServiceName(),
		"WorkerServiceName":  cluster.WorkerService().ServiceName(),
	})
}
