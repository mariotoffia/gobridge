package route

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// TestReceiveCountHeaderKeys_WireContract pins the exact wire-key strings the
// runtime uses to read each source transport's redelivery count. The
// runtime cannot import the adapter packages (arch-lint layer rule) and the
// adapter constants are unexported in separate Go modules, so these values are
// mirrored by hand — an adapter that renamed its wire key would otherwise
// silently disable MaxReplayAttempts for that transport with zero failing tests.
//
// This test fails the instant the runtime side drifts. Each adapter also pins
// its OWN key value (cross-referenced below), so a rename on either side surfaces
// as a failing test and these comments route a developer to the paired location
// that MUST change in lockstep:
//
//   - "sqs.ApproximateReceiveCount"
//     adapter: adapters/aws/transport/sqs/acl_inbound.go ("sqs." + attr name)
//     pin:     adapters/aws/transport/sqs/headers_test.go
//   - "asb.delivery-count"
//     adapter: adapters/azure/transport/servicebus/headers.go (asbHeaderDeliveryCount)
//     pin:     adapters/azure/transport/servicebus/headers_test.go
//   - "amqp10.delivery-count"
//     adapter: adapters/amqp/transport/amqp10/headers.go (headerDeliveryCount)
//     pin:     adapters/amqp/transport/amqp10/headers_test.go
func TestReceiveCountHeaderKeys_WireContract(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"sqs", headerSQSReceiveCount, "sqs.ApproximateReceiveCount"},
		{"asb", headerASBDeliveryCount, "asb.delivery-count"},
		{"amqp10", headerAMQP10DeliveryCount, "amqp10.delivery-count"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("wire-key drift: got %q, want %q — an adapter rename or a runtime edit broke the receiveCount contract; update BOTH the runtime constant and the paired adapter header key + its own pin test", c.got, c.want)
			}
		})
	}

	// receiveCountHeaderKeys must cover exactly the pinned set, in precedence
	// order, so the strip/detector helpers can never fall out of sync with the
	// constants receiveCount reads.
	want := []string{"sqs.ApproximateReceiveCount", "asb.delivery-count", "amqp10.delivery-count"}
	if len(receiveCountHeaderKeys) != len(want) {
		t.Fatalf("receiveCountHeaderKeys = %v, want %v", receiveCountHeaderKeys, want)
	}
	for i := range want {
		if receiveCountHeaderKeys[i] != want[i] {
			t.Errorf("receiveCountHeaderKeys[%d] = %q, want %q", i, receiveCountHeaderKeys[i], want[i])
		}
	}
}

// TestUnparseableReceiveCountKey pins detector: it must flag a
// present-but-uninterpretable count ONLY when receiveCount fails open to 0
// (no usable fallback count), never on a cleanly parsed count (even a literal 0)
// nor on an absent header.
func TestUnparseableReceiveCountKey(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]any
		want    string
	}{
		{"absent -> no signal", map[string]any{"other": "x"}, ""},
		{"nil headers -> no signal", nil, ""},
		{"valid int -> no signal", map[string]any{headerSQSReceiveCount: 3}, ""},
		{"literal zero is parsed, not garbage", map[string]any{headerSQSReceiveCount: 0}, ""},
		{"garbage string -> flags key", map[string]any{headerSQSReceiveCount: "notanumber"}, headerSQSReceiveCount},
		{"garbage type -> flags key", map[string]any{headerASBDeliveryCount: []byte("x")}, headerASBDeliveryCount},
		{
			name:    "garbage sqs but valid asb fallback -> no signal",
			headers: map[string]any{headerSQSReceiveCount: "bad", headerASBDeliveryCount: 4},
			want:    "",
		},
		{
			name:    "both garbage -> flags first in precedence",
			headers: map[string]any{headerSQSReceiveCount: "bad", headerAMQP10DeliveryCount: "worse"},
			want:    headerSQSReceiveCount,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "u", Payload: []byte("x"), Headers: tt.headers})
			if got := unparseableReceiveCountKey(env); got != tt.want {
				t.Errorf("unparseableReceiveCountKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSendDirectHold_UnparseableReceiveCountEmitsSignal proves's
// observability: a delivery carrying a present-but-garbage redelivery count is
// treated as a first delivery (fail-open, correct) but now emits a metric AND a
// debug log so the resulting silent-unbounded-retry becomes observable. A
// well-typed count emits nothing.
func TestSendDirectHold_UnparseableReceiveCountEmitsSignal(t *testing.T) {
	// A transient (recoverable) send failure is the harm path: with rc==0 the
	// replay cap never fires, so the delivery is retried rather than DLQ'd.
	recoverable := shared.NewBridgeError(shared.ErrCodeConnectionLost, shared.ErrorTransient, "transient send failure")

	t.Run("present-but-garbage count emits metric + debug log and still retries", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		var logbuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "garbage-rc",
			Payload: []byte("p"),
			Headers: map[string]any{headerSQSReceiveCount: "not-a-number"},
		})
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID: "r1",
			Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxReplayAttempts: 5},
			Sender:  stubSender{err: recoverable},
			Metrics: rec,
			Logger:  logger,
		})
		del := &stubDelivery{env: env}

		if err := r.sendDirectHold(context.Background(), del, env, routing.DispatchPlan{BindingID: "b1", Address: "addr"}); err != nil {
			t.Fatalf("sendDirectHold returned error: %v", err)
		}
		if got := countCounter(rec, shared.MetricReceiveCountUnparseable); got != 1 {
			t.Fatalf("MetricReceiveCountUnparseable emitted %d times, want 1", got)
		}
		if !strings.Contains(logbuf.String(), "unparseable redelivery-count header") {
			t.Fatalf("expected debug log for unparseable count, got: %q", logbuf.String())
		}
		if !del.retried || del.acked {
			t.Fatalf("expected uncapped-retry path (retried, not acked); got retried=%v acked=%v", del.retried, del.acked)
		}
	})

	t.Run("well-typed count emits no signal", func(t *testing.T) {
		rec := &ports.RecordingExporter{}
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "good-rc",
			Payload: []byte("p"),
			Headers: map[string]any{headerSQSReceiveCount: 2},
		})
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID: "r1",
			Policy:  routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxReplayAttempts: 5},
			Sender:  stubSender{err: recoverable},
			Metrics: rec,
		})
		if err := r.sendDirectHold(context.Background(), &stubDelivery{env: env}, env, routing.DispatchPlan{BindingID: "b1", Address: "addr"}); err != nil {
			t.Fatalf("sendDirectHold returned error: %v", err)
		}
		if got := countCounter(rec, shared.MetricReceiveCountUnparseable); got != 0 {
			t.Fatalf("well-typed count must not emit the unparseable signal, got %d", got)
		}
	})
}

