package sqs

// Production-readiness regression tests for Chunk 13 (AWS SQS plugin).
//
//   - SendMessageBatch per-entry failures must classify the AWS
//     error Code with the SAME policy as MapError (KMS + throttling codes)
//     BEFORE falling back to SenderFault, so a per-entry RETRYABLE target
//     outage stays retryable instead of becoming a terminal reject.
//   - a malformed ("poison") message must not hot-loop forever. The
//     receiver warns at startup when the source queue has no native redrive
//     policy, and — when poison_max_receives is configured — DELETES a poison
//     message once its ApproximateReceiveCount reaches that bound to break the
//     loop.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// batch per-entry AWS error-code classification
// ═══════════════════════════════════════════════════════════════════════════

// TestClassifyBatchEntryCode locks the per-entry Code → BridgeError
// policy in isolation. It mirrors MapError: KMS and throttling/service codes
// are classified explicitly; every other code returns (nil, false) so the
// caller falls back to the SenderFault verdict.
func TestClassifyBatchEntryCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		code      string
		wantMatch bool
		wantCode  shared.ErrorCode
		wantClass shared.ErrorClass
	}{
		// KMS — retryable.
		{"kms access denied (propagating) is transient auth", "KmsAccessDenied", true, shared.ErrCodeTemporaryAuthFailure, shared.ErrorTransient},
		{"kms throttled is throttled", "KmsThrottled", true, shared.ErrCodeThrottled, shared.ErrorTransient},
		// KMS — permanent misconfiguration.
		{"kms disabled is not authorized", "KmsDisabled", true, shared.ErrCodeNotAuthorized, shared.ErrorPermanent},
		{"kms invalid state is not authorized", "KmsInvalidState", true, shared.ErrCodeNotAuthorized, shared.ErrorPermanent},
		{"kms not found is not authorized", "KmsNotFound", true, shared.ErrCodeNotAuthorized, shared.ErrorPermanent},
		{"kms opt-in required is not authorized", "KmsOptInRequired", true, shared.ErrCodeNotAuthorized, shared.ErrorPermanent},
		{"kms invalid key usage is not authorized", "KmsInvalidKeyUsage", true, shared.ErrCodeNotAuthorized, shared.ErrorPermanent},
		// Throttling / rate limit — retryable.
		{"ThrottlingException is throttled", "ThrottlingException", true, shared.ErrCodeThrottled, shared.ErrorTransient},
		{"RequestThrottled is throttled", "RequestThrottled", true, shared.ErrCodeThrottled, shared.ErrorTransient},
		{"OverLimit is throttled", "OverLimit", true, shared.ErrCodeThrottled, shared.ErrorTransient},
		// Service faults — retryable.
		{"ServiceUnavailable is unavailable", "ServiceUnavailable", true, shared.ErrCodeUnavailable, shared.ErrorTransient},
		{"InternalError is unavailable", "InternalError", true, shared.ErrCodeUnavailable, shared.ErrorTransient},
		// Unmatched — caller falls back to SenderFault.
		{"empty code is unmatched", "", false, "", ""},
		{"InvalidParameterValue is unmatched (sender fault path)", "InvalidParameterValue", false, "", ""},
		{"plain AccessDenied is unmatched (sender fault path)", "AccessDenied", false, "", ""},
		{"MissingParameter is unmatched (sender fault path)", "MissingParameter", false, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, matched := classifyBatchEntryCode(tc.code)
			if matched != tc.wantMatch {
				t.Fatalf("classifyBatchEntryCode(%q) matched = %v, want %v", tc.code, matched, tc.wantMatch)
			}
			if !tc.wantMatch {
				if got != nil {
					t.Fatalf("unmatched code %q must return nil error, got %v", tc.code, got)
				}
				return
			}
			if got.Code != tc.wantCode {
				t.Errorf("code %q → %s, want %s", tc.code, got.Code, tc.wantCode)
			}
			if got.Class != tc.wantClass {
				t.Errorf("code %q → class %s, want %s", tc.code, got.Class, tc.wantClass)
			}
		})
	}
}

