//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// The shape a deploy leaves behind, asserted against the deployed stack rather
// than against the template that would produce it.
//
// Synth tests already pin what the profile declares. These pin what survives a
// real CloudFormation run: that the outputs an operator scripts against are
// present and well-formed, that re-running the same deploy is a no-op rather
// than a service replacement, that the health-check path the profile declares is
// one the container actually answers, and that destroying the stack leaves
// nothing behind.
func TestLocal_DeploymentShape(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "shape"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalSQSFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	service := stack.Outputs["ControlServiceName"]
	hosts := stack.WaitServiceReady(t, ctx, service, 1, 8*time.Minute)

	t.Run("outputs_are_well_formed", func(t *testing.T) {
		// Every output an operator or a downstream stack consumes. A missing one
		// is a stack nobody can address; a malformed one is worse, because it
		// only fails where it is used.
		if arn := strings.TrimSpace(stack.Outputs["ClusterArn"]); !strings.HasPrefix(arn, "arn:aws:ecs:") {
			t.Errorf("output ClusterArn = %q, want an ECS cluster ARN", arn)
		}
		if strings.TrimSpace(stack.Outputs["ControlServiceName"]) == "" {
			t.Error("output ControlServiceName is absent, so nothing can address the deployed service")
		}
		// The two URL outputs must be absolute, must address the load balancer
		// this stack deployed, must carry the paths the API is served on, and
		// must name the scheme their listener actually speaks. This fixture
		// attaches a plaintext HTTP:80 listener and declares it, so an https
		// URL here would hand a consumer a connection failure rather than a
		// 404.
		for name, wantPath := range map[string]string{
			"AdminURL":   "/api/v1/",
			"HealthzURL": "/api/v1/monitor/health",
		} {
			value := strings.TrimSpace(stack.Outputs[name])
			parsed, err := url.Parse(value)
			if err != nil || !parsed.IsAbs() || parsed.Host == "" {
				t.Errorf("output %s = %q, want an absolute URL", name, value)
				continue
			}
			if parsed.Path != wantPath {
				t.Errorf("output %s addresses %q, want the path %q", name, parsed.Path, wantPath)
			}
			if !strings.Contains(parsed.Host, "elb.") {
				t.Errorf("output %s addresses %q, which is not the load balancer this stack deployed",
					name, parsed.Host)
			}
			if parsed.Scheme != "http" {
				t.Errorf("output %s is published as %q, but this stack's listener is plaintext HTTP",
					name, parsed.Scheme)
			}
		}
	})

	t.Run("health_check_path_is_one_the_container_answers", func(t *testing.T) {
		// The emulator does not route a load balancer to an ECS task, so the
		// health check never runs locally. What CAN be checked is the pair the
		// gap would otherwise hide: the path and port the target group declares,
		// against the container itself. A profile that declared a path the
		// runtime does not serve would take every task out of service on AWS and
		// no synth test would see it.
		checks := targetGroupHealthChecks(t, stack)
		if len(checks) == 0 {
			t.Fatal("the deployed stack declares no target group, so nothing pins the health-check path")
		}
		for _, check := range checks {
			status, body, err := stack.Call(ctx, "GET", slotURL(hosts[0], check.Port, check.Path), nil, nil)
			if err != nil {
				t.Fatalf("probe the declared health-check path %s:%d%s: %v",
					hosts[0], check.Port, check.Path, err)
			}
			if status != 200 {
				t.Errorf("the deployment health-checks %s on port %d and expects %s, but the container "+
					"answered %d: %s", check.Path, check.Port, check.Codes, status, truncateBody(body))
			}
		}
	})

	t.Run("task_role_is_assumable_and_scoped_to_this_stacks_queues", func(t *testing.T) {
		// The deployment's own credentials, not the harness's. The granted half
		// is executed: the role exists, is assumable, and can send to the queue
		// its own sender is bound to, which no synth assertion can establish.
		//
		// The DENIED half cannot be executed here. The emulator does not
		// evaluate IAM — a call the role has no grant for succeeds — so a
		// "denied" assertion would pass on any policy at all, including an empty
		// one, and would be evidence of nothing. What IS checked instead is the
		// policy CloudFormation actually attached to the deployed role: every
		// SQS statement must name this stack's own queues and none may be a
		// wildcard. A grant that is too wide is then visible where it is made,
		// and the refusal itself rests on AWS's IAM.
		role := stack.TaskRoleARN(t, ctx, service)
		asRole := sqs.NewFromConfig(assumeTaskRole(t, ctx, role))

		queues := newLocalQueues(t, topology, localRouteOutbound)
		granted := queues.URL(t, localRouteOutbound)
		if _, err := asRole.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: aws.String(granted), MessageBody: aws.String("least-privilege probe"),
		}); err != nil {
			t.Fatalf("the task role cannot send to the queue its own sender is bound to (%s): %v",
				granted, err)
		}
		drainOne(t, ctx, asRole, granted)

		assertQueueGrantsAreScoped(t, ctx, role, localQueueName(topology, ""))
	})
}

