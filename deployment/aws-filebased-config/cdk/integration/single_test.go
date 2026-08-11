//go:build integration_aws
// +build integration_aws

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"
)

// TestIntegration_Single_Healthz deploys a GoBridgeSingle +
// GoBridgeALBAttachment stack against the sandbox account and waits
// for the ALB-fronted health endpoint (HealthzURL) to return 200, then probes
// the admin /status endpoint and asserts a parseable JSON object.
func TestIntegration_Single_Healthz(t *testing.T) {
	env := RequireSandbox(t)

	app := NewApp(t, env)
	stackName := StackName(env, "single")
	stack := awscdk.NewStack(app, jsii.String(stackName), &awscdk.StackProps{
		Env: StackEnv(env),
	})
	_ = newSingleFixture(stack, env)
	ApplyDestroyAspect(stack)

	outputs := DeployStack(t, app, env, stackName)

	healthz := outputs["HealthzURL"]
	admin := outputs["AdminURL"]
	if healthz == "" || admin == "" {
		t.Fatalf("expected HealthzURL+AdminURL outputs, got %v", outputs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	if err := pollUntil(ctx, 15*time.Second, 10*time.Minute, func() (bool, error) {
		status, _, err := httpGet(ctx, healthz)
		if err != nil {
			t.Logf("healthz probe error (will retry): %v", err)
			return false, nil
		}
		t.Logf("healthz status=%d", status)
		return status == 200, nil
	}); err != nil {
		t.Fatalf("healthz never became 200: %v", err)
	}

	statusURL := strings.TrimRight(admin, "/") + "/status"
	status, body, err := httpGet(ctx, statusURL)
	if err != nil {
		t.Fatalf("GET %s: %v", statusURL, err)
	}
	if status != 200 {
		t.Fatalf("GET %s: status=%d body=%s", statusURL, status, string(body))
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("status body not JSON object: %v body=%s", err, string(body))
	}
	if len(obj) == 0 {
		t.Fatalf("status JSON object is empty")
	}
}