// TestSendBatch_RetryableCodesStayRetryable is the end-to-end
// regression: a SendMessageBatch failed entry whose Code names a retryable
// KMS / throttling / service condition must classify retryable through
// SendBatch, EVEN when SenderFault=true. Before the fix every SenderFault=true
// entry mapped to ErrInvalidPayload (Rejected, terminal) and every
// SenderFault=false entry to ErrUnavailable — throwing the Code (and its
// retry semantics) away.
func TestSendBatch_RetryableCodesStayRetryable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		code        string
		senderFault bool
		wantCode    shared.ErrorCode
		wantClass   shared.ErrorClass
	}{
		{
			// The finding's exact scenario: KMS access denied during
			// key-policy / IAM propagation, reported with SenderFault=true.
			name: "KmsAccessDenied+SenderFault stays retryable (not terminal reject)",
			code: "KmsAccessDenied", senderFault: true,
			wantCode: shared.ErrCodeTemporaryAuthFailure, wantClass: shared.ErrorTransient,
		},
		{
			name: "KmsThrottled surfaces as throttled, not generic unavailable",
			code: "KmsThrottled", senderFault: false,
			wantCode: shared.ErrCodeThrottled, wantClass: shared.ErrorTransient,
		},
		{
			name: "ThrottlingException surfaces as throttled",
			code: "ThrottlingException", senderFault: false,
			wantCode: shared.ErrCodeThrottled, wantClass: shared.ErrorTransient,
		},
		{
			name: "KmsDisabled is permanent not-authorized (matches MapError)",
			code: "KmsDisabled", senderFault: true,
			wantCode: shared.ErrCodeNotAuthorized, wantClass: shared.ErrorPermanent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			results := sendBatchWithFailedEntry(t, tc.code, tc.senderFault)
			if results[0].Err == nil {
				t.Fatal("expected the failed entry (index 0) to carry an error")
			}
			be, ok := shared.AsBridgeError(results[0].Err)
			if !ok {
				t.Fatalf("expected BridgeError, got %T: %v", results[0].Err, results[0].Err)
			}
			if be.Code != tc.wantCode {
				t.Errorf("code=%q sender_fault=%v → %s, want %s", tc.code, tc.senderFault, be.Code, tc.wantCode)
			}
			if be.Class != tc.wantClass {
				t.Errorf("code=%q sender_fault=%v → class %s, want %s", tc.code, tc.senderFault, be.Class, tc.wantClass)
			}
			// The Code and sender_fault must still be attached as context for
			// observability regardless of classification.
			if got := be.Context["code"]; got != tc.code {
				t.Errorf("context[code] = %v, want %q", got, tc.code)
			}
		})
	}
}

// TestSendBatch_UnknownCodeFallsBackToSenderFault guards the fallback:
// a Code outside the KMS/throttling/service set keeps the SenderFault verdict —
// a caller-malformed request is rejected, a server fault is transient.
func TestSendBatch_UnknownCodeFallsBackToSenderFault(t *testing.T) {
	t.Parallel()

	t.Run("sender fault → rejected", func(t *testing.T) {
		t.Parallel()
		results := sendBatchWithFailedEntry(t, "InvalidParameterValue", true)
		be, ok := shared.AsBridgeError(results[0].Err)
		if !ok {
			t.Fatalf("expected BridgeError, got %T", results[0].Err)
		}
		if be.Class != shared.ErrorRejected {
			t.Errorf("SenderFault=true unknown code → %s, want %s", be.Class, shared.ErrorRejected)
		}
	})

	t.Run("server fault → transient", func(t *testing.T) {
		t.Parallel()
		results := sendBatchWithFailedEntry(t, "SomeUnknownServerError", false)
		be, ok := shared.AsBridgeError(results[0].Err)
		if !ok {
			t.Fatalf("expected BridgeError, got %T", results[0].Err)
		}
		if be.Class != shared.ErrorTransient {
			t.Errorf("SenderFault=false unknown code → %s, want %s", be.Class, shared.ErrorTransient)
		}
	})
}

