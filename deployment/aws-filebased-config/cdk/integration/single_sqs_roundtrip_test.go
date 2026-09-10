//go:build integration_aws
// +build integration_aws

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// TestIntegration_Single_SQS_Roundtrip deploys the same single
// fixture, then uses the AWS SDK directly to SendMessage to the
// inbound queue and ReceiveMessage from the outbound queue. The route
// is configured 1:1 (inbound -> outbound) so a unique payload sent
// to inbound must reappear on outbound within the polling window.
func TestIntegration_Single_SQS_Roundtrip(t *testing.T) {
	env := RequireSandbox(t)

	app := NewApp(t, env)
	stackName := StackName(env, "single-rt")
	stack := awscdk.NewStack(app, jsii.String(stackName), &awscdk.StackProps{
		Env: StackEnv(env),
	})
	fix := newSingleFixture(stack, env)
	ApplyDestroyAspect(stack)

	// Surface queue URLs so this test can avoid name lookups.
	awscdk.NewCfnOutput(stack, jsii.String("InboundQueueUrl"), &awscdk.CfnOutputProps{
		Value: fix.InboundQ.QueueUrl(),
	}).OverrideLogicalId(jsii.String("InboundQueueUrl"))
	awscdk.NewCfnOutput(stack, jsii.String("OutboundQueueUrl"), &awscdk.CfnOutputProps{
		Value: fix.OutboundQ.QueueUrl(),
	}).OverrideLogicalId(jsii.String("OutboundQueueUrl"))

	outputs := DeployStack(t, app, env, stackName)
	inboundURL := outputs["InboundQueueUrl"]
	outboundURL := outputs["OutboundQueueUrl"]
	healthz := outputs["HealthzURL"]
	if inboundURL == "" || outboundURL == "" || healthz == "" {
		t.Fatalf("missing required outputs: %v", outputs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	sqsClient := sqs.NewFromConfig(awsCfg)

	// Wait for healthz so the runtime is actually pumping.
	if err := pollUntil(ctx, 15*time.Second, 10*time.Minute, func() (bool, error) {
		status, _, err := httpGet(ctx, healthz)
		if err != nil {
			return false, nil
		}
		return status == 200, nil
	}); err != nil {
		t.Fatalf("healthz never became 200: %v", err)
	}

	payload := fmt.Sprintf("gobridge-it-%d", time.Now().UnixNano())
	if _, err := sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &inboundURL,
		MessageBody: &payload,
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	t.Logf("sent %s to %s", payload, inboundURL)

	if err := pollUntil(ctx, 5*time.Second, 60*time.Second, func() (bool, error) {
		out, err := sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &outboundURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     5,
		})
		if err != nil {
			return false, err
		}
		for _, m := range out.Messages {
			if m.Body != nil && strings.Contains(*m.Body, payload) {
				_, _ = sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl:      &outboundURL,
					ReceiptHandle: m.ReceiptHandle,
				})
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("payload %q never appeared on outbound: %v", payload, err)
	}
}