// Deploying the SAME cloud assembly a second time must change nothing.
//
// It gets its own deployment rather than sharing one: a redeploy that fails
// leaves the stack in a state every later assertion would then be reading, and a
// failure of one shape question would be reported as a failure of the others.
func TestLocal_RedeployIsIdempotent(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "redeploy"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalSQSFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	service := stack.Outputs["ControlServiceName"]
	stack.WaitServiceReady(t, ctx, service, 1, 8*time.Minute)

	// Put every service back on the task definition CloudFormation registered.
	// The harness rolled them off it to restore the storage the emulator dropped,
	// and CloudFormation does not know that happened — so without this the second
	// deploy answers a question about the harness.
	restoreDeployedTaskDefinitions(t, ctx, stack.Outputs["ClusterArn"])

	before := serviceIdentity(t, ctx, stack, service)
	// A service that came back with a new ARN or a new creation time would mean
	// the profile churns its workload on every deploy, which is a rolling outage
	// nobody asked for.
	if err := redeployStack(t, stack); err != nil {
		if emulatorCannotUpdateECSService(err) {
			t.Skipf("the emulator cannot update an AWS::ECS::Service — it reports the service it "+
				"created as not found and then cannot roll back — so whether a redeploy of this "+
				"profile is a no-op stays a synth and credentialed question: %v", err)
		}
		t.Fatalf("re-deploying the same assembly failed: %v", err)
	}
	after := serviceIdentity(t, ctx, stack, service)
	if before != after {
		t.Fatalf("re-deploying the same assembly replaced the service: %s → %s", before, after)
	}
}

// Destroying the stack must take everything it created with it.
func TestLocal_DestroyLeavesNothing(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "destroy"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalSQSFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	stack.WaitServiceReady(t, ctx, stack.Outputs["ControlServiceName"], 1, 8*time.Minute)

	// Destroy explicitly rather than through the registered cleanup, so the
	// aftermath can be asserted. The cleanup still runs and is a no-op on an
	// already-destroyed stack.
	quiesceServices(t, stack.Outputs)
	DestroyStack(t, stack.Env, stack.StackName, stack.AssemblyD)

	client := cloudformation.NewFromConfig(localAWSConfig(t))
	err := pollUntil(ctx, 3*time.Second, 5*time.Minute, func() (bool, error) {
		out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(stack.StackName),
		})
		if err != nil {
			// The stack is gone, which is the whole point.
			return true, nil
		}
		for _, described := range out.Stacks {
			if string(described.StackStatus) != "DELETE_COMPLETE" {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("stack %s was still present after destroy: %v", stack.StackName, err)
	}
	// The queues the stack declared carry a DESTROY removal policy, so they must
	// go with it. A retained queue is a resource an operator pays for and a name
	// the next deploy collides with.
	requireQueueAbsent(t, ctx, localQueueName(topology, localRouteInbound))
	requireQueueAbsent(t, ctx, localQueueName(topology, localRouteOutbound))
}

// serviceIdentity is what must not change across a no-op redeploy: which service

// exists, and when it was created.
func serviceIdentity(t *testing.T, ctx context.Context, stack LocalStack, service string) string {
	t.Helper()
	described, err := stack.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster: aws.String(stack.Outputs["ClusterArn"]), Services: []string{service},
	})
	if err != nil || len(described.Services) != 1 {
		t.Fatalf("describe service %s: %v", service, err)
	}
	created := described.Services[0].CreatedAt
	if created == nil {
		t.Fatalf("service %s reports no creation time, so a replacement cannot be detected", service)
	}
	return aws.ToString(described.Services[0].ServiceArn) + "@" + created.UTC().Format(time.RFC3339Nano)
}