// sendBatchWithFailedEntry dispatches a one-message batch whose single entry
// SQS reports as failed with the given Code / SenderFault, and returns the
// per-entry results.
func sendBatchWithFailedEntry(t *testing.T, code string, senderFault bool) []ports.BatchResult {
	t.Helper()
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			return &awssqs.SendMessageBatchOutput{
				Failed: []sqstypes.BatchResultErrorEntry{{
					Id:          aws.String("0"),
					SenderFault: senderFault,
					Code:        aws.String(code),
					Message:     aws.String("entry failed: " + code),
				}},
			}, nil
		},
	}
	s := &Sender{
		queueURL: "https://sqs.us-west-1.amazonaws.com/123/q",
		cfg:      SenderConfig{Timeout: 10 * time.Second, BatchSize: 10},
		metrics:  &ports.NoopExporter{},
		clk:      clocktest.NewAt(time.Unix(1_700_000_000, 0)),
	}
	s.storeClient(mock)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m-0", Payload: []byte(`{"ok":true}`)})
	results, err := s.SendBatch(context.Background(), []ports.OutboundMessage{{Envelope: env}})
	if err != nil {
		t.Fatalf("dispatched batch must not yield a whole-batch error, got %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	return results
}

// ═══════════════════════════════════════════════════════════════════════════
// poison backstop + startup redrive-policy validation
// ═══════════════════════════════════════════════════════════════════════════

// poisonMessage builds a malformed SQS message that fails NewEnvelope (the
// receiver's clock is the zero time, so conversion returns
// ErrEnvelopeClockMissing) carrying the given ApproximateReceiveCount.
func poisonMessage(recvCount string) sqstypes.Message {
	return sqstypes.Message{
		MessageId:     aws.String("poison-1"),
		ReceiptHandle: aws.String("rh-poison"),
		Body:          aws.String("x"),
		Attributes:    map[string]string{"ApproximateReceiveCount": recvCount},
	}
}

// newPoisonReceiver builds a zero-clock Receiver (so conversion always fails)
// with a recording metrics exporter and the given poison backstop bound.
func newPoisonReceiver(t *testing.T, poisonMax int32, rec *ports.RecordingExporter) *Receiver {
	t.Helper()
	r, err := NewReceiver(ReceiverConfig{
		QueueURL:          "http://test/q",
		Clock:             clocktest.NewAt(time.Time{}),
		Metrics:           rec,
		PoisonMaxReceives: poisonMax,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	return r
}

// TestPollAndConvert_PoisonBackstopDeletesAtThreshold verifies the
// adapter-enforced backstop: once a poison message's ApproximateReceiveCount
// reaches poison_max_receives, the receiver DELETES it (breaking the hot loop)
// and records SQSPoisonDropped. Without the fix a poison message is never
// deleted and redelivers forever when the queue has no native redrive policy.
func TestPollAndConvert_PoisonBackstopDeletesAtThreshold(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{poisonMessage("5")}}, nil
		},
	}
	r := newPoisonReceiver(t, 5, rec)

	results, err := r.pollAndConvert(context.Background(), mock, "http://test/q", 10, 0)
	if err != nil {
		t.Fatalf("pollAndConvert: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0 (poison dropped)", len(results))
	}

	mock.mu.Lock()
	deletes := append([]awssqs.DeleteMessageInput(nil), mock.DeleteCalls...)
	mock.mu.Unlock()
	if len(deletes) != 1 {
		t.Fatalf("DeleteMessage calls = %d, want 1 (backstop delete)", len(deletes))
	}
	if got := aws.ToString(deletes[0].ReceiptHandle); got != "rh-poison" {
		t.Errorf("deleted receipt handle = %q, want %q", got, "rh-poison")
	}
	if n := len(rec.FindEntries(MetricSQSPoisonDropped)); n != 1 {
		t.Fatalf("%s entries = %d, want 1", MetricSQSPoisonDropped, n)
	}
	// Still counted as malformed so existing dashboards keep working.
	if n := len(rec.FindEntries(MetricSQSMalformedMessages)); n != 1 {
		t.Fatalf("%s entries = %d, want 1", MetricSQSMalformedMessages, n)
	}
}

// TestPollAndConvert_PoisonBackstopBelowThresholdNoDelete verifies the
// backstop does NOT fire before the bound: the message is dropped WITHOUT a
// delete so a native redrive policy still owns it.
func TestPollAndConvert_PoisonBackstopBelowThresholdNoDelete(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{poisonMessage("4")}}, nil
		},
	}
	r := newPoisonReceiver(t, 5, rec)

	if _, err := r.pollAndConvert(context.Background(), mock, "http://test/q", 10, 0); err != nil {
		t.Fatalf("pollAndConvert: %v", err)
	}

	mock.mu.Lock()
	deletes := len(mock.DeleteCalls)
	mock.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("DeleteMessage calls = %d, want 0 (below backstop bound)", deletes)
	}
	if n := len(rec.FindEntries(MetricSQSPoisonDropped)); n != 0 {
		t.Fatalf("%s entries = %d, want 0", MetricSQSPoisonDropped, n)
	}
}

// TestPollAndConvert_PoisonBackstopDisabledNoDelete verifies the
// default (poison_max_receives = 0) preserves the drop-WITHOUT-delete
// behaviour even at a very high receive count — native redrive stays in charge.
func TestPollAndConvert_PoisonBackstopDisabledNoDelete(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return &awssqs.ReceiveMessageOutput{Messages: []sqstypes.Message{poisonMessage("9999")}}, nil
		},
	}
	r := newPoisonReceiver(t, 0, rec)

	if _, err := r.pollAndConvert(context.Background(), mock, "http://test/q", 10, 0); err != nil {
		t.Fatalf("pollAndConvert: %v", err)
	}

	mock.mu.Lock()
	deletes := len(mock.DeleteCalls)
	mock.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("DeleteMessage calls = %d, want 0 (backstop disabled)", deletes)
	}
	if n := len(rec.FindEntries(MetricSQSPoisonDropped)); n != 0 {
		t.Fatalf("%s entries = %d, want 0", MetricSQSPoisonDropped, n)
	}
}

