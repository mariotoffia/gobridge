//go:build integration_local
// +build integration_local

// Command lambdafn is the Go function deployed on both sides of the Lambda
// topology of the local deployment suite.
//
// One binary serves both ends because the two differ only in what invokes them
// and where they forward to: the producer is invoked directly and puts its
// payload on the bridge's inbound queue, the consumer is driven by an event
// source mapping on the bridge's outbound queue and puts each record on the
// results queue nothing else writes. Both forward the body BYTE FOR BYTE, so
// what arrives at the end of the loop is what the test sent — a transformation
// anywhere in the middle would be indistinguishable from the bridge losing the
// content otherwise.
//
// It resolves its target queue BY NAME, never by a URL handed to it. A queue
// URL minted by the test process names the emulator's gateway on the host, and
// a launched function container does not reach it under that name; resolving
// through the endpoint the SDK chain already gives the container is the same
// code path an operator's own endpoint override takes. It is the rule
// `byQueueName` applies to the deployed bridge's own transports.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// The environment the deployment gives each function. The role decides which
// half of the loop this instance is; the queue is what it forwards to.
const (
	roleVar   = "GOBRIDGE_FN_ROLE"
	targetVar = "GOBRIDGE_FN_TARGET_QUEUE"

	roleProducer = "producer"
	roleConsumer = "consumer"
)

func main() {
	role := os.Getenv(roleVar)
	if role != roleProducer && role != roleConsumer {
		// Fail here rather than per invocation: a function deployed with no role
		// can never do anything useful, and an init failure names itself in the
		// deploy instead of as a silent no-op under load.
		fmt.Fprintf(os.Stderr, "%s must be %q or %q, got %q\n", roleVar, roleProducer, roleConsumer, role)
		os.Exit(1)
	}
	target := os.Getenv(targetVar)
	if target == "" {
		fmt.Fprintf(os.Stderr, "%s is empty: the function has nowhere to forward to\n", targetVar)
		os.Exit(1)
	}
	lambda.Start(func(ctx context.Context, raw json.RawMessage) error {
		bodies, err := forwarded(role, raw)
		if err != nil {
			return err
		}
		return send(ctx, target, bodies)
	})
}

// forwarded is what this invocation has to put on the target queue.
//
// A producer forwards the invocation payload itself. A consumer forwards one
// body per record of the SQS event the mapping delivered, and refuses an event
// with no records: an empty batch means the mapping delivered something that is
// not the event shape it is documented to deliver, which is worth failing on
// rather than reporting success for having done nothing.
func forwarded(role string, raw json.RawMessage) ([]string, error) {
	if role == roleProducer {
		return []string{string(raw)}, nil
	}
	var event events.SQSEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("decode the SQS event: %w", err)
	}
	if len(event.Records) == 0 {
		return nil, fmt.Errorf("the event source mapping delivered no records: %s", string(raw))
	}
	bodies := make([]string, 0, len(event.Records))
	for _, record := range event.Records {
		bodies = append(bodies, record.Body)
	}
	return bodies, nil
}

// send resolves the target queue by name and puts every body on it.
func send(ctx context.Context, target string, bodies []string) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load the AWS configuration: %w", err)
	}
	client := sqs.NewFromConfig(cfg)
	resolved, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: &target})
	if err != nil {
		return fmt.Errorf("resolve the queue %q from inside the function: %w", target, err)
	}
	for i := range bodies {
		if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    resolved.QueueUrl,
			MessageBody: &bodies[i],
		}); err != nil {
			return fmt.Errorf("send to %q: %w", target, err)
		}
	}
	return nil
}
