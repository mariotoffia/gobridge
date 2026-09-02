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

// The control/worker cluster: one writer of the shared config document, many
// readers of it, all competing for the same source queue.
//
// This is the arrangement the whole profile is built around, and the two things
// that can go wrong with it are invisible at synth. A config the control writes
// that a worker never sees means the fleet silently splits into two
// configurations; a message two workers both deliver means the shared queue is
// not actually shared. Both are asserted on a deployed cluster here.
func TestLocal_ClusterSharedConfigAndScaling(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "cluster"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalClusterFixture(s, env, topology, 1)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// Resolve the deployed queues BEFORE waiting for the members. A queue the
	// deploy did not create surfaces here, naming the resource, instead of eight
	// minutes later as a member that never became ready.
	queues := newLocalQueues(t, topology, localRouteInbound, localRouteOutbound)

	control := stack.Outputs["ControlServiceName"]
	worker := stack.Outputs["WorkerServiceName"]
	controlHosts := stack.WaitServiceReady(t, ctx, control, 1, 8*time.Minute)
	workerHosts := stack.WaitServiceReady(t, ctx, worker, 1, 8*time.Minute)

	t.Run("a_config_the_control_writes_reaches_a_worker", func(t *testing.T) {
		// The control mounts the config filesystem read-write and the worker
		// mounts it read-only, so this is the EFS sharing proof: the only path
		// from the transaction below to the worker's running config is the
		// shared document.
		before := stack.RunningConfigVersion(t, ctx, workerHosts[0])
		commitLogLevel(t, ctx, stack.adminProbe(), controlHosts[0], stack.AdminKey, "debug")
		after := stack.WaitConfigVersionPast(t, ctx, workerHosts[0], before, 5*time.Minute)
		t.Logf("the worker moved from config version %d to %d without a redeploy", before, after)
	})

	t.Run("scaling_workers_out_and_back_delivers_each_message_once", func(t *testing.T) {
		// Everything that survives the scale must arrive exactly once. A
		// duplicate here means two workers took the same message off the shared
		// queue, which is the failure the queue's own visibility window exists
		// to prevent and which only appears with more than one consumer.
		stack.ScaleService(t, ctx, worker, 3)
		stack.WaitServiceReady(t, ctx, worker, 3, 8*time.Minute)

		sent := queues.sendTagged(t, ctx, localRouteInbound, 30)
		received := queues.drain(t, ctx, localRouteOutbound, len(sent), 5*time.Minute)
		testcontent.AssertReceivedSet(t, sent, received)
		testcontent.AssertNoDuplicates(t, received)

		stack.ScaleService(t, ctx, worker, 1)
		stack.WaitServiceReady(t, ctx, worker, 1, 8*time.Minute)

		// And after scaling back in: a worker that went away must not have left
		// a message half-delivered, and the survivors must still carry traffic.
		sentAfter := queues.sendTagged(t, ctx, localRouteInbound, 10)
		receivedAfter := queues.drain(t, ctx, localRouteOutbound, len(sentAfter), 5*time.Minute)
		testcontent.AssertReceivedSet(t, sentAfter, receivedAfter)
		testcontent.AssertNoDuplicates(t, receivedAfter)
	})
}