// TestCheckRedrivePolicy exercises the startup redrive-policy check
// with the poison backstop OFF (advisory mode): a queue with no RedrivePolicy
// is surfaced (metric), one with a policy is silent, and a GetQueueAttributes
// error degrades gracefully. None of these fail startup.
func TestCheckRedrivePolicy(t *testing.T) {
	t.Parallel()

	redriveAttr := string(sqstypes.QueueAttributeNameRedrivePolicy)

	cases := []struct {
		name        string
		fn          func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error)
		wantMissing int
	}{
		{
			name: "no redrive policy → missing metric emitted",
			fn: func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
				return &awssqs.GetQueueAttributesOutput{Attributes: map[string]string{}}, nil
			},
			wantMissing: 1,
		},
		{
			name: "redrive policy present → silent",
			fn: func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
				return &awssqs.GetQueueAttributesOutput{Attributes: map[string]string{
					redriveAttr: `{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:1:dlq","maxReceiveCount":"5"}`,
				}}, nil
			},
			wantMissing: 0,
		},
		{
			name: "GetQueueAttributes denied → graceful, no metric",
			fn: func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
				return nil, errors.New("AccessDenied: not authorized to perform sqs:GetQueueAttributes")
			},
			wantMissing: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &ports.RecordingExporter{}
			mock := &mockSQSClient{GetQueueAttributesFn: tc.fn}
			r, err := NewReceiver(ReceiverConfig{
				QueueURL: "http://test/q",
				Metrics:  rec,
			}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}

			if err := r.checkRedrivePolicy(context.Background(), mock, "http://test/q"); err != nil {
				t.Fatalf("advisory check must never fail startup, got %v", err)
			}
			if got := len(rec.FindEntries(MetricSQSMissingRedrivePolicy)); got != tc.wantMissing {
				t.Fatalf("%s entries = %d, want %d", MetricSQSMissingRedrivePolicy, got, tc.wantMissing)
			}
		})
	}
}

// TestCheckRedrivePolicy_PoisonVsNativeRedrive is the destructive-
// preemption guard (adversarial follow-up). With the poison backstop ON the
// startup check STILL reads the native redrive policy and REFUSES to start
// when the backstop would fire at or before native maxReceiveCount — the
// adapter's DeleteMessage would destroy the payload before SQS moves it to the
// DLQ. A backstop above maxReceiveCount, or no native redrive at all, is safe.
// An unreadable policy degrades to a best-effort warning, never a start
// failure.
func TestCheckRedrivePolicy_PoisonVsNativeRedrive(t *testing.T) {
	t.Parallel()

	redriveAttr := string(sqstypes.QueueAttributeNameRedrivePolicy)
	redrive := func(maxReceive string) func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
		return func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
			return &awssqs.GetQueueAttributesOutput{Attributes: map[string]string{
				redriveAttr: `{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:1:dlq","maxReceiveCount":"` + maxReceive + `"}`,
			}}, nil
		}
	}

	cases := []struct {
		name      string
		poisonMax int32
		fn        func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error)
		wantErr   bool
	}{
		{
			name:      "backstop below native maxReceiveCount → rejected (preempts DLQ)",
			poisonMax: 3, fn: redrive("5"), wantErr: true,
		},
		{
			name:      "backstop equal to native maxReceiveCount → rejected (still preempts)",
			poisonMax: 5, fn: redrive("5"), wantErr: true,
		},
		{
			name:      "backstop above native maxReceiveCount → accepted (native redrive wins)",
			poisonMax: 6, fn: redrive("5"), wantErr: false,
		},
		{
			name:      "native maxReceiveCount as JSON number is tolerated → rejected",
			poisonMax: 3,
			fn: func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
				return &awssqs.GetQueueAttributesOutput{Attributes: map[string]string{
					redriveAttr: `{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:1:dlq","maxReceiveCount":5}`,
				}}, nil
			},
			wantErr: true,
		},
		{
			name:      "no native redrive, backstop on → accepted (backstop is the sole bound)",
			poisonMax: 3,
			fn: func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
				return &awssqs.GetQueueAttributesOutput{Attributes: map[string]string{}}, nil
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := &mockSQSClient{GetQueueAttributesFn: tc.fn}
			r, err := NewReceiver(ReceiverConfig{
				QueueURL:          "http://test/q",
				Metrics:           &ports.RecordingExporter{},
				PoisonMaxReceives: tc.poisonMax,
			}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}

			gotErr := r.checkRedrivePolicy(context.Background(), mock, "http://test/q")
			if tc.wantErr {
				if gotErr == nil {
					t.Fatalf("poison_max_receives=%d vs native redrive: want startup error, got nil", tc.poisonMax)
				}
				if be, ok := shared.AsBridgeError(gotErr); !ok || be.Class != shared.ErrorPermanent {
					t.Fatalf("conflict must be a permanent config error, got %v", gotErr)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("poison_max_receives=%d vs native redrive: want nil, got %v", tc.poisonMax, gotErr)
			}
		})
	}
}

