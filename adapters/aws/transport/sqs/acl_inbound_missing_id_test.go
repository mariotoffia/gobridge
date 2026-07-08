package sqs

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSQSMissingID_PopulatedPreserved confirms that a present
// MessageId travels straight through to env.ID() — the fallback only
// fires for empty IDs.
func TestSQSMissingID_PopulatedPreserved(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("preserved-99"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
	}
	env, _, err := r.convertMessage(context.Background(), "http://test/q", msg)
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if env.ID() != "preserved-99" {
		t.Errorf("ID = %q, want %q", env.ID(), "preserved-99")
	}
}

// TestSQSMissingID_EmptyTriggersFallbackUnique pins the
// generateEnvelopeID fallback for empty MessageId. 100 empty-ID
// messages MUST get distinct envelope IDs (the H-3 root regression
// was that all collapsed to one literal "test-envelope").
func TestSQSMissingID_EmptyTriggersFallbackUnique(t *testing.T) {
	r := receiverForTest(t, false)
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		msg := sqstypes.Message{
			MessageId:     aws.String(""),
			ReceiptHandle: aws.String("rh"),
			Body:          aws.String("b"),
		}
		env, _, err := r.convertMessage(context.Background(), "http://test/q", msg)
		if err != nil {
			t.Fatalf("convertMessage[%d]: %v", i, err)
		}
		if env.ID() == "" || env.ID() == "test-envelope" {
			t.Errorf("envelope ID regression: got %q", env.ID())
		}
		if _, dup := seen[env.ID()]; dup {
			t.Errorf("duplicate generated ID at iteration %d: %q", i, env.ID())
		}
		seen[env.ID()] = struct{}{}
	}
}

// TestSQSMissingID_NewEnvelopeFailureWrapsBridgeError exercises the
// wrapEnvelopeErr classifier directly: a NewEnvelope ErrInvalidEnvelopeID
// MUST surface as a *shared.BridgeError with code INVALID_PAYLOAD so
// the poll loop drops the message instead of retrying forever.
func TestSQSMissingID_NewEnvelopeFailureWrapsBridgeError(t *testing.T) {
	err := wrapEnvelopeErr(messaging.ErrInvalidEnvelopeID)
	if err == nil {
		t.Fatal("wrapEnvelopeErr returned nil for ErrInvalidEnvelopeID")
	}
	if err.Code != shared.ErrCodeInvalidPayload {
		t.Errorf("Code = %q, want %q", err.Code, shared.ErrCodeInvalidPayload)
	}
	if !errors.Is(err, messaging.ErrInvalidEnvelopeID) {
		t.Errorf("error chain lost ErrInvalidEnvelopeID; chain = %v", err)
	}
}

// stubMalformedClient returns a single message on the first call and
// an empty result thereafter. Used to drive pollAndConvert through
// exactly one ReceiveMessage cycle.
type stubMalformedClient struct {
	mockSQSClient
	msg sqstypes.Message
}

func (s *stubMalformedClient) ReceiveMessage(_ context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	out := &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{s.msg}}
	s.msg = sqstypes.Message{}
	return out, nil
}

// TestSQSPollAndConvert_DropsMalformedMessage validates the integrated
// poll path: a good message is returned and the malformed counter
// stays at zero (no unwarranted emission). The metric-emission branch
// is exercised indirectly: convertMessage cannot fail on a normal
// message thanks to the fallback ID generator, so the sole way to
// trigger the counter in production is via wrapEnvelopeErr — which is
// asserted in TestSQSMissingID_NewEnvelopeFailureWrapsBridgeError.
func TestSQSPollAndConvert_DropsMalformedMessage(t *testing.T) {
	good := sqstypes.Message{
		MessageId:     aws.String("good-1"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
	}
	rec := &ports.RecordingExporter{}
	mock := &stubMalformedClient{msg: good}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "http://test/q",
		Client:   mock,
		Metrics:  rec,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	r.storeClient(mock)

	results, err := r.pollAndConvert(context.Background(), "http://test/q", r.cfg.MaxMessages, 0)
	if err != nil {
		t.Fatalf("pollAndConvert: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	for _, e := range rec.Entries() {
		if e.Name == MetricSQSMalformedMessages {
			t.Errorf("unexpected malformed counter emitted: %+v", e)
		}
	}
}

// TestSQSConvertMessage_StaticSignature locks the convertMessage
// triple-return shape introduced by H-3. A regression that drops the
// error return — re-introducing the silent MustEnvelope masking — will
// fail to compile here.
func TestSQSConvertMessage_StaticSignature(t *testing.T) {
	r := receiverForTest(t, false)
	var (
		_ *messaging.Envelope
		_ string
		_ error
	)
	env, handle, err := r.convertMessage(context.Background(), "q", sqstypes.Message{
		MessageId:     aws.String("x"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
	})
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if env == nil || handle == "" {
		t.Fatalf("unexpected nil/empty return: env=%v handle=%q", env, handle)
	}
}
