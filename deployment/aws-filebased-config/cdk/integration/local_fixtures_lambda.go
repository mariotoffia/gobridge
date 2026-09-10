//go:build integration_local
// +build integration_local

package integration

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

// The topology with a Go function on each side of the bridge.

// localLambdaResults is the queue the consumer function writes to. Nothing else
// in the topology can write it — the bridge does not know it exists and its
// send grant belongs to the consumer alone — so a message on it has been
// through the whole loop.
const localLambdaResults = "results"

// newLocalLambdaFixture is the single-task SQS↔SQS bridge with a producer
// function in front of it and a consumer function behind it.
//
// The bridge half is deliberately the plainest route the profile has: what this
// topology exists to prove is the two ends, and a route with its own failure
// modes would make a silent results queue ambiguous. The producer is invoked
// directly and has no event source; the consumer has one and is never invoked
// by the test, so the only way anything reaches the results queue is the
// mapping doing its job.
func newLocalLambdaFixture(
	stack awscdk.Stack,
	env SandboxEnv,
	topology, assetDir string,
	arch awslambda.Architecture,
) {
	vpc := lookupVpc(stack, env)
	inbound := localBridgeQueue(stack, topology, localRouteInbound)
	outbound := localBridgeQueue(stack, topology, localRouteOutbound)
	results := localBridgeQueue(stack, topology, localLambdaResults)

	// Only the two queues the bridge itself addresses go in the registry: it is
	// what the facade grants the task role against, and the results queue is the
	// consumer's, not the bridge's.
	queues := registry.NewQueueRegistry()
	queues.AddQueue(localQueueName(topology, localRouteInbound), inbound)
	queues.AddQueue(localQueueName(topology, localRouteOutbound), outbound)

	cfg, err := bridgecfg.New("gobridge-local-lambda").
		WithHTTPAdminAPI(localAdminOptions()).
		WithMemoryDLQ().
		WithSQSReceiver(localRouteInbound, queues.Ref(localQueueName(topology, localRouteInbound)),
			byQueueName(localQueueName(topology, localRouteInbound))).
		WithSQSSender(localRouteOutbound, queues.Ref(localQueueName(topology, localRouteOutbound)),
			byQueueName(localQueueName(topology, localRouteOutbound))).
		WithRoute(localRouteInbound, localRouteOutbound).
		Build()
	if err != nil {
		panic("integration: build the local Lambda config: " + err.Error())
	}

	src := gobridgecdk.BridgeYamlInline(cfg)
	single := newLocalSingleService(stack, vpc, env, "gobridge-local-lambda", src, queues)

	producer := localLambdaFunction(stack, "ProducerFunction", assetDir, "producer", arch, inbound)
	consumer := localLambdaFunction(stack, "ConsumerFunction", assetDir, "consumer", arch, results)
	localLambdaSQSTrigger(consumer, outbound)

	// No load balancer attachment. It is deployed by the topologies whose proofs
	// need the health-check parity it carries; this one addresses the deployed
	// member the same way they do and would learn nothing from a second copy.
	localOutputs(stack, map[string]*string{
		"ClusterArn":           single.Cluster().ClusterArn(),
		"ControlServiceName":   single.ControlService().ServiceName(),
		"ProducerFunctionName": producer.FunctionName(),
		"ConsumerFunctionName": consumer.FunctionName(),
		"OutboundQueueArn":     outbound.QueueArn(),
	})
}
