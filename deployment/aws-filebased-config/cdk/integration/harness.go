//go:build integration_aws || integration_local
// +build integration_aws integration_local

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

// SandboxEnv captures the resolved environment used to build a CDK
// app that targets a real AWS sandbox account.
type SandboxEnv struct {
	Account           string
	Region            string
	VpcID             string
	AvailabilityZones []string
	SubnetIDs         []string
	PublicSubnetIDs   []string
	StackPrefix       string
	Keep              bool
}

// localSandboxHook synthesizes a SandboxEnv against local emulation. Only the
// integration_local build installs it; under integration_aws it stays nil and
// the branch below cannot be taken, so "real account, real money" keeps meaning
// exactly that.
var localSandboxHook func(t *testing.T) SandboxEnv

// cdkBinaryName is the CLI that drives deploy and destroy. The local build
// swaps it for the emulator-aware wrapper; the outputs-file contract, the
// argument list and the cleanup are identical either way.
var cdkBinaryName = "cdk"

// postSynthHook rewrites the synthesized cloud assembly before it is deployed,
// and postDeployHook runs once the stack is up. Both are local-only: the
// emulator needs the task definitions adjusted for what it can actually back,
// and the DynamoDB data plane needs the deployed schema mirrored to it.
var (
	postSynthHook  func(t *testing.T, asmDir, stackName string)
	postDeployHook func(t *testing.T, outputs StackOutputs)
)

// RequireSandbox reads the GOBRIDGE_INT_* env vars. Ordinary tagged tests
// retain the existing explicit skip when sandbox configuration is absent. When
// GOBRIDGE_INT_HA=1 requests credentialed release proof, missing base variables
// fail instead, so the requested proof cannot silently pass by skipping.
//
// GOBRIDGE_INT_LOCAL=1 takes the local branch instead: the sandbox is stood up
// against local emulation, so nothing is read from the environment and nothing
// is skipped.
func RequireSandbox(t *testing.T) SandboxEnv {
	t.Helper()
	if localSandboxHook != nil && os.Getenv("GOBRIDGE_INT_LOCAL") == "1" {
		return localSandboxHook(t)
	}
	env, err := sandboxEnvFrom(os.Getenv)
	if err != nil {
		if os.Getenv("GOBRIDGE_INT_HA") == "1" {
			t.Fatalf("GOBRIDGE_INT_HA=1 requested credentialed proof but sandbox configuration is invalid: %v", err)
		}
		t.Skipf("integration sandbox env not configured: %v", err)
	}
	return env
}

