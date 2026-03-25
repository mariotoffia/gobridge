// Package sqstests provides test utilities and helpers for SQS transport testing.
package sqstests

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mariotoffia/gobridge/tests/awsutils"
	"github.com/mariotoffia/gobridge/tests/docker"
)

// ============================================================================
// SQS LocalStack Helper
// ============================================================================

// SQSLocalHelper manages LocalStack SQS resources for testing.
type SQSLocalHelper struct {
	t            *testing.T
	container    *docker.LocalStackContainer
	client       *sqs.Client
	endpoint     string
	queues       []string
	roundTripper *awsutils.RoundTripper
	awsConfig    aws.Config
}

// NewSQSLocalHelper creates a helper with LocalStack SQS.
// If container is nil, creates a new LocalStack container.
func NewSQSLocalHelper(t *testing.T, container *docker.LocalStackContainer) *SQSLocalHelper {
	t.Helper()

	var err error
	if container == nil {
		ctx := context.Background()
		container, err = docker.LocalStackForSQS().Start(ctx)
		if err != nil {
			t.Fatalf("failed to start LocalStack: %v", err)
		}
	}

	endpoint := container.SQSEndpoint()

	// Create RoundTripper for error injection
	roundTripper := awsutils.NewRoundTripper(http.DefaultTransport)

	// Create HTTP client with RoundTripper
	httpClient := &http.Client{
		Transport: roundTripper,
	}

	// Create SQS client with custom HTTP client
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "test",
		)),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	return &SQSLocalHelper{
		t:            t,
		container:    container,
		client:       client,
		endpoint:     endpoint,
		queues:       make([]string, 0),
		roundTripper: roundTripper,
		awsConfig:    cfg,
	}
}

// Endpoint returns the LocalStack endpoint URL.
func (h *SQSLocalHelper) Endpoint() string {
	return h.endpoint
}

// Client returns the SQS client.
func (h *SQSLocalHelper) Client() *sqs.Client {
	return h.client
}

// CreateQueue creates a standard SQS queue and returns the URL.
func (h *SQSLocalHelper) CreateQueue(ctx context.Context, name string) string {
	h.t.Helper()

	result, err := h.client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(name),
	})
	if err != nil {
		h.t.Fatalf("failed to create queue %s: %v", name, err)
	}

	h.queues = append(h.queues, *result.QueueUrl)
	return *result.QueueUrl
}

// CreateFIFOQueue creates a FIFO SQS queue and returns the URL.
func (h *SQSLocalHelper) CreateFIFOQueue(ctx context.Context, name string) string {
	h.t.Helper()

	// FIFO queue names must end with .fifo
	if len(name) < 5 || name[len(name)-5:] != ".fifo" {
		name = name + ".fifo"
	}

	result, err := h.client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	if err != nil {
		h.t.Fatalf("failed to create FIFO queue %s: %v", name, err)
	}

	h.queues = append(h.queues, *result.QueueUrl)
	return *result.QueueUrl
}

// SendMessage sends a message to the queue.
func (h *SQSLocalHelper) SendMessage(ctx context.Context, queueURL, body string, attrs map[string]string) string {
	h.t.Helper()

	input := &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(body),
	}

	if len(attrs) > 0 {
		input.MessageAttributes = make(map[string]types.MessageAttributeValue)
		for k, v := range attrs {
			input.MessageAttributes[k] = types.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(v),
			}
		}
	}

	result, err := h.client.SendMessage(ctx, input)
	if err != nil {
		h.t.Fatalf("failed to send message: %v", err)
	}

	return *result.MessageId
}

// SendFIFOMessage sends a message to a FIFO queue.
func (h *SQSLocalHelper) SendFIFOMessage(ctx context.Context, queueURL, body, groupID, dedupID string) string {
	h.t.Helper()

	result, err := h.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(groupID),
		MessageDeduplicationId: aws.String(dedupID),
	})
	if err != nil {
		h.t.Fatalf("failed to send FIFO message: %v", err)
	}

	return *result.MessageId
}

// ReceiveMessages receives messages from the queue.
func (h *SQSLocalHelper) ReceiveMessages(ctx context.Context, queueURL string, maxMessages int32) []types.Message {
	h.t.Helper()

	result, err := h.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(queueURL),
		MaxNumberOfMessages:   maxMessages,
		WaitTimeSeconds:       1,
		MessageAttributeNames: []string{"All"},
		AttributeNames:        []types.QueueAttributeName{types.QueueAttributeNameAll},
	})
	if err != nil {
		h.t.Fatalf("failed to receive messages: %v", err)
	}

	return result.Messages
}

// GetQueueMessageCount returns the approximate number of messages in the queue.
func (h *SQSLocalHelper) GetQueueMessageCount(ctx context.Context, queueURL string) int {
	h.t.Helper()

	result, err := h.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil {
		h.t.Fatalf("failed to get queue attributes: %v", err)
	}

	count := 0
	if v, ok := result.Attributes["ApproximateNumberOfMessages"]; ok {
		fmt.Sscanf(v, "%d", &count)
	}
	return count
}

// PurgeQueue removes all messages from the queue.
func (h *SQSLocalHelper) PurgeQueue(ctx context.Context, queueURL string) {
	h.t.Helper()

	_, err := h.client.PurgeQueue(ctx, &sqs.PurgeQueueInput{
		QueueUrl: aws.String(queueURL),
	})
	if err != nil {
		h.t.Fatalf("failed to purge queue: %v", err)
	}
}

// Cleanup removes all created queues and stops the container if owned.
func (h *SQSLocalHelper) Cleanup(ctx context.Context) {
	// Delete all created queues
	for _, queueURL := range h.queues {
		_, _ = h.client.DeleteQueue(ctx, &sqs.DeleteQueueInput{
			QueueUrl: aws.String(queueURL),
		})
	}
}

// Container returns the underlying LocalStack container.
func (h *SQSLocalHelper) Container() *docker.LocalStackContainer {
	return h.container
}

// RoundTripper returns the RoundTripper for error injection.
// Use this to push error transactions that will be returned instead of
// making real HTTP calls to LocalStack.
//
// Example:
//
//	helper.RoundTripper().Enable().Push(awsutils.SqsErrors{}.OverLimit())
func (h *SQSLocalHelper) RoundTripper() *awsutils.RoundTripper {
	return h.roundTripper
}

// AWSConfig returns the AWS config used by this helper.
// This config includes the custom HTTP transport with RoundTripper.
// Use this when creating SQS Source/Target instances for testing.
func (h *SQSLocalHelper) AWSConfig() aws.Config {
	return h.awsConfig
}

// Region returns the AWS region configured for this helper.
func (h *SQSLocalHelper) Region() string {
	return h.awsConfig.Region
}
