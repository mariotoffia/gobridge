package integration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// SQS QueueTags discovery integration tests against the local AWS emulator
//
// The twin of the QueueName suite: where QueueName resolves through
// GetQueueUrl, QueueTags scans the account with ListQueues and reads each
// candidate's tags with ListQueueTags. Only a real emulator proves the tags
// a queue is created with are the tags discovery reads back.
//
// Summary:
// ┌──────┬──────────────────────────────────────────────────────────────┐
// │ Test │ Description                                                  │
// ├──────┼──────────────────────────────────────────────────────────────┤
// │ QT1  │ Sender resolves QueueTags → URL and delivers                 │
// │ QT2  │ Receiver resolves QueueTags → URL and receives               │
// │ QT3  │ Extra tags on the queue still match; a partial queue does not│
// │ QT4  │ Two matching queues are ambiguous, not first-wins            │
// │ QT5  │ Zero matches reports unavailable                             │
// │ QT6  │ QueueNamePrefix narrows an otherwise ambiguous scan          │
// └──────┴──────────────────────────────────────────────────────────────┘
//
// Every selector below is scoped to a value unique to the test that built
// it. ListQueues returns the whole account, so an unscoped selector like
// {app: bridge} would match queues this suite did not create. Tests in this
// package run sequentially against a per-process emulator, so the scope is
// not defending against a sibling test here; it defends the three cases that
// do bite — a -count=N repeat where a DeleteQueue cleanup silently failed, a
// future tagged-queue test in this package, and a run under FLOCI_ENDPOINT
// where several processes share one emulator.
//
// The multi-page ListQueues loop is deliberately absent. Discovery asks for
// 1000 results per page, so a second page needs more than 1000 live queues —
// unreachable here and against real SQS alike. Its guards (a NextToken that
// does not advance, an empty URL) are covered by the adapter's unit tests,
// which can return responses no correct service would.
// ═══════════════════════════════════════════════════════════════════════════

// queueTagScope returns a tag value no other test can be holding, so a
// selector built from it can only ever match queues this test created.
func queueTagScope(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

// TestIntegration_SQS_Sender_QueueTagsResolution validates that a Sender
// configured with QueueTags discovers its queue by tag and delivers to it.
//
// Scenario:
//
//	Sender(QueueTags) ──ListQueues+ListQueueTags──▶ resolve URL
//	                   ──send──▶ [SQS Queue] ◀──poll── Client
func TestIntegration_SQS_Sender_QueueTagsResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := newSQSClient(t)
	scope := queueTagScope(t)
	queueURL := createSQSQueueWithTags(t, client, uniqueQueueName("qt1"), map[string]string{
		"gobridge-scope": scope,
		"role":           "ingress",
	})

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueTags: map[string]string{"gobridge-scope": scope, "role": "ingress"},
		Endpoint:  flocilocal.Endpoint(t),
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qt1-msg-1",
		Subject: "qt1-subject",
		Payload: []byte("resolved-by-tags"),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	bodies := pollSQS(t, client, queueURL, 1, 10*time.Second)
	if len(bodies) != 1 {
		t.Fatalf("expected 1 message on the tag-selected queue, got %d", len(bodies))
	}
	if bodies[0] != "resolved-by-tags" {
		t.Fatalf("body mismatch: got %q, want %q", bodies[0], "resolved-by-tags")
	}
}

// TestIntegration_SQS_Receiver_QueueTagsResolution validates that a Receiver
// configured with QueueTags discovers its queue by tag and consumes from it.
//
// Scenario:
//
//	Client ──send──▶ [SQS Queue]
//	Receiver(QueueTags) ──ListQueues+ListQueueTags──▶ resolve URL
//	                     ──recv──▶ callback
func TestIntegration_SQS_Receiver_QueueTagsResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := newSQSClient(t)
	scope := queueTagScope(t)
	queueURL := createSQSQueueWithTags(t, client, uniqueQueueName("qt2"), map[string]string{
		"gobridge-scope": scope,
	})
	sendToSQS(t, client, queueURL, `{"resolved":"by-tags"}`, nil)

	autoExtend := false
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueTags:         map[string]string{"gobridge-scope": scope},
		Endpoint:          flocilocal.Endpoint(t),
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
		AutoExtend:        &autoExtend,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var received []ports.Delivery
	err = receiver.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
		received = append(received, del)
		_ = del.Ack(ctx)
		cancel()
		return nil
	})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("receiver.Run: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 delivery from the tag-selected queue, got %d", len(received))
	}
	if payload := string(received[0].Envelope().Payload()); payload != `{"resolved":"by-tags"}` {
		t.Fatalf("payload mismatch: got %q", payload)
	}
}

