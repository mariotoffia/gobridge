//go:build integration_aws
// +build integration_aws

package integration

import (
	"context"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/jsii-runtime-go"
)

// The deployed proof of the static member-slot profile.
//
// Everything below the deployment is already unit-proven: the barrier protocol in
// package bridge, the App's host wiring in lib/bootstrap, and the synthesized
// stack in the construct tests. What ONLY a deployment can prove is the part the
// unit tests have to assume — that an ECS slot's member_id really does survive the
// task being replaced, so a restarted member rejoins the cohort it left instead of
// arriving as a stranger the barrier counts against nobody.
//
// So the run is: propose a live-safe change and watch every slot converge, kill a
// slot's task and watch its replacement come back under the SAME identity on the
// COMMITTED generation (not the config document that happens to be on EFS), then
// roll the change back the same way.

func requireStaticSlotRolloutSandbox(t *testing.T) haSandbox {
	t.Helper()
	if os.Getenv("GOBRIDGE_INT_HA_ROLLOUT") != "1" {
		t.Skip("deployed static-slot rollout proof not requested; set GOBRIDGE_INT_HA_ROLLOUT=1 " +
			"with the documented GOBRIDGE_INT_HA_* sandbox variables")
	}
	t.Setenv("GOBRIDGE_INT_HA", "1")
	return requireHAFailoverSandbox(t)
}

// credentialedProbe reaches the cohort the way an operator inside the sandbox VPC
// does: each task's own ENI address, over plain HTTP.
func credentialedProbe(fixture haRuntimeFixture) cohortProbe {
	return cohortProbe{
		Members: func(ctx context.Context) ([]cohortMember, error) {
			tasks, err := fixture.runningTasks(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]cohortMember, 0, len(tasks))
			for _, task := range tasks {
				if task.lastStatus != "RUNNING" {
					continue
				}
				out = append(out, cohortMember{TaskARN: task.arn, Service: task.service, Host: task.privateIP})
			}
			return out, nil
		},
		Call: func(
			ctx context.Context,
			method, url string,
			header map[string]string,
			body []byte,
		) (int, []byte, error) {
			return httpCall(ctx, http.DefaultClient, method, url, header, body)
		},
	}
}

