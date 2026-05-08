//go:build integration_aws
// +build integration_aws

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Test_T21_Integration_Cluster_ScaleAndKill deploys a GoBridgeCluster
// with WorkerDesiredCount=2, scales the worker service to 3, kills
// one task, and asserts the service self-heals back to 3 RUNNING
// while ALB /healthz remains 200 throughout.
func Test_T21_Integration_Cluster_ScaleAndKill(t *testing.T) {
	env := RequireSandbox(t)

	app := NewApp(t, env)
	stackName := StackName(env, "cluster")
	stack := awscdk.NewStack(app, jsii.String(stackName), &awscdk.StackProps{
		Env: StackEnv(env),
	})
	_ = newClusterFixture(stack, env)
	ApplyDestroyAspect(stack)

	outputs := DeployStack(t, app, env, stackName)
	healthz := outputs["HealthzURL"]
	clusterArn := outputs["ClusterArn"]
	workerSvc := outputs["WorkerServiceName"]
	if healthz == "" || clusterArn == "" || workerSvc == "" {
		t.Fatalf("missing outputs: %v", outputs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	ecsClient := ecs.NewFromConfig(awsCfg)

	healthzCheck := func() error {
		status, _, err := httpGet(ctx, healthz)
		if err != nil {
			return err
		}
		if status != 200 {
			return errFromStatus(status)
		}
		return nil
	}

	// 1. Initial healthz wait.
	if err := pollUntil(ctx, 15*time.Second, 12*time.Minute, func() (bool, error) {
		return healthzCheck() == nil, nil
	}); err != nil {
		t.Fatalf("initial healthz never became 200: %v", err)
	}

	// 2. Scale worker service to 3.
	desired := int32(3)
	if _, err := ecsClient.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:      &clusterArn,
		Service:      &workerSvc,
		DesiredCount: &desired,
	}); err != nil {
		t.Fatalf("UpdateService scale=3: %v", err)
	}
	if err := pollUntil(ctx, 10*time.Second, 5*time.Minute, func() (bool, error) {
		out, err := ecsClient.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  &clusterArn,
			Services: []string{workerSvc},
		})
		if err != nil {
			return false, err
		}
		if len(out.Services) == 0 {
			return false, nil
		}
		t.Logf("worker running=%d desired=%d", out.Services[0].RunningCount, out.Services[0].DesiredCount)
		return out.Services[0].RunningCount == 3, nil
	}); err != nil {
		t.Fatalf("worker did not reach RunningCount=3: %v", err)
	}
	if err := healthzCheck(); err != nil {
		t.Fatalf("healthz failed after scale-up: %v", err)
	}

	// 3. Stop one running worker task; expect self-heal back to 3.
	tasks, err := ecsClient.ListTasks(ctx, &ecs.ListTasksInput{
		Cluster:       &clusterArn,
		ServiceName:   &workerSvc,
		DesiredStatus: ecstypes.DesiredStatusRunning,
	})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks.TaskArns) == 0 {
		t.Fatalf("no running worker tasks to kill")
	}
	victim := tasks.TaskArns[0]
	reason := "T21-integration-kill"
	if _, err := ecsClient.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: &clusterArn,
		Task:    &victim,
		Reason:  &reason,
	}); err != nil {
		t.Fatalf("StopTask %s: %v", victim, err)
	}
	t.Logf("stopped worker task %s", victim)

	if err := pollUntil(ctx, 10*time.Second, 5*time.Minute, func() (bool, error) {
		out, err := ecsClient.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster:  &clusterArn,
			Services: []string{workerSvc},
		})
		if err != nil {
			return false, err
		}
		if len(out.Services) == 0 {
			return false, nil
		}
		t.Logf("post-kill running=%d desired=%d", out.Services[0].RunningCount, out.Services[0].DesiredCount)
		return out.Services[0].RunningCount == 3, nil
	}); err != nil {
		t.Fatalf("worker did not self-heal back to 3: %v", err)
	}
	if err := healthzCheck(); err != nil {
		t.Fatalf("healthz failed after kill+heal: %v", err)
	}
}

func errFromStatus(code int) error { return fmt.Errorf("unexpected http status %d", code) }
