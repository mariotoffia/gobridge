//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"

	"github.com/mariotoffia/gobridge/testutil/testcontent"
)

// The deployed data plane: a single task that must actually carry messages from
// one queue to another.
//
// Everything here is asserted against the DEPLOYED stack — the config the
// deployment seeded, the task CloudFormation created, the queues the stack
// declared. What a local run cannot prove is named where it matters: the
// emulator does not route a load balancer to an ECS task, so no assertion here
// travels through the ALB the stack also deploys.
func TestLocal_SQSDataPlane(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "dataplane"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalSQSFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Resolve the deployed queues BEFORE waiting for the member. A queue the
	// deploy did not create surfaces here, naming the resource, instead of eight
	// minutes later as a member that never became ready.
	queues := newLocalQueues(t, topology, localRouteInbound, localRouteOutbound)
	stack.WaitServiceReady(t, ctx, stack.Outputs["ControlServiceName"], 1, 8*time.Minute)

	t.Run("roundtrip_preserves_content", func(t *testing.T) {
		sent := queues.sendTagged(t, ctx, localRouteInbound, 1)
		received := queues.drain(t, ctx, localRouteOutbound, len(sent), 2*time.Minute)
		testcontent.AssertReceivedSet(t, sent, received)
	})

	t.Run("batch_of_ten_arrives_once_each", func(t *testing.T) {
		sent := queues.sendTagged(t, ctx, localRouteInbound, 10)
		received := queues.drain(t, ctx, localRouteOutbound, len(sent), 3*time.Minute)
		testcontent.AssertReceivedSet(t, sent, received)
		testcontent.AssertNoDuplicates(t, received)
	})
}
