//go:build integration_aws
// +build integration_aws

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
	Account     string
	Region      string
	VpcID       string
	SubnetIDs   []string
	StackPrefix string
	Keep        bool
}

// RequireSandbox reads the GOBRIDGE_INT_* env vars. Ordinary tagged tests
// retain the existing explicit skip when sandbox configuration is absent. When
// GOBRIDGE_INT_HA=1 requests credentialed release proof, missing base variables
// fail instead, so the requested proof cannot silently pass by skipping.
func RequireSandbox(t *testing.T) SandboxEnv {
	t.Helper()
	required := map[string]string{
		"GOBRIDGE_INT_AWS_ACCOUNT": os.Getenv("GOBRIDGE_INT_AWS_ACCOUNT"),
		"GOBRIDGE_INT_AWS_REGION":  os.Getenv("GOBRIDGE_INT_AWS_REGION"),
		"GOBRIDGE_INT_VPC_ID":      os.Getenv("GOBRIDGE_INT_VPC_ID"),
		"GOBRIDGE_INT_SUBNET_IDS":  os.Getenv("GOBRIDGE_INT_SUBNET_IDS"),
	}
	var missing []string
	for k, v := range required {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		if os.Getenv("GOBRIDGE_INT_HA") == "1" {
			t.Fatalf("GOBRIDGE_INT_HA=1 requested credentialed proof but sandbox variables are missing: %s", strings.Join(missing, ", "))
		}
		t.Skipf("integration sandbox env not configured; missing: %s", strings.Join(missing, ", "))
	}

	prefix := os.Getenv("GOBRIDGE_INT_STACK_PREFIX")
	if prefix == "" {
		prefix = "gobridge-it"
	}
	subnets := strings.Split(required["GOBRIDGE_INT_SUBNET_IDS"], ",")
	for i := range subnets {
		subnets[i] = strings.TrimSpace(subnets[i])
	}
	return SandboxEnv{
		Account:     required["GOBRIDGE_INT_AWS_ACCOUNT"],
		Region:      required["GOBRIDGE_INT_AWS_REGION"],
		VpcID:       required["GOBRIDGE_INT_VPC_ID"],
		SubnetIDs:   subnets,
		StackPrefix: prefix,
		Keep:        os.Getenv("GOBRIDGE_INT_KEEP") == "1",
	}
}

// NewApp returns a CDK App configured with the sandbox env so
// stacks created under it inherit Account/Region without further
// wiring.
func NewApp(t *testing.T, env SandboxEnv) awscdk.App {
	t.Helper()
	return awscdk.NewApp(&awscdk.AppProps{
		Context: &map[string]any{
			"@aws-cdk/core:bootstrapQualifier": "hnb659fds",
		},
	})
}

// StackEnv builds the CDK env struct used on every stack so
// FromLookup helpers (VPC, etc.) resolve at synth time.
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

	outFile := filepath.Join(t.TempDir(), "outputs.json")

	args := []string{
		"deploy",
		stackName,
		"--app", asmDir,
		"--require-approval", "never",
		"--outputs-file", outFile,
		"--ci",
	}
	t.Logf("cdk %s", strings.Join(args, " "))
	cmd := exec.Command("cdk", args...)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("cdk deploy %s: %v", stackName, err)
	}

	t.Cleanup(func() {
		if env.Keep {
			t.Logf("GOBRIDGE_INT_KEEP=1 → leaving stack %s in place", stackName)
			return
		}
		DestroyStack(t, env, stackName, asmDir)
	})

	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read outputs file: %v", err)
	}
	var nested map[string]map[string]string
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatalf("parse outputs json: %v", err)
	}
	flat := StackOutputs{}
	for _, m := range nested {
		for k, v := range m {
			flat[k] = v
		}
	}
	return flat
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
	t.Logf("cdk %s", strings.Join(args, " "))
	cmd := exec.Command("cdk", args...)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Logf("cdk destroy %s failed: %v", stackName, err)
	}
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