func TestIntegration_HA_StaticSlotCoordinatedRolloutSurvivesRestart(t *testing.T) {
	env := requireStaticSlotRolloutSandbox(t)
	app := NewApp(t, env.SandboxEnv)
	stackName := StackName(env.SandboxEnv, "dynamodb-ha-slots")
	stack := awscdk.NewStack(app, jsii.String(stackName), &awscdk.StackProps{Env: StackEnv(env.SandboxEnv)})
	_ = newHAFixture(t, stack, env, staticSlotRoster())
	ApplyDestroyAspect(stack)

	outputs := DeployStack(t, app, env.SandboxEnv, stackName)
	if err := missingHAOutput(outputs,
		"ClusterArn", "ControlServiceName", "WorkerServiceNames", "MemberSlotIDs", "RolloutTableName"); err != nil {
		t.Fatal(err)
	}

	// Budget: the phase waits below sum to just over an hour in the worst case
	// (roster 15 + rollout 15 + stop 5 + rejoin 12 + rollback 15). The parent must
	// exceed that, or a slow-but-correct run dies inside an unrelated poll.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		t.Fatalf("load credentialed AWS config: %v", err)
	}
	fixture := haRuntimeFixture{
		clients:        haAWSClients{ecs: ecs.NewFromConfig(awsCfg)},
		clusterARN:     outputs["ClusterArn"],
		controlService: outputs["ControlServiceName"],
	}
	probe := credentialedProbe(fixture)
	adminKey := readSSMSecret(t, ctx, ssm.NewFromConfig(awsCfg), env.AdminParam)

	roster := strings.Split(outputs["MemberSlotIDs"], ",")
	sort.Strings(roster)
	want := []string{staticSlotControlID, staticSlotWorkerA, staticSlotWorkerB}
	sort.Strings(want)
	if strings.Join(roster, ",") != strings.Join(want, ",") {
		t.Fatalf("deployed roster = %v, want %v", roster, want)
	}

	// Every slot must be up, distinct, and already carrying the deployment's
	// generation-zero baseline: without it a restart in the window before the
	// first rollout would boot whatever EFS currently holds.
	slots := waitForEverySlot(t, ctx, probe, adminKey, roster)
	// The baseline is seeded AT generation zero, so the generation number cannot
	// tell "seeded" from "never seeded". The digest can: rolloutHealth fills both
	// fields only when the member verified a baseline, and leaves the digest empty
	// otherwise.
	baselines := map[string]struct{}{}
	for id, health := range slots {
		if health.BaselineDigest == "" {
			t.Fatalf("slot %s reports no committed baseline; a restart before the first rollout would not "+
				"recover to the config this deployment admitted", id)
		}
		baselines[health.BaselineDigest] = struct{}{}
	}
	if len(baselines) != 1 {
		t.Fatalf("slots recovered to %d different baselines, want one cohort artifact: %v",
			len(baselines), baselines)
	}

	controlIP, err := memberHost(ctx, probe, adminKey, staticSlotControlID)
	if err != nil {
		t.Fatal(err)
	}
	baseGeneration := slots[staticSlotControlID].Generation

	// 1. Propose a live-safe change and watch the whole cohort converge on it.
	commitLogLevel(t, ctx, probe, controlIP, adminKey, "debug")
	rolled := waitCohortApplied(t, ctx, probe, adminKey, roster, baseGeneration)
	t.Logf("cohort converged on generation %d", rolled)

	// 2. Replace one worker slot's task. Its member_id is the deployment's, not
	//    the task's, so the replacement must rejoin the SAME cohort seat — and its
	//    boot resolution must hand it the committed generation.
	victim := slots[staticSlotWorkerA]
	victimARN, err := memberTaskARN(ctx, probe, adminKey, staticSlotWorkerA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.clients.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(fixture.clusterARN),
		Task:    aws.String(victimARN),
		Reason:  aws.String("static member-slot rollout proof: restart-stability of member_id"),
	}); err != nil {
		t.Fatalf("stop slot %s task: %v", victim.MemberID, err)
	}
	fixture.waitTaskStopped(t, ctx, victimARN, 5*time.Minute)

	restarted := waitSlotAtGeneration(t, ctx, probe, adminKey, staticSlotWorkerA, rolled, 12*time.Minute)
	// Re-check identity uniqueness AFTER the replacement: this is exactly when a
	// non-stable identity would show up, as a fourth id or as a collapsed seat.
	waitForEverySlot(t, ctx, probe, adminKey, roster)
	if restarted.MemberID != staticSlotWorkerA {
		t.Fatalf("restarted slot announced member_id %q, want %q: the seat is not restart-stable",
			restarted.MemberID, staticSlotWorkerA)
	}
	if !restarted.Applied {
		t.Fatalf("restarted slot %s is at generation %d but has not applied it (%s)",
			staticSlotWorkerA, restarted.Generation, restarted.TerminalReason)
	}

	// 3. Roll the change back the same way, and require the whole cohort — the
	//    restarted slot included — to converge on the rollback.
	commitLogLevel(t, ctx, probe, controlIP, adminKey, "info")
	rolledBack := waitCohortApplied(t, ctx, probe, adminKey, roster, rolled)
	t.Logf("cohort converged on rollback generation %d", rolledBack)
}

func readSSMSecret(t *testing.T, ctx context.Context, client *ssm.Client, name string) string {
	t.Helper()
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(name), WithDecryption: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("read admin key parameter %s: %v", name, err)
	}
	return aws.ToString(out.Parameter.Value)
}

// httpCall issues one request and returns (status, body). A transport failure is
// an error; an HTTP status is not.
func httpCall(
	ctx context.Context,
	client *http.Client,
	method, url string,
	header map[string]string,
	body []byte,
) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}
