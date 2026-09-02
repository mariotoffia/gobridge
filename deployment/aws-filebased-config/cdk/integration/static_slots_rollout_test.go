//go:build integration_aws
// +build integration_aws

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
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

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
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

const (
	staticSlotControlID = "gobridge-ha-control"
	staticSlotWorkerA   = "gobridge-ha-worker-1"
	staticSlotWorkerB   = "gobridge-ha-worker-2"
)

func staticSlotRoster() *ha.MemberSlots {
	return &ha.MemberSlots{
		ControlMemberID: staticSlotControlID,
		WorkerMemberIDs: []string{staticSlotWorkerA, staticSlotWorkerB},
	}
}

func requireStaticSlotRolloutSandbox(t *testing.T) haSandbox {
	t.Helper()
	if os.Getenv("GOBRIDGE_INT_HA_ROLLOUT") != "1" {
		t.Skip("deployed static-slot rollout proof not requested; set GOBRIDGE_INT_HA_ROLLOUT=1 " +
			"with the documented GOBRIDGE_INT_HA_* sandbox variables")
	}
	t.Setenv("GOBRIDGE_INT_HA", "1")
	return requireHAFailoverSandbox(t)
}

// slotHealth is the part of one member's /deephealth this proof reads.
type slotHealth struct {
	MemberID           string `json:"member_id"`
	Generation         uint64 `json:"generation"`
	State              string `json:"state"`
	Applied            bool   `json:"applied"`
	ConfirmPending     bool   `json:"confirm_pending"`
	BaselineGeneration uint64 `json:"baseline_generation"`
	BaselineDigest     string `json:"baseline_digest"`
	TerminalReason     string `json:"terminal_reason"`
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
	slots := waitForEverySlot(t, ctx, fixture, adminKey, roster)
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

	controlIP := slotPrivateIP(t, ctx, fixture, adminKey, staticSlotControlID)
	baseGeneration := slots[staticSlotControlID].Generation

	// 1. Propose a live-safe change and watch the whole cohort converge on it.
	commitLogLevel(t, ctx, controlIP, adminKey, "debug")
	rolled := waitCohortApplied(t, ctx, fixture, adminKey, roster, baseGeneration)
	t.Logf("cohort converged on generation %d", rolled)

	// 2. Replace one worker slot's task. Its member_id is the deployment's, not
	//    the task's, so the replacement must rejoin the SAME cohort seat — and its
	//    boot resolution must hand it the committed generation.
	victim := slots[staticSlotWorkerA]
	victimARN := slotTaskARN(t, ctx, fixture, adminKey, staticSlotWorkerA)
	if _, err := fixture.clients.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(fixture.clusterARN),
		Task:    aws.String(victimARN),
		Reason:  aws.String("static member-slot rollout proof: restart-stability of member_id"),
	}); err != nil {
		t.Fatalf("stop slot %s task: %v", victim.MemberID, err)
	}
	fixture.waitTaskStopped(t, ctx, victimARN, 5*time.Minute)

	restarted := waitSlotAtGeneration(t, ctx, fixture, adminKey, staticSlotWorkerA, rolled, 12*time.Minute)
	// Re-check identity uniqueness AFTER the replacement: this is exactly when a
	// non-stable identity would show up, as a fourth id or as a collapsed seat.
	waitForEverySlot(t, ctx, fixture, adminKey, roster)
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
	commitLogLevel(t, ctx, controlIP, adminKey, "info")
	rolledBack := waitCohortApplied(t, ctx, fixture, adminKey, roster, rolled)
	t.Logf("cohort converged on rollback generation %d", rolledBack)
}

