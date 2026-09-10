package flocilocal_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cwsdk "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	sqssdk "github.com/aws/aws-sdk-go-v2/service/sqs"
	ssmsdk "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

func TestMain(m *testing.M) {
	flocilocal.Configure(flocilocal.WithCleanOrphans(true))
	code := m.Run()
	flocilocal.Shutdown()
	os.Exit(code)
}

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// The package exists so one emulator serves every AWS API the suite needs,
// instead of one wrapper package per service. That is the contract worth
// pinning here: if the emulator ever stops answering one of these APIs, it
// fails in the helper that promises them rather than as a confusing error in
// one of the test packages that depend on the promise.
func TestFlociLocal_OneContainerServesSQSAndSSMAndCloudWatch(t *testing.T) {
	cfg := flocilocal.AWSConfig(t)
	ctx := context.Background()

	sqsClient := sqssdk.NewFromConfig(cfg)
	queue, err := sqsClient.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String(unique("flocilocal-sqs")),
	})
	if err != nil {
		t.Fatalf("SQS CreateQueue: %v", err)
	}
	if _, err := sqsClient.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:    queue.QueueUrl,
		MessageBody: aws.String("hello"),
	}); err != nil {
		t.Fatalf("SQS SendMessage: %v", err)
	}
	received, err := sqsClient.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            queue.QueueUrl,
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     2,
	})
	if err != nil {
		t.Fatalf("SQS ReceiveMessage: %v", err)
	}
	if len(received.Messages) != 1 || *received.Messages[0].Body != "hello" {
		t.Fatalf("SQS roundtrip: got %d messages, want the one that was sent", len(received.Messages))
	}

	ssmClient := ssmsdk.NewFromConfig(cfg)
	param := "/" + unique("flocilocal-ssm")
	if _, err := ssmClient.PutParameter(ctx, &ssmsdk.PutParameterInput{
		Name:  aws.String(param),
		Value: aws.String("secret-value"),
		Type:  ssmtypes.ParameterTypeSecureString,
	}); err != nil {
		t.Fatalf("SSM PutParameter: %v", err)
	}
	got, err := ssmClient.GetParameter(ctx, &ssmsdk.GetParameterInput{
		Name:           aws.String(param),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("SSM GetParameter: %v", err)
	}
	if *got.Parameter.Value != "secret-value" {
		t.Errorf("SSM roundtrip: value = %q, want %q", *got.Parameter.Value, "secret-value")
	}

	cwClient := cwsdk.NewFromConfig(cfg)
	namespace := unique("Flocilocal/CW")
	if _, err := cwClient.PutMetricData(ctx, &cwsdk.PutMetricDataInput{
		Namespace: aws.String(namespace),
		MetricData: []cwtypes.MetricDatum{{
			MetricName: aws.String("Probe"),
			Value:      aws.Float64(1),
		}},
	}); err != nil {
		t.Fatalf("CloudWatch PutMetricData: %v", err)
	}
	metrics, err := cwClient.ListMetrics(ctx, &cwsdk.ListMetricsInput{
		Namespace: aws.String(namespace),
	})
	if err != nil {
		t.Fatalf("CloudWatch ListMetrics: %v", err)
	}
	found := false
	for _, m := range metrics.Metrics {
		if m.MetricName != nil && *m.MetricName == "Probe" {
			found = true
		}
	}
	if !found {
		t.Error("CloudWatch roundtrip: metric Probe was written but does not list")
	}
}

// Every test package shares one container per test binary, so two callers must
// be handed the same emulator — not two.
func TestFlociLocal_EndpointIsSharedAcrossCalls(t *testing.T) {
	first := flocilocal.Endpoint(t)
	second := flocilocal.Endpoint(t)

	if first == "" {
		t.Fatal("Endpoint returned an empty string")
	}
	if first != second {
		t.Errorf("Endpoint is not a per-binary singleton: %q then %q", first, second)
	}
}

// A client built from AWSConfig must reach the emulator rather than real AWS,
// which is what the static credentials and the base endpoint are for.
func TestFlociLocal_AWSConfigTargetsTheEmulator(t *testing.T) {
	cfg := flocilocal.AWSConfig(t)

	if cfg.BaseEndpoint == nil || *cfg.BaseEndpoint != flocilocal.Endpoint(t) {
		t.Errorf("BaseEndpoint = %v, want %q", cfg.BaseEndpoint, flocilocal.Endpoint(t))
	}
	if cfg.Region != flocilocal.Region {
		t.Errorf("Region = %q, want %q", cfg.Region, flocilocal.Region)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieve credentials: %v", err)
	}
	// Checked by source, not by value. The test environment exports
	// AWS_ACCESS_KEY_ID=test, so a config that supplied no credentials of its
	// own would fall through to the environment and hand back the same pair —
	// the source is the only thing that tells the two apart.
	if creds.Source != "StaticCredentials" {
		t.Errorf("credential source = %q, want StaticCredentials; the config is falling through to the ambient AWS environment", creds.Source)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		t.Error("static test credentials are empty; the SDK cannot sign")
	}
}

// Tests that need a guaranteed-clean baseline call ForceStart, so it has to
// actually discard the previous emulator — both its state and its container.
// Handing back a fresh emulator while leaking the old one would pass a
// state-only assertion and still leak one container per call across a suite
// that calls it per test.
func TestFlociLocal_ForceStartReplacesTheRunningEmulator(t *testing.T) {
	client := sqssdk.NewFromConfig(flocilocal.AWSConfig(t))
	name := unique("flocilocal-forcestart")
	if _, err := client.CreateQueue(context.Background(), &sqssdk.CreateQueueInput{
		QueueName: aws.String(name),
	}); err != nil {
		t.Fatalf("CreateQueue before ForceStart: %v", err)
	}
	previous := containerFor(t, flocilocal.Endpoint(t))
	if !isRunning(t, previous) {
		t.Fatalf("container %s is not running, so there is nothing for ForceStart to replace", previous)
	}

	flocilocal.ForceStart(t)

	fresh := sqssdk.NewFromConfig(flocilocal.AWSConfig(t))
	if _, err := fresh.GetQueueUrl(context.Background(), &sqssdk.GetQueueUrlInput{
		QueueName: aws.String(name),
	}); err == nil {
		t.Errorf("queue %q survived ForceStart; the emulator state was not discarded", name)
	}
	if isRunning(t, previous) {
		t.Errorf("container %s is still running after ForceStart; the old emulator leaked", previous)
	}
}

// containerFor names the container serving endpoint. The helper derives both
// from one free port, so the port is the link between them. Asserting on this
// one name rather than on everything matching the container prefix keeps the
// test from treating another process's emulator as its own.
func containerFor(t *testing.T, endpoint string) string {
	t.Helper()
	port := endpoint[strings.LastIndex(endpoint, ":")+1:]
	if port == "" || port == endpoint {
		t.Fatalf("cannot derive a container name from endpoint %q", endpoint)
	}
	return "gobridge-flocilocal-" + port
}

func isRunning(t *testing.T, container string) bool {
	t.Helper()
	out, err := dockerexec.Run(dockerexec.InspectTimeout,
		"ps", "--filter", "name="+container, "--format", "{{.Names}}")
	if err != nil {
		t.Fatalf("docker ps: %v\n%s", err, out)
	}
	return strings.Contains(string(out), container)
}
