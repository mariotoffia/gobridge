//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

// Queue plumbing for this package's SQS tests. The emulator helper owns the
// container and the signing config; creating and reaping queues is this
// suite's own business and lives beside the tests that need it.

// newSQSClient returns an SQS client bound to the shared emulator.
func newSQSClient(t testing.TB) *awssqs.Client {
	t.Helper()
	return awssqs.NewFromConfig(flocilocal.AWSConfig(t))
}

// uniqueQueueName suffixes prefix with a nanosecond timestamp so tests sharing
// one emulator cannot collide on a queue name.
func uniqueQueueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// createSQSQueue creates a queue with default attributes and registers a
// t.Cleanup to delete it.
func createSQSQueue(t testing.TB, client *awssqs.Client, name string) string {
	t.Helper()
	return createSQSQueueWithAttrs(t, client, name, nil)
}

// createSQSQueueWithAttrs creates a queue with the given attributes and
// registers a t.Cleanup to delete it.
func createSQSQueueWithAttrs(
	t testing.TB, client *awssqs.Client, name string, attrs map[string]string,
) string {
	t.Helper()
	out, err := client.CreateQueue(context.Background(), &awssqs.CreateQueueInput{
		QueueName:  aws.String(name),
		Attributes: attrs,
	})
	if err != nil {
		t.Fatalf("create queue %q: %v", name, err)
	}
	queueURL := *out.QueueUrl
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &awssqs.DeleteQueueInput{
			QueueUrl: aws.String(queueURL),
		})
	})
	return queueURL
}
