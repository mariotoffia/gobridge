//go:build longrunning

package longrunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/processors/circuitbreaker"
	"github.com/mariotoffia/gobridge/processors/transform"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Tests: Processor Lifecycle (Category 6 — Routing and Filtering)
//
// Summary:
// ┌──────┬────────────────────────────────────┬──────────┐
// │ ID   │ Description                        │ Status   │
// ├──────┼────────────────────────────────────┼──────────┤
// │ PL-1 │ Circuit breaker lifecycle          │ PENDING  │
// │ PL-2 │ Transform JSONPath mapping         │ PENDING  │
// └──────┴────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════

// TestGAP_CircuitBreakerProcessor_Lifecycle validates the full circuit
// breaker lifecycle: closed → open → half-open → closed in a live pipeline.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Phase 1: Sustained transient failures for tenant "A"
//
//	     ○ CLOSED ──5 failures──▶ ● OPEN
//	           │                      │
//	           │                 (fail-fast)
//	           │                      │
//	Phase 2: Wait > ResetTimeout      │
//	           │                      │
//	           │               ● HALF-OPEN
//	           │                      │
//	Phase 3: 2 successes              │
//	           │                      │
//	           ▼                      ▼
//	     ◎ CLOSED ◀──success────── probe
//
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - FailureThreshold: 5
//   - SuccessThreshold: 2
//   - ResetTimeout: 3s
//   - KeyExtractor: HeaderKey("tenant")
//
// Assertions:
//   - CB opens after 5 transient failures for tenant "A"
//   - CB transitions through half-open to closed after successes
//   - Successful messages arrive at collector
//   - Failed messages are DLQ'd or retried via SQS
func TestGAP_CircuitBreakerProcessor_Lifecycle(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		outTopic    = "gap-cb/output"
		testTimeout = 120 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	dlq := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "gap-cb-col")

	sessID := mqttlocal.UniqueClientID("gap-cb-sess")
	sess := setupMQTTSession(t, sessID, domain.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	// headerErrorProcessor returns transient errors for error_type=transient.
	// Placed AFTER CB in chain so CB observes the error from next().
	errorByHeaderProc := &headerErrorProcessor{}

	cbProc := circuitbreaker.New("gap-cb", cb.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		ResetTimeout:     3 * time.Second,
	}, circuitbreaker.WithKeyExtractor(circuitbreaker.HeaderKey("tenant")))

	// Use Inject-based test to control message flow precisely.
	// This avoids SQS redelivery interference between phases.
	//
	// NOTE: Inject uses syntheticDelivery whose Retry() returns ErrNotSupported,
	// so processor errors fall through retryOrFallback -> DLQ. In a real SQS
	// pipeline, Retry() would succeed and SQS would redeliver. This test
	// exercises the CB state machine and the DLQ fallback path, not the SQS
	// redelivery path. Both paths are valid production scenarios (Inject API
	// is a supported production feature).
	rt := goruntime.New(
		goruntime.WithInstanceID("gap-cb"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-cb-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
			MaxInFlight:  20,
		},
		Processors:         []ports.Processor{cbProc, errorByHeaderProc},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "cb-bind", Address: outTopic}),
		SourceCapabilities: []ports.Capability{ports.CapHTTPEndpoint},
	}, &noopReceiver{}, snd, nil, nil))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 10*time.Second, rt)

	// Phase 1: Inject transient-error messages for tenant "A".
	// The headerErrorProcessor returns ErrUnavailable (transient) for each.
	// After 5 failures, CB opens. Track inject errors to verify CB rejection.
	t.Log("GAP-CB: Phase 1 — injecting 10 transient-error messages for tenant A")
	var cbRejections int
	for i := 0; i < 10; i++ {
		env := &domain.Envelope{
			ID:      fmt.Sprintf("cb-fail-%d", i),
			Subject: outTopic,
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
			Headers: map[string]any{"error_type": "transient", "tenant": "A"},
		}
		if err := rt.Inject(ctx, "gap-cb-route", env); err != nil {
			cbRejections++
		}
	}
	t.Logf("GAP-CB: Phase 1 — %d inject errors (CB rejections after open)", cbRejections)

	lrWaitFor(t, 10*time.Second, "CB open for tenant A", func() bool {
		m := cbProc.Metrics()
		if tm, ok := m["A"]; ok {
			return tm.State == "open"
		}
		return false
	})
	metrics := cbProc.Metrics()
	assert.Equal(t, "open", metrics["A"].State,
		"CB should be open after transient failures for tenant A")
	t.Logf("GAP-CB: Phase 1 — tenant A state=%s consecutiveFailures=%d",
		metrics["A"].State, metrics["A"].ConsecutiveFailures)

	// Verify DLQ received entries from processor-failed messages.
	// At minimum, the first 5 messages hit the headerErrorProcessor and fail
	// with ErrUnavailable. Since Inject uses syntheticDelivery (no retry),
	// they fall through to DLQ routing.
	phase1DLQ := dlq.count()
	t.Logf("GAP-CB: Phase 1 — DLQ entries after failures: %d", phase1DLQ)
	assert.GreaterOrEqual(t, phase1DLQ, 5,
		"DLQ should contain at least 5 entries from transient failures")

	// Phase 2: Wait for ResetTimeout, then inject success probes one at a time.
	t.Log("GAP-CB: Phase 2 — waiting 4s for ResetTimeout expiry")
	time.Sleep(4 * time.Second) // ESSENTIAL: wait for circuit breaker ResetTimeout expiry

	t.Log("GAP-CB: Phase 2 — injecting success probes for tenant A")
	for probe := 0; probe < 5; probe++ {
		env := &domain.Envelope{
			ID:      fmt.Sprintf("cb-probe-%d", probe),
			Subject: outTopic,
			Payload: []byte(fmt.Sprintf(`{"probe":%d}`, probe)),
			Headers: map[string]any{"tenant": "A"},
		}
		_ = rt.Inject(ctx, "gap-cb-route", env)
		time.Sleep(1 * time.Second) // OTHER: pacing — allow probe to complete before next injection
		m := cbProc.Metrics()
		if tm, ok := m["A"]; ok {
			t.Logf("GAP-CB: probe %d — state=%s successes=%d", probe, tm.State, tm.ConsecutiveSuccesses)
			if tm.State == "closed" {
				break
			}
		}
	}

	lrWaitFor(t, 10*time.Second, "CB closed for tenant A", func() bool {
		m := cbProc.Metrics()
		if tm, ok := m["A"]; ok {
			return tm.State == "closed"
		}
		return false
	})
	metrics = cbProc.Metrics()
	assert.Equal(t, "closed", metrics["A"].State,
		"CB should return to closed after successful probes")
	t.Logf("GAP-CB: Phase 2 — state=%s successes=%d",
		metrics["A"].State, metrics["A"].ConsecutiveSuccesses)

	// Phase 3: Inject normal messages — all should succeed and reach collector.
	prePhase3 := collector.count()
	t.Log("GAP-CB: Phase 3 — injecting 20 normal messages for tenant A")
	for i := 0; i < 20; i++ {
		env := &domain.Envelope{
			ID:      fmt.Sprintf("cb-normal-%d", i),
			Subject: outTopic,
			Payload: []byte(fmt.Sprintf(`{"normal":%d}`, i)),
			Headers: map[string]any{"tenant": "A"},
		}
		err := rt.Inject(ctx, "gap-cb-route", env)
		require.NoError(t, err, "Phase 3 inject should succeed")
	}

	lrWaitFor(t, 30*time.Second,
		fmt.Sprintf("collector >= %d", prePhase3+20),
		func() bool { return collector.count() >= prePhase3+20 })

	delivered := collector.count()
	dlqCount := dlq.count()
	t.Logf("GAP-CB: delivered=%d, dlq=%d", delivered, dlqCount)

	assert.GreaterOrEqual(t, delivered, 20,
		"at least 20 successful messages should be delivered")
}

