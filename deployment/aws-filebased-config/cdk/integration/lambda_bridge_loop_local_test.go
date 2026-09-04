//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/testutil/testcontent"
)

// A Go function on each side of the deployed bridge, carrying messages through
// the whole loop.
//
// The shape is the one an operator gets from this profile when the bridge sits
// between two Lambdas: a producer with no event source, invoked directly, that
// puts a message on the bridge's inbound queue; the deployed bridge, which
// carries it to its outbound queue; and a consumer the test NEVER invokes,
// driven only by an event source mapping on that queue, which puts what it
// received on a results queue nothing else in the stack can write.
//
// That last property is what makes the assertion mean anything. The results
// queue is outside the bridge's queue registry, so the task role has no grant
// against it, and the consumer's send grant is the only one there is — a
// message on it has crossed all three hops or it is not there.
func TestLocal_LambdaProducerAndConsumer(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "lambda"
	assetDir, arch := buildLambdaFunctionAsset(t)
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalLambdaFixture(s, env, topology, assetDir, arch)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Resolve the deployed queues BEFORE waiting for the member. A queue the
	// deploy did not create surfaces here, naming the resource, instead of eight
	// minutes later as a member that never became ready.
	queues := newLocalQueues(t, topology, localRouteInbound, localRouteOutbound, localLambdaResults)
	stack.WaitServiceReady(t, ctx, stack.Outputs["ControlServiceName"], 1, 8*time.Minute)

	functions := lambdasvc.NewFromConfig(localAWSConfig(t))

	// Both ends are pinned to the queue they are supposed to be on, because
	// nothing observable downstream distinguishes a loop that went through the
	// bridge from one that skipped it: a producer that wrote straight to the
	// outbound queue would fill the results queue just the same. What proves the
	// middle hop is the pair — the producer sends to the bridge's INBOUND queue,
	// the consumer is driven by its OUTBOUND one, and the deployed route is the
	// only thing that connects them.
	t.Run("the_producer_sends_to_the_bridges_inbound_queue", func(t *testing.T) {
		producer := stack.Outputs["ProducerFunctionName"]
		if producer == "" {
			t.Fatal("the stack published no ProducerFunctionName")
		}
		deployed, err := functions.GetFunctionConfiguration(ctx, &lambdasvc.GetFunctionConfigurationInput{
			FunctionName: aws.String(producer),
		})
		if err != nil {
			t.Fatalf("read the producer's deployed configuration: %v", err)
		}
		var variables map[string]string
		if deployed.Environment != nil {
			variables = deployed.Environment.Variables
		}
		if got, want := variables["GOBRIDGE_FN_TARGET_QUEUE"],
			localQueueName(topology, localRouteInbound); got != want {
			t.Fatalf("the producer forwards to %q, want the bridge's inbound queue %q — a message "+
				"reaching the results queue would then never have crossed the bridge (environment: %v)",
				got, want, variables)
		}
	})

	t.Run("the_consumer_is_driven_by_a_mapping_on_the_bridges_outbound_queue", func(t *testing.T) {
		consumer := stack.Outputs["ConsumerFunctionName"]
		if consumer == "" {
			t.Fatal("the stack published no ConsumerFunctionName")
		}
		listed, err := functions.ListEventSourceMappings(ctx, &lambdasvc.ListEventSourceMappingsInput{
			FunctionName: aws.String(consumer),
		})
		if err != nil {
			t.Fatalf("list the consumer's event source mappings: %v", err)
		}
		if len(listed.EventSourceMappings) != 1 {
			t.Fatalf("the consumer has %d event source mappings, want exactly one: %+v",
				len(listed.EventSourceMappings), listed.EventSourceMappings)
		}
		mapping := listed.EventSourceMappings[0]
		if got, want := aws.ToString(mapping.EventSourceArn), stack.Outputs["OutboundQueueArn"]; got != want {
			t.Fatalf("the consumer's mapping reads %q, want the bridge's outbound queue %q", got, want)
		}
		if state := aws.ToString(mapping.State); state != "Enabled" {
			t.Fatalf("the consumer's mapping is %q, want Enabled", state)
		}
		// The producer has none: it is invoked, never triggered. A mapping here
		// would mean the topology is not the one the proof describes.
		producer, err := functions.ListEventSourceMappings(ctx, &lambdasvc.ListEventSourceMappingsInput{
			FunctionName: aws.String(stack.Outputs["ProducerFunctionName"]),
		})
		if err != nil {
			t.Fatalf("list the producer's event source mappings: %v", err)
		}
		if len(producer.EventSourceMappings) != 0 {
			t.Fatalf("the producer has %d event source mappings, want none: %+v",
				len(producer.EventSourceMappings), producer.EventSourceMappings)
		}
	})

	t.Run("a_message_crosses_producer_bridge_and_consumer_intact", func(t *testing.T) {
		const messages = 3
		sent := make([]testcontent.Expected, 0, messages)
		for i := 0; i < messages; i++ {
			envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
				Subject: "gobridge.local.deployment",
				Payload: []byte(`{"scenario":"` + topology + `"}`),
			})
			_, expected := testcontent.Tag(envelope)
			invokeProducer(t, ctx, functions, stack.Outputs["ProducerFunctionName"], expected.Payload)
			sent = append(sent, expected)
		}

		received := queues.drain(t, ctx, localLambdaResults, len(sent), 3*time.Minute)
		// Bodies, not just identifiers. Both functions forward what they were
		// given verbatim and the route is a pass-through, so anything that
		// rewrote a payload on the way would be a real defect — and a set
		// comparison on the tag alone would not see it.
		testcontent.AssertContentMatches(t, sent, received, testcontent.MatchPayloadExact())
		testcontent.AssertNoDuplicates(t, received)
	})
}

// invokeProducer calls the producer function synchronously with payload, which
// is the body it forwards to the bridge's inbound queue.
//
// A synchronous invocation is what makes the failure legible: a function that
// could not reach the queue reports it in FunctionError here, rather than
// leaving the test to time out on an empty results queue three minutes later
// with nothing to say about why.
func invokeProducer(
	t *testing.T,
	ctx context.Context,
	client *lambdasvc.Client,
	name string,
	payload []byte,
) {
	t.Helper()
	if name == "" {
		t.Fatal("the stack published no ProducerFunctionName")
	}
	out, err := client.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(name),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
		Payload:        payload,
	})
	if err != nil {
		t.Fatalf("invoke the producer function %s: %v", name, err)
	}
	if failure := aws.ToString(out.FunctionError); failure != "" {
		t.Fatalf("the producer function failed with %s: %s", failure, string(out.Payload))
	}
}
