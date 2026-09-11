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

// Losing a task must not lose a message.
//
// The contract is at-least-once, so a duplicate after a restart is allowed and a
// LOSS is not. That is why this accounts for every message by identity rather
// than by count: a count-based check passes when one message is lost and another
// duplicated, which is exactly the state this exists to catch.
func TestLocal_TaskRestartLosesNoInFlightMessage(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "restart"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalSQSFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Resolve the deployed queues BEFORE waiting for the member. A queue the
	// deploy did not create surfaces here, naming the resource, instead of eight
	// minutes later as a member that never became ready.
	queues := newLocalQueues(t, topology, localRouteInbound, localRouteOutbound)

	service := stack.Outputs["ControlServiceName"]
	stack.WaitServiceReady(t, ctx, service, 1, 8*time.Minute)

	// Half the batch goes to a healthy bridge; the task is then stopped WITHOUT
	// waiting for its replacement, and the rest is sent while the deployment has
	// no consumer at all. That second half is the case the contract is about —
	// work that arrives across a restart — and sending it after the stop is what
	// makes the restart matter on every run rather than on the lucky ones: a
	// batch sent to a running bridge is drained faster than a stop can be issued,
	// which is exactly how a proof like this passes while proving nothing.
	first := queues.sendTagged(t, ctx, localRouteInbound, 60)
	victim, _ := stack.StopOneTaskNow(t, ctx, service, "local deployment proof: no message is lost across a restart")
	second := queues.sendTagged(t, ctx, localRouteInbound, 60)

	outstanding := queues.outstanding(t, ctx, localRouteInbound)
	if outstanding == 0 {
		t.Fatal("the source held no work while the task was away, so the replacement had nothing to " +
			"pick up and this run would prove nothing about losing it")
	}
	t.Logf("stopped task %s; %d messages were on the source while the deployment had no consumer",
		victim, outstanding)
	stack.WaitServiceReady(t, ctx, service, 1, 8*time.Minute)

	sent := append(append([]testcontent.Expected{}, first...), second...)
	received := queues.drain(t, ctx, localRouteOutbound, len(sent), 8*time.Minute)
	testcontent.AssertReceivedSet(t, sent, received)
}
