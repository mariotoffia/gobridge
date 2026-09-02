//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/mariotoffia/gobridge/testutil/testcontent"
)

// A deployed MQTT↔SQS bridge, asserted on the two fields that are easiest to
// conflate and most expensive to get wrong.
//
// Subject is the producer's logical event name and travels WITH the message.
// Address is the transport destination chosen at egress and belongs to the
// binding. A bridge that wrote the destination into the subject, or published on
// the subject instead of the address, would still pass a plain round trip — so
// both directions assert the pair, not the payload alone.
func TestLocal_MQTTSubjectAndAddressMapping(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "mqtt"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalMQTTFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// Resolve the deployed queues BEFORE waiting for the member. A queue the
	// deploy did not create surfaces here, naming the resource, instead of eight
	// minutes later as a member that never became ready.
	queues := newLocalQueues(t, topology, localSQSInbound, localSQSOutbound)
	stack.WaitServiceReady(t, ctx, stack.Outputs["ControlServiceName"], 1, 8*time.Minute)
	broker := newLocalBroker(t)

	t.Run("mqtt_ingress_keeps_the_subject_and_uses_the_bindings_address", func(t *testing.T) {
		const subject = "sensor.temperature.reading"
		tid := broker.publishTagged(t, ctx, localMQTTInboundTopic, subject)

		// The queue read here is the one the binding's Address names. Arriving on
		// it IS the address assertion: no other queue is bridged from MQTT.
		messages := queues.receiveWithAttributes(t, ctx, localSQSOutbound, 1, 2*time.Minute)
		received := testcontent.ReceivedFromBodies([]string{aws.ToString(messages[0].Body)})
		if received[0].TID != tid {
			t.Fatalf("the message on %s carries %q, want the published %q",
				localQueueName(topology, localSQSOutbound), received[0].TID, tid)
		}
		attribute, ok := messages[0].MessageAttributes["Subject"]
		if !ok {
			t.Fatalf("the bridged message carries no Subject attribute, so the producer's logical "+
				"subject was lost crossing MQTT → SQS (attributes: %v)", messages[0].MessageAttributes)
		}
		if got := aws.ToString(attribute.StringValue); got != subject {
			t.Fatalf("bridged Subject = %q, want the published %q", got, subject)
		}
	})

	t.Run("sqs_ingress_keeps_the_subject_and_publishes_on_the_bindings_address", func(t *testing.T) {
		const subject = "command.actuator.set"
		// Subscribe before sending: the deployed bridge publishes as soon as it
		// picks the message up, and a subscription opened afterwards would miss it.
		delivered := broker.subscribe(t, ctx, localMQTTOutboundTopic)
		tid := queues.sendTaggedWithSubject(t, ctx, localSQSInbound, subject)

		select {
		case <-time.After(2 * time.Minute):
			t.Fatalf("nothing was published on %s within the budget", localMQTTOutboundTopic)
		case message := <-delivered:
			received := testcontent.ReceivedFromBodies([]string{string(message.payload)})
			if received[0].TID != tid {
				t.Fatalf("the message published on %s carries %q, want %q",
					localMQTTOutboundTopic, received[0].TID, tid)
			}
			// Publishing on the binding's Address is asserted by the topic the
			// subscription is on; the Subject travels beside it as a user
			// property, which is the only place it can survive on MQTT.
			if message.subject != subject {
				t.Fatalf("the published message carries subject %q, want %q", message.subject, subject)
			}
		}
	})
}
