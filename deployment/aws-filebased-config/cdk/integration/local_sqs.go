//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/testutil/testcontent"
)

// Driving a deployed data plane from the test process.
//
// Every message carries a TID in its JSON body, so what arrived is compared
// against what was sent BY IDENTITY rather than by count — which is the only
// way a test can tell "all ten arrived" from "one arrived ten times", and the
// difference between the two is the whole point of the restart and rescale
// proofs.

// localQueues addresses the SQS queues of one deployed topology.
type localQueues struct {
	topology string
	client   *sqs.Client
	urls     map[string]string
}

// newLocalQueues resolves the URLs of the named logical queues of a topology.
func newLocalQueues(t *testing.T, topology string, logical ...string) localQueues {
	t.Helper()
	q := localQueues{topology: topology, client: sqs.NewFromConfig(localAWSConfig(t)), urls: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range logical {
		out, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{
			QueueName: aws.String(localQueueName(topology, name)),
		})
		if err != nil {
			t.Fatalf("resolve the deployed queue %q of topology %s: %v", name, topology, err)
		}
		q.urls[name] = aws.ToString(out.QueueUrl)
	}
	return q
}

// URL is the queue URL of one logical queue.
func (q localQueues) URL(t *testing.T, logical string) string {
	t.Helper()
	url, ok := q.urls[logical]
	if !ok {
		t.Fatalf("logical queue %q was not resolved for topology %s", logical, q.topology)
	}
	return url
}

// sendTagged puts n uniquely identified messages on one queue and returns what
// was sent, so the caller can account for every one of them.
func (q localQueues) sendTagged(t *testing.T, ctx context.Context, logical string, n int) []testcontent.Expected {
	t.Helper()
	url := q.URL(t, logical)
	sent := make([]testcontent.Expected, 0, n)
	for i := 0; i < n; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "gobridge.local.deployment",
			Payload: []byte(`{"scenario":"` + q.topology + `"}`),
		})
		_, expected := testcontent.Tag(env)
		if _, err := q.client.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(url),
			MessageBody: aws.String(string(expected.Payload)),
		}); err != nil {
			t.Fatalf("send message %d to %s: %v", i, logical, err)
		}
		sent = append(sent, expected)
	}
	return sent
}

// sendTaggedWithSubject puts one uniquely identified message on a queue with the
// given logical subject, and returns its identifier.
//
// The subject travels in the reserved "Subject" message attribute, which is the
// one place SQS has for it — the same slot the bridge's own sender writes.
func (q localQueues) sendTaggedWithSubject(
	t *testing.T,
	ctx context.Context,
	logical, subject string,
) string {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: subject,
		Payload: []byte(`{"origin":"sqs"}`),
	})
	tid, expected := testcontent.Tag(env)
	if _, err := q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.URL(t, logical)),
		MessageBody: aws.String(string(expected.Payload)),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"Subject": {DataType: aws.String("String"), StringValue: aws.String(subject)},
		},
	}); err != nil {
		t.Fatalf("send a subject-carrying message to %s: %v", logical, err)
	}
	return tid
}

// drain receives from one queue until want messages have arrived or the budget
// runs out, deleting each as it goes so a later phase starts from empty.
//
// It keeps polling for a short settle period after reaching want, so a duplicate
// that arrives just behind the last expected message is caught rather than left
// on the queue for the next phase to trip over.
func (q localQueues) drain(
	t *testing.T,
	ctx context.Context,
	logical string,
	want int,
	timeout time.Duration,
) []testcontent.Received {
	t.Helper()
	bodies := q.receiveBodies(t, ctx, logical, want, timeout)
	return testcontent.ReceivedFromBodies(bodies)
}

// receiveBodies is drain's transport half: it returns the raw bodies.
func (q localQueues) receiveBodies(
	t *testing.T,
	ctx context.Context,
	logical string,
	want int,
	timeout time.Duration,
) []string {
	t.Helper()
	url := q.URL(t, logical)
	bodies := make([]string, 0, want)
	// The settle window is a poll interval past the last expected arrival, not a
	// sleep: it exists so a duplicate delivered right behind the expected set is
	// observed rather than left behind, and it ends as soon as one poll comes
	// back empty.
	const settlePolls = 2
	settled := 0
	err := pollUntil(ctx, time.Second, timeout, func() (bool, error) {
		out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			return false, err
		}
		for _, message := range out.Messages {
			bodies = append(bodies, aws.ToString(message.Body))
			if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(url), ReceiptHandle: message.ReceiptHandle,
			}); err != nil {
				return false, err
			}
		}
		if len(bodies) < want {
			settled = 0
			return false, nil
		}
		if len(out.Messages) > 0 {
			settled = 0
			return false, nil
		}
		settled++
		return settled >= settlePolls, nil
	})
	if err != nil {
		t.Fatalf("queue %s of topology %s yielded %d of %d expected messages within %s: %v",
			logical, q.topology, len(bodies), want, timeout, err)
	}
	return bodies
}

