//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// What the deployed shape is asserted against, and where each fact comes from.
//
// The health check is read out of the SYNTHESIZED template rather than out of
// the emulator: the emulator does not model an ELBv2 health check, so reading it
// back from there would assert on the harness. The template is what
// CloudFormation was given, so a path the profile declares and the runtime does
// not serve is caught exactly where the emulation gap would otherwise hide it.

const targetGroupType = "AWS::ElasticLoadBalancingV2::TargetGroup"

// targetGroupHealthCheck is one target group's declared health check.
type targetGroupHealthCheck struct {
	Group string
	Path  string
	Port  int
	Codes string
}

// targetGroupHealthChecks reads every health check the deployed stack declared.
func targetGroupHealthChecks(t *testing.T, stack LocalStack) []targetGroupHealthCheck {
	t.Helper()
	resources := synthesizedResources(t, stack)
	checks := make([]targetGroupHealthCheck, 0, 2)
	for logicalID, value := range resources {
		resource, _ := value.(map[string]any)
		if resource["Type"] != targetGroupType {
			continue
		}
		properties, _ := resource["Properties"].(map[string]any)
		path, _ := properties["HealthCheckPath"].(string)
		if path == "" {
			t.Fatalf("target group %s declares no health-check path, so ECS would take its tasks out "+
				"of service on the load balancer's default path", logicalID)
		}
		port := 0
		switch raw := properties["HealthCheckPort"].(type) {
		case string:
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				t.Fatalf("target group %s declares an unreadable health-check port %q", logicalID, raw)
			}
			port = parsed
		case float64:
			port = int(raw)
		default:
			t.Fatalf("target group %s declares no literal health-check port, so nothing can check that "+
				"the container answers on it", logicalID)
		}
		codes, _ := properties["Matcher"].(map[string]any)
		httpCode := "200"
		if codes != nil {
			if value, ok := codes["HttpCode"].(string); ok && value != "" {
				httpCode = value
			}
		}
		checks = append(checks, targetGroupHealthCheck{Group: logicalID, Path: path, Port: port, Codes: httpCode})
	}
	return checks
}

// synthesizedResources parses the template the deploy was given.
func synthesizedResources(t *testing.T, stack LocalStack) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stack.AssemblyD, stack.StackName+".template.json"))
	if err != nil {
		t.Fatalf("read the synthesized template of %s: %v", stack.StackName, err)
	}
	var template map[string]any
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatalf("parse the synthesized template of %s: %v", stack.StackName, err)
	}
	resources, _ := template["Resources"].(map[string]any)
	if len(resources) == 0 {
		t.Fatalf("the synthesized template of %s declares no resources", stack.StackName)
	}
	return resources
}

// redeployStack deploys the SAME cloud assembly a second time and reports
// whether CloudFormation could carry the update out at all.
func redeployStack(t *testing.T, stack LocalStack) error {
	t.Helper()
	_, err := cdkDeployE(t, stack.StackName, stack.AssemblyD)
	return err
}

// emulatorCannotUpdateECSService reports whether a failed update failed because
// the emulator cannot update an AWS::ECS::Service at all.
//
// The emulator's CloudFormation reports the service it created as "not found"
// when it comes to update it, and then cascades "Rollback is not implemented"
// across every other resource. That is an emulation gap and not a statement
// about the profile, so a proof that meets it says so rather than reporting a
// deployment defect that does not exist.
func emulatorCannotUpdateECSService(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "AWS::ECS::Service") && strings.Contains(text, "not found")
}

// createQueueOutsideTheStack creates a queue the deployment does not declare and
// removes it afterwards, so a grant that reaches it is a grant that reaches
// anything.
func createQueueOutsideTheStack(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	url := createQueue(t, ctx, name)
	t.Cleanup(func() {
		_, _ = sqs.NewFromConfig(localAWSConfig(t)).DeleteQueue(context.WithoutCancel(ctx),
			&sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
	})
	return url
}

// createQueue creates a queue and leaves it alone afterwards.
//
// The caller that restores a queue the STACK declared uses this rather than
// createQueueOutsideTheStack: the stack still believes it owns that name, and a
// cleanup that deletes it first turns the stack's destroy into a DELETE_FAILED
// which then strands everything else the stack created.
func createQueue(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	created, err := sqs.NewFromConfig(localAWSConfig(t)).CreateQueue(ctx,
		&sqs.CreateQueueInput{QueueName: aws.String(name)})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	return aws.ToString(created.QueueUrl)
}

// drainOne removes one message a probe put on a queue, so a later assertion
// about that queue is not reading the probe's own message.
func drainOne(t *testing.T, ctx context.Context, client *sqs.Client, url string) {
	t.Helper()
	out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(url), MaxNumberOfMessages: 1, WaitTimeSeconds: 2,
	})
	if err != nil || len(out.Messages) == 0 {
		return
	}
	_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(url), ReceiptHandle: out.Messages[0].ReceiptHandle,
	})
}

// requireQueueAbsent fails unless the named queue is gone.
func requireQueueAbsent(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	_, err := sqs.NewFromConfig(localAWSConfig(t)).GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String(name),
	})
	if err == nil {
		t.Errorf("queue %s survived the destroy, so the stack left a resource behind", name)
		return
	}
	var missing *sqstypes.QueueDoesNotExist
	if !errors.As(err, &missing) {
		t.Errorf("cannot tell whether queue %s survived the destroy: %v", name, err)
	}
}