// commitLogLevel drives one live-safe config change through the admin config
// transaction API on the control slot. log_level is the smallest change the
// barrier classifies live-safe, so the proof is about the protocol rather than
// about what changed.
func commitLogLevel(t *testing.T, ctx context.Context, controlIP, adminKey, level string) {
	t.Helper()
	base := "http://" + net.JoinHostPort(controlIP, "8080") + "/api/v1/admin/config/transactions"

	var created struct {
		TxnID string `json:"txn_id"`
	}
	adminCall(t, ctx, http.MethodPost, base, adminKey, nil, &created)
	if created.TxnID == "" {
		t.Fatal("config transaction create returned no txn_id")
	}
	overlay := map[string]any{"bridge": map[string]any{"log_level": level}}
	adminCall(t, ctx, http.MethodPatch, base+"/"+created.TxnID, adminKey, overlay, nil)

	// A coordinated cohort DEFERS: the commit is proposed to the barrier, so the
	// admin layer reports committed_not_applied rather than a completed swap. Both
	// that and a plain 200 are success here; only a rollback is a failure.
	var committed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	status := adminCallStatus(t, ctx, http.MethodPost, base+"/"+created.TxnID+"/commit", adminKey, nil, &committed)
	if committed.Status == "rolled_back" {
		t.Fatalf("commit rolled back: %s", committed.Error)
	}
	if status >= 400 && committed.Status != "committed_not_applied" {
		t.Fatalf("commit status=%d body status=%q error=%q", status, committed.Status, committed.Error)
	}
}