// TestCheckRedrivePolicy_UnreadableStaysBestEffort proves the
// permission-degraded path: with the backstop ON and GetQueueAttributes
// denied, startup is NOT failed (least-privilege deployments keep running) but
// a Warn is emitted so the unverifiable destructive backstop is visible.
func TestCheckRedrivePolicy_UnreadableStaysBestEffort(t *testing.T) {
	t.Parallel()

	h := &captureHandler{}
	mock := &mockSQSClient{
		GetQueueAttributesFn: func(context.Context, *awssqs.GetQueueAttributesInput, ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error) {
			return nil, errors.New("AccessDenied: not authorized to perform sqs:GetQueueAttributes")
		},
	}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL:          "http://test/q",
		PoisonMaxReceives: 5,
		Logger:            slog.New(h),
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	if err := r.checkRedrivePolicy(context.Background(), mock, "http://test/q"); err != nil {
		t.Fatalf("unreadable redrive must not fail startup, got %v", err)
	}

	records := h.snapshot()
	var warned bool
	for _, rec := range records {
		if rec.Level == slog.LevelWarn {
			warned = true
		}
	}
	if !warned {
		t.Fatal("expected a Warn when the destructive backstop could not be verified against native redrive")
	}
	if _, ok := findAttr(records, "poison_max_receives"); !ok {
		t.Fatal("expected the warning to carry the poison_max_receives attribute")
	}
}

// TestConfigValidate_PoisonMaxReceives locks the static config-surface
// validation: a negative poison_max_receives is a fault; a value of exactly 1
// is rejected unless the poison_drop_without_dlq opt-in is set (a single failed
// receive dropping the payload is almost never intended); 0 and values >= 2 are
// accepted. Covers both the plugin Config.Validate and the ReceiverConfig
// constructor path.
func TestConfigValidate_PoisonMaxReceives(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		value          int32
		dropWithoutDLQ bool
		wantErr        bool
	}{
		{"negative is rejected", -1, false, true},
		{"zero disables (accepted)", 0, false, false},
		{"one without opt-in is rejected", 1, false, true},
		{"one with opt-in is accepted", 1, true, false},
		{"two is accepted (no opt-in needed)", 2, false, false},
		{"large value is accepted", 25, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Plugin config surface.
			c := DefaultConfig()
			c.QueueURL = "http://test/q"
			c.PoisonMaxReceives = tc.value
			c.PoisonDropWithoutDLQ = tc.dropWithoutDLQ
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Config.Validate poison_max_receives=%d opt_in=%v: want error, got nil", tc.value, tc.dropWithoutDLQ)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Config.Validate poison_max_receives=%d opt_in=%v: want nil, got %v", tc.value, tc.dropWithoutDLQ, err)
			}

			// Constructor path (NewReceiver -> ReceiverConfig.validate) must
			// agree, so a direct embedder cannot bypass the guard. Negative is
			// excluded here only because it is exercised above; every other row
			// is checked.
			if tc.value < 0 {
				return
			}
			_, rerr := NewReceiver(ReceiverConfig{
				QueueURL:             "http://test/q",
				PoisonMaxReceives:    tc.value,
				PoisonDropWithoutDLQ: tc.dropWithoutDLQ,
			}, nil)
			if tc.wantErr && rerr == nil {
				t.Fatalf("NewReceiver poison_max_receives=%d opt_in=%v: want error, got nil", tc.value, tc.dropWithoutDLQ)
			}
			if !tc.wantErr && rerr != nil {
				t.Fatalf("NewReceiver poison_max_receives=%d opt_in=%v: want nil, got %v", tc.value, tc.dropWithoutDLQ, rerr)
			}
		})
	}
}