// receiveWithAttributes returns the messages currently on a queue together with
// their SQS message attributes, so a proof can assert on what the sender put
// beside the body.
func (q localQueues) receiveWithAttributes(
	t *testing.T,
	ctx context.Context,
	logical string,
	want int,
	timeout time.Duration,
) []sqstypes.Message {
	t.Helper()
	url := q.URL(t, logical)
	collected := make([]sqstypes.Message, 0, want)
	err := pollUntil(ctx, time.Second, timeout, func() (bool, error) {
		out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			return false, err
		}
		for _, message := range out.Messages {
			collected = append(collected, message)
			if _, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(url), ReceiptHandle: message.ReceiptHandle,
			}); err != nil {
				return false, err
			}
		}
		return len(collected) >= want, nil
	})
	if err != nil {
		t.Fatalf("queue %s of topology %s yielded %d of %d expected messages within %s: %v",
			logical, q.topology, len(collected), want, timeout, err)
	}
	return collected
}

// drainQueueURL is drain for a queue addressed by URL rather than by logical
// name — a queue a proof created itself, outside the deployment.
func drainQueueURL(
	t *testing.T,
	ctx context.Context,
	client *sqs.Client,
	url string,
	want int,
	timeout time.Duration,
) []testcontent.Received {
	t.Helper()
	bodies := make([]string, 0, want)
	err := pollUntil(ctx, time.Second, timeout, func() (bool, error) {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 2,
		})
		if err != nil {
			return false, err
		}
		for _, message := range out.Messages {
			bodies = append(bodies, aws.ToString(message.Body))
			if _, err := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(url), ReceiptHandle: message.ReceiptHandle,
			}); err != nil {
				return false, err
			}
		}
		return len(bodies) >= want, nil
	})
	if err != nil {
		t.Fatalf("%s yielded %d of %d expected messages within %s: %v", url, len(bodies), want, timeout, err)
	}
	return testcontent.ReceivedFromBodies(bodies)
}

// tryReceive waits for one message and reports whether any arrived, so a caller
// can treat "nothing came" as an answer rather than a failure.
func tryReceive(
	t *testing.T,
	ctx context.Context,
	client *sqs.Client,
	url string,
	timeout time.Duration,
) (string, bool) {
	t.Helper()
	body := ""
	err := pollUntil(ctx, time.Second, timeout, func() (bool, error) {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 1, WaitTimeSeconds: 2,
		})
		if err != nil || len(out.Messages) == 0 {
			return false, nil
		}
		body = aws.ToString(out.Messages[0].Body)
		_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: aws.String(url), ReceiptHandle: out.Messages[0].ReceiptHandle,
		})
		return true, nil
	})
	return body, err == nil
}

// waitSQSMessage returns the body of the first message to arrive on a queue.
func waitSQSMessage(
	t *testing.T,
	ctx context.Context,
	client *sqs.Client,
	url string,
	timeout time.Duration,
) string {
	t.Helper()
	body := ""
	err := pollUntil(ctx, time.Second, timeout, func() (bool, error) {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 1, WaitTimeSeconds: 2,
		})
		if err != nil {
			return false, err
		}
		if len(out.Messages) == 0 {
			return false, nil
		}
		body = aws.ToString(out.Messages[0].Body)
		_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: aws.String(url), ReceiptHandle: out.Messages[0].ReceiptHandle,
		})
		return true, nil
	})
	if err != nil {
		t.Fatalf("nothing arrived on %s within %s: %v", url, timeout, err)
	}
	return body
}

// containsAlarmName reports whether a notification body names the alarm. The
// body is SNS's own envelope around the alarm document, so a substring match on
// the name is what identifies it without pinning SNS's envelope shape.
func containsAlarmName(notification, alarmName string) bool {
	return strings.Contains(notification, alarmName)
}

// outstanding is how much work a queue still owns: the messages waiting on it
// plus the ones a consumer has taken and not yet settled.
//
// Both halves matter to a proof about losing work across a restart — a message
// the bridge is holding is exactly the in-flight case — and an absent attribute
// is a failure rather than a zero, because a silent zero would turn "the
// emulator does not report depth" into "there was no work", which is the
// opposite claim.
func (q localQueues) outstanding(t *testing.T, ctx context.Context, logical string) int {
	t.Helper()
	names := []sqstypes.QueueAttributeName{
		sqstypes.QueueAttributeNameApproximateNumberOfMessages,
		sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
	}
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.URL(t, logical)), AttributeNames: names,
	})
	if err != nil {
		t.Fatalf("read the outstanding work on queue %s: %v", logical, err)
	}
	total := 0
	for _, name := range names {
		raw, ok := out.Attributes[string(name)]
		if !ok {
			t.Fatalf("queue %s reports no %s, so how much work it still owns cannot be read: %v",
				logical, name, out.Attributes)
		}
		var value int
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatalf("parse %s of queue %s from %q: %v", name, logical, raw, err)
		}
		total += value
	}
	return total
}