// TestIntegration_SQS_QueueTags_SubsetMatchIgnoresExtraTags validates the
// selector is a subset test in both directions: tags on the queue beyond the
// selector do not disqualify it, and a queue carrying only some of the
// selector's keys is not a match.
//
// Both queues share one tag. Choosing on that alone would pick either, so a
// wrong implementation fails as ambiguity or as the wrong queue, never as a
// pass.
func TestIntegration_SQS_QueueTags_SubsetMatchIgnoresExtraTags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := newSQSClient(t)
	scope := queueTagScope(t)

	// Carries both selector keys plus two the selector never mentions.
	wantURL := createSQSQueueWithTags(t, client, uniqueQueueName("qt3-full"), map[string]string{
		"gobridge-scope": scope,
		"role":           "ingress",
		"team":           "platform",
		"cost-centre":    "42",
	})
	// Carries both selector keys, but "role" holds a different value.
	createSQSQueueWithTags(t, client, uniqueQueueName("qt3-other-value"), map[string]string{
		"gobridge-scope": scope,
		"role":           "egress",
	})
	// Carries only ONE of the two selector keys. This is the case a selector
	// that tested presence instead of value would wrongly match: with two
	// matches the resolver reports ambiguity and the send below fails.
	createSQSQueueWithTags(t, client, uniqueQueueName("qt3-missing-key"), map[string]string{
		"gobridge-scope": scope,
	})

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueTags: map[string]string{"gobridge-scope": scope, "role": "ingress"},
		Endpoint:  flocilocal.Endpoint(t),
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qt3-msg-1",
		Subject: "qt3-subject",
		Payload: []byte("subset-match"),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	bodies := pollSQS(t, client, wantURL, 1, 10*time.Second)
	if len(bodies) != 1 {
		t.Fatalf("expected the fully tagged queue to receive the message, got %d", len(bodies))
	}
	if bodies[0] != "subset-match" {
		t.Fatalf("body mismatch: got %q, want %q", bodies[0], "subset-match")
	}
}

// TestIntegration_SQS_QueueTags_AmbiguousSelectorFails validates that two
// queues matching the same selector is an error, not a silent first-wins
// pick. Choosing arbitrarily would route production traffic to whichever
// queue the account happened to list first.
func TestIntegration_SQS_QueueTags_AmbiguousSelectorFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := newSQSClient(t)
	scope := queueTagScope(t)
	selector := map[string]string{"gobridge-scope": scope, "role": "ingress"}

	createSQSQueueWithTags(t, client, uniqueQueueName("qt4-a"), selector)
	createSQSQueueWithTags(t, client, uniqueQueueName("qt4-b"), selector)

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueTags: selector,
		Endpoint:  flocilocal.Endpoint(t),
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qt4-msg-1",
		Subject: "qt4-subject",
		Payload: []byte("ambiguous"),
	})
	err = sender.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if err == nil {
		t.Fatal("sender.Send succeeded with two queues matching the selector; want an ambiguity error")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("sender.Send error = %v, want shared.ErrInvalidConfig", err)
	}
}

// TestIntegration_SQS_QueueTags_NoMatchIsUnavailable validates that a
// selector matching nothing reports unavailable rather than creating a
// queue, resolving to an empty URL, or silently doing nothing.
func TestIntegration_SQS_QueueTags_NoMatchIsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := newSQSClient(t)
	scope := queueTagScope(t)
	// A queue the scan must fetch tags for and then reject. Without it the
	// test passes against an empty account and proves nothing about matching.
	createSQSQueueWithTags(t, client, uniqueQueueName("qt5-decoy"), map[string]string{
		"gobridge-scope": scope + "-decoy",
	})

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueTags: map[string]string{"gobridge-scope": scope},
		Endpoint:  flocilocal.Endpoint(t),
		Region:    "us-west-1",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qt5-msg-1",
		Subject: "qt5-subject",
		Payload: []byte("no-match"),
	})
	err = sender.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if err == nil {
		t.Fatal("sender.Send succeeded with no queue matching the selector; want an unavailable error")
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("sender.Send error = %v, want shared.ErrUnavailable", err)
	}
}

// TestIntegration_SQS_QueueTags_NamePrefixNarrowsScan validates that
// QueueNamePrefix restricts the scan: the same selector that is ambiguous
// across the account resolves once the prefix excludes the other match.
//
// The two queues are identical in tags, so only the prefix can separate
// them. A prefix that is ignored leaves the selector ambiguous and the test
// fails on the error the previous case asserts.
func TestIntegration_SQS_QueueTags_NamePrefixNarrowsScan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client := newSQSClient(t)
	scope := queueTagScope(t)
	selector := map[string]string{"gobridge-scope": scope}

	prefix := uniqueQueueName("qt6-wanted")
	wantURL := createSQSQueueWithTags(t, client, prefix+"-a", selector)
	createSQSQueueWithTags(t, client, uniqueQueueName("qt6-other"), selector)

	sender, err := sqsadapter.NewSender(sqsadapter.SenderConfig{
		QueueTags:       selector,
		QueueNamePrefix: prefix,
		Endpoint:        flocilocal.Endpoint(t),
		Region:          "us-west-1",
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "qt6-msg-1",
		Subject: "qt6-subject",
		Payload: []byte("prefix-narrowed"),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("sender.Send: %v", err)
	}

	bodies := pollSQS(t, client, wantURL, 1, 10*time.Second)
	if len(bodies) != 1 {
		t.Fatalf("expected the prefixed queue to receive the message, got %d", len(bodies))
	}
	if bodies[0] != "prefix-narrowed" {
		t.Fatalf("body mismatch: got %q, want %q", bodies[0], "prefix-narrowed")
	}
}
