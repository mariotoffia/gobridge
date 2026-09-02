//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// One deployed stack on local emulation, and the handles every proof needs to
// drive it.
//
// The static member-slot cohort is one shape this can take; the data-plane,
// deployment-shape and resilience topologies are others. They all deploy the
// SAME way — synthesize, rewrite what the emulator cannot back, deploy through
// cdklocal, mirror the tables, restore the task storage — so that sequence lives
// here once rather than once per topology.

// monitorReadyPath is the endpoint that answers 200 from the point a member is
// carrying traffic. It is the shipped surface, so a proof that waits on it is
// waiting on what an operator waits on.
const monitorReadyPath = "/api/v1/monitor/ready"

// LocalStack is a deployed stack plus what is needed to address it.
type LocalStack struct {
	Outputs   StackOutputs
	Env       SandboxEnv
	StackName string
	AssemblyD string
	AdminKey  string

	clusterARN string
	ecs        *ecs.Client
	backend    *localBackend
}

// DeployLocal synthesizes a stack built by build, deploys it against local
// emulation and returns it ready to drive. The stack must publish `ClusterArn`:
// the harness restores the task storage the emulator's CloudFormation drops, and
// without the cluster it cannot find the services to restore it on.
func DeployLocal(t *testing.T, env SandboxEnv, scenario string, build func(awscdk.Stack)) LocalStack {
	t.Helper()
	if localState == nil {
		t.Fatal("DeployLocal requires the local sandbox; call RequireSandbox first")
	}
	seedLocalParameters(t)
	clearProfileLogGroups(t)

	app := NewApp(t, env)
	stackName := StackName(env, scenario)
	stack := awscdk.NewStack(app, jsii.String(stackName), &awscdk.StackProps{Env: StackEnv(env)})
	build(stack)
	ApplyDestroyAspect(stack)

	outputs := DeployStack(t, app, env, stackName)
	if strings.TrimSpace(outputs["ClusterArn"]) == "" {
		t.Fatalf("stack %s published no ClusterArn, so its services cannot be addressed", stackName)
	}
	return LocalStack{
		Outputs:    outputs,
		Env:        env,
		StackName:  stackName,
		AssemblyD:  *app.Outdir(),
		AdminKey:   localAdminKey,
		clusterARN: outputs["ClusterArn"],
		ecs:        ecs.NewFromConfig(localAWSConfig(t)),
		backend:    localState,
	}
}

// clearProfileLogGroups removes the log groups this profile names deterministically.
//
// The facade derives its log-group names from the construct id, not from the
// stack, so two deployments of the same facade in one account and region collide
// on them — the second one's CREATE fails with "already exists". That is a real
// property of the profile, not an emulation gap, and it is why this suite
// deploys one topology at a time. What IS an emulation gap is that a destroyed
// stack can leave its log group behind, so a later deployment collides with a
// stack that no longer exists; this clears that before each deploy rather than
// letting one broken destroy cascade into every test after it.
func clearProfileLogGroups(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := cloudwatchlogs.NewFromConfig(localAWSConfig(t))
	listed, err := client.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String("/gobridge/"),
	})
	if err != nil {
		t.Logf("cannot list the profile's log groups before deploying: %v", err)
		return
	}
	for _, group := range listed.LogGroups {
		name := aws.ToString(group.LogGroupName)
		if _, err := client.DeleteLogGroup(ctx, &cloudwatchlogs.DeleteLogGroupInput{
			LogGroupName: aws.String(name),
		}); err != nil {
			t.Logf("cannot remove the leftover log group %s: %v", name, err)
		}
	}
}

// Call issues one HTTP request from inside the deployment network. Every call to
// a deployed member goes through here: the emulator's ECS reports no ENI, and on
// a Docker-for-Mac host the container network is not routable from the test
// process.
func (s LocalStack) Call(
	ctx context.Context,
	method, url string,
	header map[string]string,
	body []byte,
) (int, []byte, error) {
	return proberCall(ctx, s.backend.prober, method, url, header, body)
}

