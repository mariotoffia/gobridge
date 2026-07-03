package runtime_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// timeoutProcessor always fails with a deterministically-transient chain
// timeout (shared.ErrProcessorTimeout), simulating an oversized payload, a
// catastrophic regex, or a hung transform that will never succeed on retry.
type timeoutProcessor struct{}

func (p *timeoutProcessor) Name() string { return "timeout-proc" }

func (p *timeoutProcessor) Process(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
	return shared.ErrProcessorTimeout
}

// newReplayCapRunner builds a direct_hold runner with the given replay cap and
// a FakeDLQStore, plus a processor that always times out (recoverable error).
func newReplayCapRunner(maxReplay int, dlqStore *FakeDLQStore) *route.RouteRunner {
	cfg := route.RouteRunnerConfig{
		RouteID: "replay-cap-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: maxReplay,
		}.WithDefaults(),
		Receiver:   NewFakeReceiver(),
		Sender:     NewFakeSender(),
		DLQ:        dlq.New(dlqStore),
		Bindings:   []routing.DestinationBinding{{ID: "bind-1", Address: "topic"}},
		Processors: []ports.Processor{&timeoutProcessor{}},
	}
	return route.NewRouteRunnerFromConfig(cfg)
}

// TestHandleProcessorError_ReplayCap_PoisonsToDLQ is the regression test for
// FIX 2: handleProcessorError's recoverable branch had NO MaxReplayAttempts
// gate, so a deterministically-transient processor failure (a repeating chain
// timeout) redelivered forever, each attempt pinning a concurrency slot for the
// full ProcessorTimeout and eventually wedging the route semaphore on brokers
// without a native redrive cap. At or above the cap the delivery must now be
// poisoned to the DLQ and terminally acked, never retried again.
func TestHandleProcessorError_ReplayCap_PoisonsToDLQ(t *testing.T) {
	dlqStore := NewFakeDLQStore()
	runner := newReplayCapRunner(3, dlqStore)

	// Receive count already at the cap: the next attempt must poison, not retry.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "poison-msg",
		Subject: "test",
		Headers: map[string]any{"sqs.ApproximateReceiveCount": 3},
	})
	del := NewFakeDelivery(env)

	if err := runner.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery returned error: %v", err)
	}

	if got := dlqStore.Count(); got != 1 {
		t.Fatalf("expected 1 DLQ entry after replay cap, got %d", got)
	}
	if !del.IsAcked() {
		t.Fatal("expected delivery to be terminally acked after poisoning")
	}
	if del.IsRetried() {
		t.Fatal("delivery was retried; the replay cap must stop the infinite retry loop")
	}
}

// TestHandleProcessorError_BelowCap_Retries proves the gate is a gate and not a
// blanket poison: a transient processor error below MaxReplayAttempts is still
// retried (source redelivers), so the cap only fires once the message has
// demonstrably failed enough times.
func TestHandleProcessorError_BelowCap_Retries(t *testing.T) {
	dlqStore := NewFakeDLQStore()
	runner := newReplayCapRunner(3, dlqStore)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "retry-msg",
		Subject: "test",
		Headers: map[string]any{"sqs.ApproximateReceiveCount": 1},
	})
	del := NewFakeDelivery(env)

	if err := runner.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery returned error: %v", err)
	}

	if got := dlqStore.Count(); got != 0 {
		t.Fatalf("expected no DLQ entry below the replay cap, got %d", got)
	}
	if !del.IsRetried() {
		t.Fatal("expected delivery to be retried below the replay cap")
	}
	if del.IsAcked() {
		t.Fatal("delivery was acked; below the cap a transient error must retry, not settle")
	}
	// Guard against RetryDelay regressions turning the retry into an immediate,
	// zero-delay hot loop.
	if del.RetryAfter <= 0 {
		t.Fatalf("expected a positive retry backoff, got %v", del.RetryAfter)
	}
}