// TestBuildOutboxRecords_StampsBridgeSpanForDrain proves: the
// shared-outbox build path now stamps THIS bridge's active span onto the
// persisted envelope, so a record drained later (by a separate drainer with no
// access to the span) propagates this bridge hop downstream instead of the bare
// upstream traceparent. The source envelope is never mutated, and with tracing
// off the upstream traceparent passes through unchanged.
func TestBuildOutboxRecords_StampsBridgeSpanForDrain(t *testing.T) {
	const bridgeTP = "00-11111111111111111111111111111111-2222222222222222-01"
	const upstreamTP = "00-99999999999999999999999999999999-8888888888888888-01"

	newEnv := func(id string) *messaging.Envelope {
		return messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      id,
			Payload: []byte("p"),
			Headers: map[string]any{messaging.HeaderTraceParent: upstreamTP},
		})
	}
	plans := []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}
	bindings := []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}}

	t.Run("active span replaces upstream traceparent on the persisted record", func(t *testing.T) {
		tr := &fakeTracer{inject: map[string]any{messaging.HeaderTraceParent: bridgeTP}}
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID:  "r1",
			Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Tracer:   tr,
			Bindings: bindings,
		})
		env := newEnv("e1")

		recs, err := r.buildOutboxRecords(context.Background(), env, plans)
		if err != nil {
			t.Fatalf("buildOutboxRecords: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("records = %d, want 1", len(recs))
		}
		if got, _ := messaging.GetHeaderString(recs[0].Snapshot().Headers(), messaging.HeaderTraceParent); got != bridgeTP {
			t.Fatalf("persisted traceparent = %q, want bridge hop %q (upstream %q must be replaced so downstream parents on this bridge)", got, bridgeTP, upstreamTP)
		}
		// Source envelope must be untouched — retry/receiveCount paths re-read it.
		if srcTP, _ := messaging.GetHeaderString(env.Headers(), messaging.HeaderTraceParent); srcTP != upstreamTP {
			t.Fatalf("source envelope traceparent mutated to %q, want untouched %q", srcTP, upstreamTP)
		}
	})

	t.Run("tracing off: upstream traceparent passes through", func(t *testing.T) {
		r := NewRouteRunnerFromConfig(RouteRunnerConfig{
			RouteID:  "r1",
			Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
			Bindings: bindings,
		})
		env := newEnv("e2")

		recs, err := r.buildOutboxRecords(context.Background(), env, plans)
		if err != nil {
			t.Fatalf("buildOutboxRecords: %v", err)
		}
		if got, _ := messaging.GetHeaderString(recs[0].Snapshot().Headers(), messaging.HeaderTraceParent); got != upstreamTP {
			t.Fatalf("tracing off: persisted traceparent = %q, want upstream pass-through %q", got, upstreamTP)
		}
	})
}

// --- minimal test doubles (package route has no shared fakes) ---------------

type stubSender struct{ err error }

func (s stubSender) Send(context.Context, ports.OutboundMessage) error { return s.err }

type stubDelivery struct {
	env     *messaging.Envelope
	acked   bool
	retried bool
}

func (d *stubDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *stubDelivery) Ack(context.Context) error     { d.acked = true; return nil }
func (d *stubDelivery) Retry(context.Context, time.Duration, error) error {
	d.retried = true
	return nil
}
func (d *stubDelivery) Extend(context.Context, time.Time) error { return nil }

type noopTestSpan struct{}

func (noopTestSpan) End()                           {}
func (noopTestSpan) SetError(error)                 {}
func (noopTestSpan) AddEvent(string, ...shared.Tag) {}
func (noopTestSpan) SetAttributes(...shared.Tag)    {}

// fakeTracer.Inject stamps a fixed set of keys onto the carrier so a test can
// assert the runtime propagated THIS hop's context.
type fakeTracer struct{ inject map[string]any }

func (t *fakeTracer) StartSpan(ctx context.Context, _ string, _ ...shared.Tag) (context.Context, ports.Span) {
	return ctx, noopTestSpan{}
}
func (t *fakeTracer) Extract(ctx context.Context, _ map[string]any) context.Context { return ctx }
func (t *fakeTracer) Inject(_ context.Context, headers map[string]any) map[string]any {
	for k, v := range t.inject {
		headers[k] = v
	}
	return headers
}
func (t *fakeTracer) Close(context.Context) error { return nil }

func countCounter(rec *ports.RecordingExporter, name string) int {
	n := 0
	for _, e := range rec.Entries() {
		if e.Kind == "counter" && e.Name == name {
			n++
		}
	}
	return n
}
