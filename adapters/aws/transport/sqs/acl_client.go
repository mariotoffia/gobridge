package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// sqsAPI is the subset of the SQS SDK client used by Receiver and Sender.
// The real *sqs.Client satisfies this interface. Tests supply a mock.
//
// Every method is used: receive/delete/visibility for consumption,
// send/send-batch for production, get-queue-url for resolution, and
// get-queue-attributes for the poison-queue depth probe. Splitting it would
// fragment a single cohesive SDK seam across several interfaces.
//
//nolint:interfacebloat // SDK-boundary seam: one method per SQS API the
type sqsAPI interface {
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, params *sqs.ChangeMessageVisibilityInput, optFns ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	SendMessageBatch(ctx context.Context, params *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	GetQueueUrl(ctx context.Context, params *sqs.GetQueueUrlInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

// Compile-time check: *sqs.Client satisfies sqsAPI.
var _ sqsAPI = (*sqs.Client)(nil)

func buildAWSConfig(ctx context.Context, region, endpoint, profile string) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error

	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return cfg, shared.ErrUnavailable.Wrap(fmt.Errorf("sqs: load AWS config: %w", err))
	}

	if endpoint != "" {
		cfg.BaseEndpoint = aws.String(endpoint)
	}
	return cfg, nil
}

func resolveQueueURL(ctx context.Context, client sqsAPI, queueURL, queueName string) (string, error) {
	if queueURL != "" {
		return queueURL, nil
	}
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String(queueName),
	})
	if err != nil {
		return "", MapError(err)
	}
	if out.QueueUrl == nil {
		return "", fmt.Errorf("sqs: get queue URL: nil QueueUrl for queue %q", queueName)
	}
	return *out.QueueUrl, nil
}