// RuntimeHosts returns the address of every RUNNING runtime container of the
// named service, inside the deployment network.
//
// A task runs more than one container — the seeder writes the shared config and
// exits — so the runtime is picked by name rather than by "whatever answered
// first": a seeder that is still running would otherwise be handed back as the
// member, and every call to it would fail for a reason that names nothing.
func (s LocalStack) RuntimeHosts(ctx context.Context, service string) ([]string, error) {
	tasks, err := s.serviceTasks(ctx, service)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		taskID := task.arn[strings.LastIndex(task.arn, "/")+1:]
		for name, container := range taskContainers(taskID) {
			if !isRuntimeContainer(name) {
				continue
			}
			if host := containerIP(s.backend.network, container); host != "" {
				hosts = append(hosts, host)
			}
		}
	}
	sort.Strings(hosts)
	return hosts, nil
}

// isRuntimeContainer reports whether an ECS container name is the bridge runtime
// rather than one of the deployment's init containers. The facades name the
// runtime container after the bridge and the init container after what it seeds,
// so the seeder is excluded by name and everything else is the runtime.
func isRuntimeContainer(name string) bool {
	return !strings.Contains(strings.ToLower(name), "seed")
}

// WaitServiceReady waits until the named service is running exactly want
// containers and every one of them answers its monitor readiness endpoint with
// 200, which is the point from which they are pumping messages. It returns their
// addresses.
//
// The addresses are re-resolved on every poll, deliberately. The harness rolls
// each service onto a restored task definition right after the deploy, so the
// task that exists when the wait begins is frequently NOT the task that ends up
// serving: a wait pinned to one address polls a container that has already been
// replaced, and burns its whole budget on a member that is long gone.
//
// A service that never gets there has its containers' logs printed. Without them
// the failure is a status code and nothing else, and the cause — a config the
// seeder never wrote, a transport that cannot reach its backend, a store that
// refused — is only ever in the container.
func (s LocalStack) WaitServiceReady(
	t *testing.T,
	ctx context.Context,
	service string,
	want int,
	timeout time.Duration,
) []string {
	t.Helper()
	var ready []string
	var lastStatus int
	var lastBody []byte
	err := pollUntil(ctx, 3*time.Second, timeout, func() (bool, error) {
		hosts, err := s.RuntimeHosts(ctx, service)
		if err != nil || len(hosts) != want {
			return false, nil
		}
		ready = ready[:0]
		for _, host := range hosts {
			status, body, err := s.Call(ctx, "GET", slotURL(host, slotMonitorPort, monitorReadyPath), nil, nil)
			if err != nil {
				return false, nil
			}
			lastStatus, lastBody = status, body
			if status != 200 {
				// A member that came up carrying nothing AND has an apply error is
				// not slow, it is finished: its configuration was refused and it
				// will never become ready. Waiting out the budget would report a
				// timeout where the member has already said what is wrong.
				// The refusal surfaces as an apply error when the runtime rejected
				// the document, and as a degraded reason when the config manager
				// did — both mean the member is carrying nothing and will not
				// start carrying anything on its own.
				if health, herr := s.DeepHealth(ctx, host); herr == nil && health.Empty {
					if reason := health.ConfigWatch.LastApplyError; reason != "" {
						return false, fmt.Errorf("the deployed configuration was refused: %s", reason)
					}
					if health.ConfigWatch.Degraded && health.ConfigWatch.Reason != "" {
						return false, fmt.Errorf("the deployed configuration was refused: %s",
							health.ConfigWatch.Reason)
					}
				}
				return false, nil
			}
			ready = append(ready, host)
		}
		return true, nil
	})
	if err != nil {
		s.LogContainers(t, ctx)
		t.Fatalf("service %s never had %d ready containers (last status %d): %v: %s",
			service, want, lastStatus, err, truncateBody(lastBody))
	}
	return ready
}

// LogContainers prints the logs of every container the deployment is currently
// running, so a failure that only the workload can explain says why.
func (s LocalStack) LogContainers(t *testing.T, ctx context.Context) {
	t.Helper()
	listed, err := s.ecs.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster: aws.String(s.clusterARN), DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		t.Logf("cannot list the deployment's tasks to read their logs: %v", err)
		return
	}
	for _, arn := range listed.TaskArns {
		taskID := arn[strings.LastIndex(arn, "/")+1:]
		for name, container := range taskContainers(taskID) {
			out, _ := dockerexec.Run(dockerexec.LogsTimeout, "logs", "--tail", "60", container)
			t.Logf("--- %s (%s) ---\n%s", name, container, out)
		}
	}
}