func sandboxEnvFrom(getenv func(string) string) (SandboxEnv, error) {
	requiredNames := []string{
		"GOBRIDGE_INT_AWS_ACCOUNT", "GOBRIDGE_INT_AWS_REGION", "GOBRIDGE_INT_VPC_ID",
		"GOBRIDGE_INT_AVAILABILITY_ZONES", "GOBRIDGE_INT_SUBNET_IDS", "GOBRIDGE_INT_PUBLIC_SUBNET_IDS",
	}
	values := make(map[string]string, len(requiredNames))
	missing := make([]string, 0)
	for _, name := range requiredNames {
		values[name] = strings.TrimSpace(getenv(name))
		if values[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return SandboxEnv{}, fmt.Errorf("missing: %s", strings.Join(missing, ", "))
	}
	parseList := func(name string) ([]string, error) {
		parts := strings.Split(values[name], ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
			if parts[i] == "" {
				return nil, fmt.Errorf("%s contains an empty item", name)
			}
		}
		return parts, nil
	}
	zones, err := parseList("GOBRIDGE_INT_AVAILABILITY_ZONES")
	if err != nil {
		return SandboxEnv{}, err
	}
	privateSubnets, err := parseList("GOBRIDGE_INT_SUBNET_IDS")
	if err != nil {
		return SandboxEnv{}, err
	}
	publicSubnets, err := parseList("GOBRIDGE_INT_PUBLIC_SUBNET_IDS")
	if err != nil {
		return SandboxEnv{}, err
	}
	if len(zones) != len(privateSubnets) || len(zones) != len(publicSubnets) {
		return SandboxEnv{}, fmt.Errorf("GOBRIDGE_INT_AVAILABILITY_ZONES, GOBRIDGE_INT_SUBNET_IDS, and GOBRIDGE_INT_PUBLIC_SUBNET_IDS must contain the same number of ordered items")
	}
	prefix := strings.TrimSpace(getenv("GOBRIDGE_INT_STACK_PREFIX"))
	if prefix == "" {
		prefix = "gobridge-it"
	}
	return SandboxEnv{
		Account: values["GOBRIDGE_INT_AWS_ACCOUNT"], Region: values["GOBRIDGE_INT_AWS_REGION"],
		VpcID: values["GOBRIDGE_INT_VPC_ID"], AvailabilityZones: zones,
		SubnetIDs: privateSubnets, PublicSubnetIDs: publicSubnets,
		StackPrefix: prefix, Keep: strings.TrimSpace(getenv("GOBRIDGE_INT_KEEP")) == "1",
	}, nil
}

// NewApp returns a CDK App configured with the sandbox env so
// stacks created under it inherit Account/Region without further wiring.
// VPC placement uses explicit attributes and needs no context provider.
func NewApp(t *testing.T, env SandboxEnv) awscdk.App {
	t.Helper()
	return awscdk.NewApp(&awscdk.AppProps{
		Context: &map[string]any{
			"@aws-cdk/core:bootstrapQualifier": "hnb659fds",
		},
	})
}

// StackEnv builds the CDK env struct used on every stack so
// Explicit imported VPC attributes are bound to this same environment.
func StackEnv(env SandboxEnv) *awscdk.Environment {
	return &awscdk.Environment{
		Account: jsii.String(env.Account),
		Region:  jsii.String(env.Region),
	}
}

// StackName composes a deterministic-but-unique stack name from the
// sandbox prefix, scenario tag and a unix-second timestamp suffix.
func StackName(env SandboxEnv, scenario string) string {
	return fmt.Sprintf("%s-%s-%d", env.StackPrefix, scenario, time.Now().Unix())
}

// ApplyDestroyAspect walks the stack's construct tree and flips
// every CfnResource to RemovalPolicy.DESTROY so teardown does not
// strand retained EFS / log groups in the sandbox account. Done
// imperatively (rather than via awscdk.Aspects) to sidestep the
// jsii callback registration required by IAspect.
func ApplyDestroyAspect(stack awscdk.Stack) {
	walkConstructs(stack.Node(), func(node constructs.IConstruct) {
		if table, ok := node.(awsdynamodb.CfnTable); ok {
			table.SetDeletionProtectionEnabled(false)
		}
		if cfn, ok := node.(awscdk.CfnResource); ok {
			cfn.ApplyRemovalPolicy(awscdk.RemovalPolicy_DESTROY, nil)
		}
	})
}

func walkConstructs(node constructs.Node, fn func(constructs.IConstruct)) {
	for _, child := range *node.Children() {
		fn(child)
		walkConstructs(child.Node(), fn)
	}
}

// StackOutputs is the parsed shape of `cdk deploy --outputs-file`.
// Top-level key is the stack name, inner map is logical-id → value.
type StackOutputs map[string]string

// DeployStack synthesises the supplied app to a temp cloud-assembly
// directory, runs `cdk deploy` against the named stack and returns
// its outputs. A `t.Cleanup` hook is registered to destroy the stack
// (honouring GOBRIDGE_INT_KEEP).
func DeployStack(t *testing.T, app awscdk.App, env SandboxEnv, stackName string) StackOutputs {
	t.Helper()

	app.Synth(&awscdk.StageSynthesisOptions{
		Force: jsii.Bool(true),
	})
	// The app's default cloud assembly is at app.Outdir(); copy/move
	// is unnecessary — we just point cdk at it.
	asmDir := *app.Outdir()
	if postSynthHook != nil {
		postSynthHook(t, asmDir, stackName)
	}

	flat := cdkDeploy(t, stackName, asmDir)

	t.Cleanup(func() {
		if env.Keep {
			t.Logf("GOBRIDGE_INT_KEEP=1 → leaving stack %s in place", stackName)
			return
		}
		DestroyStack(t, env, stackName, asmDir)
	})

	if postDeployHook != nil {
		postDeployHook(t, flat)
	}
	return flat
}

// cdkDeploy runs one `cdk deploy` against an already-synthesized cloud assembly
// and returns the stack outputs it wrote.
//
// It registers no cleanup and runs no hook, so a caller that deploys the SAME
// assembly a second time — to prove the deploy is idempotent — gets exactly the
// deploy and nothing else.
func cdkDeploy(t *testing.T, stackName, asmDir string) StackOutputs {
	t.Helper()
	outputs, err := cdkDeployE(t, stackName, asmDir)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return outputs
}

// cdkDeployE is cdkDeploy without the fatal: a caller that treats a failed
// deploy as an answer rather than an error gets to say so itself.
func cdkDeployE(t *testing.T, stackName, asmDir string) (StackOutputs, error) {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "outputs.json")
	args := []string{
		"deploy",
		stackName,
		"--app", asmDir,
		"--require-approval", "never",
		"--outputs-file", outFile,
		"--ci",
	}
	t.Logf("%s %s", cdkBinaryName, strings.Join(args, " "))
	cmd := exec.Command(cdkBinaryName, args...)
	var captured strings.Builder
	cmd.Stdout = io.MultiWriter(testWriter{t}, &captured)
	cmd.Stderr = io.MultiWriter(testWriter{t}, &captured)
	if err := cmd.Run(); err != nil {
		// The tail, not the whole transcript: every line is already in the test
		// log, and what a caller needs from the error is the reason CloudFormation
		// gave — which is at the end.
		return nil, fmt.Errorf("%s deploy %s: %w\n%s",
			cdkBinaryName, stackName, err, lastBytes(captured.String(), 4000))
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("read outputs file: %w", err)
	}
	var nested map[string]map[string]string
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, fmt.Errorf("parse outputs json: %w", err)
	}
	flat := StackOutputs{}
	for _, m := range nested {
		for k, v := range m {
			flat[k] = v
		}
	}
	return flat, nil
}

// DestroyStack runs `cdk destroy --force` against the supplied stack.
// Errors are logged, never failed: teardown best-effort so a failed
// destroy never masks the real test failure.
func DestroyStack(t *testing.T, env SandboxEnv, stackName, asmDir string) {
	t.Helper()
	if env.Keep {
		return
	}
	args := []string{"destroy", stackName, "--app", asmDir, "--force", "--ci"}
	t.Logf("%s %s", cdkBinaryName, strings.Join(args, " "))
	cmd := exec.Command(cdkBinaryName, args...)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Logf("%s destroy %s failed: %v", cdkBinaryName, stackName, err)
	}
}

// lastBytes returns the final n bytes of s, marked when it was truncated.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}

// httpGet issues a GET with a 30s default timeout and returns
// (status, body, error). Caller controls the parent context.
func httpGet(ctx context.Context, url string) (int, []byte, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// pollUntil invokes fn every interval until it returns (true, nil)
// or maxWait elapses. A non-nil error from fn aborts the loop.
func pollUntil(ctx context.Context, interval, maxWait time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(maxWait)
	for {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pollUntil: condition not met within %s", maxWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// testWriter forwards subprocess output into the test log so failure
// triage doesn't require digging through stderr.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
