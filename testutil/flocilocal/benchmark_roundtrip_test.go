package flocilocal_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqssdk "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

// The whole suite now shares one emulator instead of one container per
// service, so the number that matters is what a request costs against it —
// first alone, then with every test package's worth of traffic on the same
// container. A regression here shows up as a slower suite everywhere at once,
// which is otherwise very hard to attribute.

// BenchmarkEmulatorRoundTrip measures one send-plus-receive against the shared
// emulator with no contention.
func BenchmarkEmulatorRoundTrip(b *testing.B) {
	client, queueURL := benchQueue(b, "bench-roundtrip")
	ctx := context.Background()

	for b.Loop() {
		if _, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
			QueueUrl:    aws.String(queueURL),
			MessageBody: aws.String("payload"),
		}); err != nil {
			b.Fatalf("SendMessage: %v", err)
		}
		out, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
			QueueUrl:            aws.String(queueURL),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     1,
		})
		if err != nil {
			b.Fatalf("ReceiveMessage: %v", err)
		}
		// Without this the measurement quietly degrades to "send plus empty
		// poll" the moment the emulator's poll semantics change.
		if len(out.Messages) != 1 {
			b.Fatalf("ReceiveMessage: got %d messages, want the one just sent", len(out.Messages))
		}
		for _, m := range out.Messages {
			if _, err := client.DeleteMessage(ctx, &sqssdk.DeleteMessageInput{
				QueueUrl:      aws.String(queueURL),
				ReceiptHandle: m.ReceiptHandle,
			}); err != nil {
				b.Fatalf("DeleteMessage: %v", err)
			}
		}
	}
}

// BenchmarkEmulatorRoundTripParallel is the shape the suite actually produces:
// many goroutines and many packages driving one container. It answers whether
// sharing the emulator serialises the suite.
//
// Each goroutine gets its own queue. Sharing one would make the goroutines race
// each other for messages rather than the emulator for throughput, and the
// unclaimed backlog would grow with the iteration count until the number
// measured depended on how long the benchmark ran.
func BenchmarkEmulatorRoundTripParallel(b *testing.B) {
	client, _ := benchQueue(b, "bench-parallel")
	ctx := context.Background()

	b.RunParallel(func(pb *testing.PB) {
		queueURL := benchQueueFor(b, client, "bench-parallel-worker")
		for pb.Next() {
			if _, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
				QueueUrl:    aws.String(queueURL),
				MessageBody: aws.String("payload"),
			}); err != nil {
				b.Errorf("SendMessage: %v", err)
				return
			}
			out, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
				QueueUrl:            aws.String(queueURL),
				MaxNumberOfMessages: 1,
				WaitTimeSeconds:     1,
			})
			if err != nil {
				b.Errorf("ReceiveMessage: %v", err)
				return
			}
			if len(out.Messages) != 1 {
				b.Errorf("ReceiveMessage: got %d messages, want the one just sent", len(out.Messages))
				return
			}
			for _, m := range out.Messages {
				if _, err := client.DeleteMessage(ctx, &sqssdk.DeleteMessageInput{
					QueueUrl:      aws.String(queueURL),
					ReceiptHandle: m.ReceiptHandle,
				}); err != nil {
					b.Errorf("DeleteMessage: %v", err)
					return
				}
			}
		}
	})
}

// benchQueue builds a client on the shared emulator and a queue to drive,
// outside the measured region.
func benchQueue(b *testing.B, prefix string) (*sqssdk.Client, string) {
	b.Helper()
	client := sqssdk.NewFromConfig(flocilocal.AWSConfig(b))
	queueURL := benchQueueFor(b, client, prefix)
	b.ResetTimer()
	return client, queueURL
}

// benchQueueFor creates one uniquely named queue and reaps it when the
// benchmark ends.
func benchQueueFor(b *testing.B, client *sqssdk.Client, prefix string) string {
	b.Helper()
	out, err := client.CreateQueue(context.Background(), &sqssdk.CreateQueueInput{
		QueueName: aws.String(unique(prefix)),
	})
	if err != nil {
		b.Fatalf("create queue: %v", err)
	}
	b.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqssdk.DeleteQueueInput{
			QueueUrl: out.QueueUrl,
		})
	})
	return *out.QueueUrl
}