// ScaleService sets a service's desired count and waits for it to settle there.
func (s LocalStack) ScaleService(t *testing.T, ctx context.Context, service string, desired int32) {
	t.Helper()
	if _, err := s.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster: aws.String(s.clusterARN), Service: aws.String(service),
		DesiredCount: aws.Int32(desired),
	}); err != nil {
		t.Fatalf("scale service %s to %d: %v", service, desired, err)
	}
	err := pollUntil(ctx, 2*time.Second, 5*time.Minute, func() (bool, error) {
		tasks, err := s.serviceTasks(ctx, service)
		if err != nil {
			return false, nil
		}
		return len(tasks) == int(desired), nil
	})
	if err != nil {
		t.Fatalf("service %s never settled on %d running tasks: %v", service, desired, err)
	}
}

// StopOneTask stops one running task of the named service and waits until the
// service has replaced it. It returns the ARN of the task it stopped, so a
// caller can assert the replacement really is a different task.
func (s LocalStack) StopOneTask(t *testing.T, ctx context.Context, service, reason string) string {
	t.Helper()
	victim, want := s.StopOneTaskNow(t, ctx, service, reason)
	err := pollUntil(ctx, 2*time.Second, 5*time.Minute, func() (bool, error) {
		current, err := s.serviceTasks(ctx, service)
		if err != nil {
			return false, nil
		}
		for _, task := range current {
			if task.arn == victim {
				return false, nil
			}
		}
		return len(current) == want, nil
	})
	if err != nil {
		t.Fatalf("service %s never replaced the task it lost: %v", service, err)
	}
	return victim
}

// StopOneTaskNow stops one running task and returns immediately, with the ARN it
// stopped and how many tasks the service had.
//
// It exists for the proof that work arriving DURING a restart is not lost: that
// one has to keep sending while the task is going away, so it cannot wait for
// the replacement first.
func (s LocalStack) StopOneTaskNow(
	t *testing.T,
	ctx context.Context,
	service, reason string,
) (string, int) {
	t.Helper()
	tasks, err := s.serviceTasks(ctx, service)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("service %s has no running task to stop: %v", service, err)
	}
	victim := tasks[0].arn
	if _, err := s.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(s.clusterARN), Task: aws.String(victim), Reason: aws.String(reason),
	}); err != nil {
		t.Fatalf("stop task %s of service %s: %v", victim, service, err)
	}
	return victim, len(tasks)
}

// TaskRoleARN is the IAM role the deployed tasks of the named service run as.
func (s LocalStack) TaskRoleARN(t *testing.T, ctx context.Context, service string) string {
	t.Helper()
	described, err := s.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster: aws.String(s.clusterARN), Services: []string{service},
	})
	if err != nil || len(described.Services) != 1 {
		t.Fatalf("describe service %s: %v", service, err)
	}
	definition, err := s.ecs.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: described.Services[0].TaskDefinition,
	})
	if err != nil {
		t.Fatalf("describe the task definition of %s: %v", service, err)
	}
	role := aws.ToString(definition.TaskDefinition.TaskRoleArn)
	if role == "" {
		t.Fatalf("service %s runs with no task role, so nothing constrains what it may call", service)
	}
	return role
}

// serviceTasks lists the running tasks of one service.
func (s LocalStack) serviceTasks(ctx context.Context, service string) ([]localTask, error) {
	listed, err := s.ecs.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster: aws.String(s.clusterARN), ServiceName: aws.String(service),
		DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("ListTasks %s: %w", service, err)
	}
	if len(listed.TaskArns) == 0 {
		return nil, nil
	}
	described, err := s.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(s.clusterARN), Tasks: listed.TaskArns,
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTasks %s: %w", service, err)
	}
	out := make([]localTask, 0, len(described.Tasks))
	for _, task := range described.Tasks {
		if aws.ToString(task.LastStatus) != "RUNNING" {
			continue
		}
		out = append(out, localTask{
			arn:     aws.ToString(task.TaskArn),
			service: strings.TrimPrefix(aws.ToString(task.Group), "service:"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].arn < out[j].arn })
	return out, nil
}

// truncateBody keeps a failure message readable when a member answers with a
// full deep-health document.
func truncateBody(body []byte) string {
	const limit = 512
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…"
}