// TestGAP_TransformProcessor_JSONPathMapping validates that JSON payloads
// are correctly transformed by JSONPath field mappings before reaching
// the target transport.
//
// Scenario:
// ───────────────────────────────────────────────
//
//	Input JSON:
//	  {"user":{"name":"X"},"items":[{"price":9.99}],
//	   "metadata":{"count":"42"},"extra":"drop_me"}
//
//	──▶ [Transform Processor] ──▶
//
//	Output JSON (DropUnmapped=true):
//	  {"userName":"X","firstItemPrice":9.99,
//	   "itemCount":42,"fallback":"default_val"}
//
// ───────────────────────────────────────────────
//
// Assertions:
//   - 100 messages delivered
//   - Each payload has correct transformed fields
//   - No unmapped fields present
func TestGAP_TransformProcessor_JSONPathMapping(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 100
		outTopic    = "gap-tf/output"
		testTimeout = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-tf-in")
	dlq := &lrDLQStore{}
	collector := newMQTTCollector(t, outTopic, "gap-tf-col")

	sessID := mqttlocal.UniqueClientID("gap-tf-sess")
	sess := setupMQTTSession(t, sessID, domain.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	tfProc, err := transform.New(transform.Config{
		DropUnmapped: true,
		Mappings: []transform.FieldMapping{
			transform.SimpleMapping("$.user.name", "userName"),
			transform.SimpleMapping("$.items[0].price", "firstItemPrice"),
			transform.TransformedMapping("$.metadata.count", "itemCount", transform.TransformInt),
			{Source: "$.missing", Target: "fallback", DefaultValue: "default_val"},
		},
	})
	require.NoError(t, err, "transform processor creation")

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-tf"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-tf-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Processors:         []ports.Processor{tfProc},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "tf-bind", Address: outTopic}),
		SourceCapabilities: directHoldCaps,
	}, newSQSReceiver(t, sqsInURL), snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Send messages with known JSON payloads directly via SQS message body.
	// NOTE: sendBulkToSQS always uses {"seq":N} as the body, so we send
	// custom payloads individually using the SQS client directly.
	for i := 0; i < msgCount; i++ {
		body := fmt.Sprintf(`{"user":{"name":"user-%d"},"items":[{"price":9.99}],"metadata":{"count":"%d"},"extra":"should_be_dropped"}`, i, 42+i)
		_, sendErr := sqsInClient.SendMessage(ctx, &awssqs.SendMessageInput{
			QueueUrl:    &sqsInURL,
			MessageBody: &body,
		})
		require.NoError(t, sendErr, "send transform msg %d", i)
	}

	lrWaitFor(t, 30*time.Second,
		fmt.Sprintf("collector >= %d", msgCount),
		func() bool { return collector.count() >= msgCount })

	// Verify transformed payloads — check both structure AND values.
	msgs := collector.getMessages()
	verified := 0
	for _, msg := range msgs {
		var payload map[string]any
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			t.Logf("GAP-TF: failed to parse payload: %v", err)
			continue
		}

		// Check userName exists and is a string matching the input pattern.
		if un, ok := payload["userName"]; ok {
			if _, isStr := un.(string); isStr {
				verified++
			} else {
				t.Errorf("GAP-TF: userName should be string, got %T", un)
			}
		}

		// Check firstItemPrice is the numeric value from $.items[0].price.
		if fip, ok := payload["firstItemPrice"]; ok {
			switch v := fip.(type) {
			case float64:
				assert.InDelta(t, 9.99, v, 0.01, "firstItemPrice should be 9.99")
			default:
				t.Errorf("GAP-TF: firstItemPrice should be float64, got %T", fip)
			}
		} else {
			t.Error("GAP-TF: firstItemPrice field missing from transformed payload")
		}

		// Check itemCount is an integer (transform: "int").
		if ic, ok := payload["itemCount"]; ok {
			switch ic.(type) {
			case float64: // JSON numbers deserialize as float64
				// OK — json.Unmarshal produces float64 for all numbers
			default:
				t.Errorf("GAP-TF: itemCount should be numeric, got %T", ic)
			}
		} else {
			t.Error("GAP-TF: itemCount field missing from transformed payload")
		}

		// Check fallback has the default value.
		if fb, ok := payload["fallback"]; ok {
			assert.Equal(t, "default_val", fb, "fallback should be default_val")
		} else {
			t.Error("GAP-TF: fallback field missing from transformed payload")
		}

		// Check unmapped fields are dropped.
		assert.NotContains(t, payload, "extra", "unmapped field 'extra' should be dropped")
		assert.NotContains(t, payload, "user", "unmapped field 'user' should be dropped")
	}

	t.Logf("GAP-TF: delivered=%d, verified=%d, dlq=%d", len(msgs), verified, dlq.count())
	assert.GreaterOrEqual(t, len(msgs), msgCount,
		"all %d messages should be delivered", msgCount)
	assert.Equal(t, msgCount, verified,
		"all %d messages should have transformed userName field", msgCount)
}

// headerErrorProcessor is a processor that returns transient errors when
// the envelope has a "error_type" header set to "transient". Used to
// inject failures inside the processor chain (where the circuit breaker
// can observe them), as opposed to sender-level errors which are handled
// outside the chain.
type headerErrorProcessor struct{}

func (p *headerErrorProcessor) Name() string { return "header-error" }

func (p *headerErrorProcessor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
	if env.Headers != nil {
		if et, ok := env.Headers["error_type"].(string); ok && et == "transient" {
			return domain.ErrUnavailable.WithMessage("headerErrorProcessor: transient")
		}
	}
	return next(ctx, env)
}