// waitForEverySlot polls until every roster member answers /deephealth with its
// own member_id, and returns the last observation per slot.
func waitForEverySlot(
	t *testing.T,
	ctx context.Context,
	fixture haRuntimeFixture,
	adminKey string,
	roster []string,
) map[string]slotHealth {
	t.Helper()
	var found map[string]slotHealth
	var answered int
	err := pollUntil(ctx, 10*time.Second, 15*time.Minute, func() (bool, error) {
		found, answered = observeSlots(ctx, fixture, adminKey)
		for _, id := range roster {
			if _, ok := found[id]; !ok {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("not every member slot reported in: observed %v, want %v", keysOf(found), roster)
	}
	if answered != len(found) {
		t.Fatalf("%d running tasks answered but only %d distinct member ids (%v): two tasks share one "+
			"cohort seat, so the roster looks satisfied while a seat is unoccupied", answered, len(found), keysOf(found))
	}
	return found
}

// waitCohortApplied polls until every roster member reports the SAME generation,
// strictly newer than after, with the swap applied and no confirm window still
// open. Returns that generation.
func waitCohortApplied(
	t *testing.T,
	ctx context.Context,
	fixture haRuntimeFixture,
	adminKey string,
	roster []string,
	after uint64,
) uint64 {
	t.Helper()
	var converged uint64
	var last map[string]slotHealth
	err := pollUntil(ctx, 10*time.Second, 15*time.Minute, func() (bool, error) {
		last, _ = observeSlots(ctx, fixture, adminKey)
		generation := uint64(0)
		for _, id := range roster {
			health, ok := last[id]
			if !ok || !health.Applied || health.ConfirmPending || health.Generation <= after {
				return false, nil
			}
			if generation == 0 {
				generation = health.Generation
			} else if health.Generation != generation {
				return false, nil // still split across generations
			}
		}
		converged = generation
		return true, nil
	})
	if err != nil {
		t.Fatalf("cohort did not converge past generation %d: %+v", after, last)
	}
	return converged
}

// waitSlotAtGeneration polls one slot until it reports the given generation.
func waitSlotAtGeneration(
	t *testing.T,
	ctx context.Context,
	fixture haRuntimeFixture,
	adminKey, memberID string,
	generation uint64,
	timeout time.Duration,
) slotHealth {
	t.Helper()
	var observed slotHealth
	err := pollUntil(ctx, 10*time.Second, timeout, func() (bool, error) {
		observedSlots, _ := observeSlots(ctx, fixture, adminKey)
		health, ok := observedSlots[memberID]
		if !ok || health.Generation != generation {
			return false, nil
		}
		observed = health
		return true, nil
	})
	if err != nil {
		t.Fatalf("slot %s did not come back on generation %d", memberID, generation)
	}
	return observed
}

// observeSlots reads /deephealth from every running task and keys the rollout
// block by the member_id that task announces. A task that is not up yet, or has
// no rollout block, is simply absent.
//
// It also returns how many tasks ANSWERED, which the caller compares against the
// number of distinct ids: keying by member_id would otherwise hide the single
// worst deployment defect this profile can have — two running tasks under one
// identity, which makes the roster look satisfied while one seat is unoccupied.
func observeSlots(ctx context.Context, fixture haRuntimeFixture, adminKey string) (map[string]slotHealth, int) {
	out := map[string]slotHealth{}
	answered := 0
	tasks, err := fixture.runningTasks(ctx)
	if err != nil {
		return out, 0
	}
	for _, task := range tasks {
		if task.privateIP == "" || task.lastStatus != "RUNNING" {
			continue
		}
		health, ok := readSlotHealth(ctx, task.privateIP, adminKey)
		if !ok || health.MemberID == "" {
			continue
		}
		answered++
		out[health.MemberID] = health
	}
	return out, answered
}

func readSlotHealth(ctx context.Context, privateIP, adminKey string) (slotHealth, bool) {
	endpoint := "http://" + net.JoinHostPort(privateIP, "8081") + "/api/v1/monitor/deephealth"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return slotHealth{}, false
	}
	req.Header.Set("X-API-Key", adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return slotHealth{}, false
	}
	defer resp.Body.Close()
	// /deephealth answers 503 with the FULL body whenever the member is not
	// ready for traffic — which a member mid-swap, and possibly a warm standby,
	// simply is. Reading only 200 would confuse "not traffic-ready" with "not
	// there", and this proof is reading the rollout block, not readiness.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return slotHealth{}, false
	}
	var body struct {
		ConfigWatch struct {
			Rollout *slotHealth `json:"rollout"`
		} `json:"config_watch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.ConfigWatch.Rollout == nil {
		return slotHealth{}, false
	}
	return *body.ConfigWatch.Rollout, true
}

func slotPrivateIP(
	t *testing.T,
	ctx context.Context,
	fixture haRuntimeFixture,
	adminKey, memberID string,
) string {
	t.Helper()
	tasks, err := fixture.runningTasks(ctx)
	if err != nil {
		t.Fatalf("list running tasks: %v", err)
	}
	for _, task := range tasks {
		if task.privateIP == "" {
			continue
		}
		if health, ok := readSlotHealth(ctx, task.privateIP, adminKey); ok && health.MemberID == memberID {
			return task.privateIP
		}
	}
	t.Fatalf("no running task announces member_id %q", memberID)
	return ""
}

func slotTaskARN(
	t *testing.T,
	ctx context.Context,
	fixture haRuntimeFixture,
	adminKey, memberID string,
) string {
	t.Helper()
	tasks, err := fixture.runningTasks(ctx)
	if err != nil {
		t.Fatalf("list running tasks: %v", err)
	}
	for _, task := range tasks {
		if task.privateIP == "" {
			continue
		}
		if health, ok := readSlotHealth(ctx, task.privateIP, adminKey); ok && health.MemberID == memberID {
			return task.arn
		}
	}
	t.Fatalf("no running task announces member_id %q", memberID)
	return ""
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

func adminCall(t *testing.T, ctx context.Context, method, url, adminKey string, body, out any) {
	t.Helper()
	if status := adminCallStatus(t, ctx, method, url, adminKey, body, out); status >= 400 {
		t.Fatalf("%s %s returned %d", method, url, status)
	}
}

func adminCallStatus(t *testing.T, ctx context.Context, method, url, adminKey string, body, out any) int {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s body: %v", method, err)
		}
		payload = encoded
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("X-API-Key", adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func keysOf(m map[string]slotHealth) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
