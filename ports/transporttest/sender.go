package transporttest

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// SentMessage records one message a Sender successfully dispatched, so the
// conformance suite can assert what reached the transport destination.
type SentMessage struct {
	// Address is the resolved transport destination the message was sent to.
	Address string
	// EnvelopeID is the ID of the dispatched envelope.
	EnvelopeID string
}

// SenderProbe couples a ports.Sender under test with an observation of the
// messages it dispatched. Sent() returns them in dispatch order.
type SenderProbe interface {
	// Sender is the ports.Sender under test. When Caps.SupportsBatchSend is
	// set it MUST also satisfy ports.BatchSender.
	Sender() ports.Sender
	// Sent returns the messages successfully dispatched so far, in order.
	Sent() []SentMessage
}

// SenderFactory builds a fresh SenderProbe for one test case. Every call
// returns independent state.
type SenderFactory func(t *testing.T) SenderProbe

// RunSenderConformanceTests runs the ports.Sender conformance suite against
// senders produced by factory. It pins that a successful Send dispatches the
// envelope to the requested address and, for a ports.BatchSender
// (Caps.SupportsBatchSend), that SendBatch returns index-aligned results and
// dispatches nothing on a whole-batch pre-validation failure. All subtests are
// race-detector safe.
func RunSenderConformanceTests(t *testing.T, factory SenderFactory, caps Caps) {
	t.Helper()

	t.Run("SendDelivers", func(t *testing.T) {
		senderSendDelivers(t, factory)
	})
	if caps.SupportsBatchSend {
		t.Run("SendBatchIsIndexAligned", func(t *testing.T) {
			senderSendBatchAligned(t, factory)
		})
		t.Run("SendBatchWholeBatchFailureSendsNothing", func(t *testing.T) {
			senderSendBatchWholeFailure(t, factory)
		})
	}
}

func senderSendDelivers(t *testing.T, factory SenderFactory) {
	ctx := context.Background()
	p := factory(t)

	msg := ports.OutboundMessage{Envelope: makeEnvelope("send-1"), Address: "dest/topic"}
	if err := p.Sender().Send(ctx, msg); err != nil {
		t.Fatalf("Send: unexpected error: %v", err)
	}

	sent := p.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent() = %d messages, want 1", len(sent))
	}
	if sent[0].EnvelopeID != "send-1" {
		t.Fatalf("sent envelope id = %q, want %q", sent[0].EnvelopeID, "send-1")
	}
	if sent[0].Address != "dest/topic" {
		t.Fatalf("sent address = %q, want %q", sent[0].Address, "dest/topic")
	}
}

func senderSendBatchAligned(t *testing.T, factory SenderFactory) {
	ctx := context.Background()
	p := factory(t)
	bs, ok := p.Sender().(ports.BatchSender)
	if !ok {
		t.Fatal("Caps.SupportsBatchSend set but Sender does not implement ports.BatchSender")
	}

	msgs := []ports.OutboundMessage{
		{Envelope: makeEnvelope("batch-0"), Address: "dest/topic"},
		{Envelope: makeEnvelope("batch-1"), Address: "dest/topic"},
		{Envelope: makeEnvelope("batch-2"), Address: "dest/topic"},
	}
	results, err := bs.SendBatch(ctx, msgs)
	if err != nil {
		t.Fatalf("SendBatch: unexpected whole-batch error: %v", err)
	}
	if len(results) != len(msgs) {
		t.Fatalf("SendBatch results = %d, want %d (index-aligned)", len(results), len(msgs))
	}
	for i, r := range results {
		if r.Index != i {
			t.Fatalf("results[%d].Index = %d, want %d", i, r.Index, i)
		}
		if r.Err != nil {
			t.Fatalf("results[%d].Err = %v, want nil", i, r.Err)
		}
	}
	if got := len(p.Sent()); got != len(msgs) {
		t.Fatalf("Sent() = %d, want %d", got, len(msgs))
	}
}

func senderSendBatchWholeFailure(t *testing.T, factory SenderFactory) {
	ctx := context.Background()
	p := factory(t)
	bs, ok := p.Sender().(ports.BatchSender)
	if !ok {
		t.Fatal("Caps.SupportsBatchSend set but Sender does not implement ports.BatchSender")
	}

	// A nil envelope is a fail-fast, whole-batch pre-validation failure: the
	// contract requires SendBatch to return (nil, err) and dispatch nothing.
	msgs := []ports.OutboundMessage{
		{Envelope: makeEnvelope("wb-0"), Address: "dest/topic"},
		{Envelope: nil, Address: "dest/topic"},
	}
	results, err := bs.SendBatch(ctx, msgs)
	if err == nil {
		t.Fatal("SendBatch with a nil envelope must return a whole-batch error")
	}
	if results != nil {
		t.Fatalf("SendBatch whole-batch failure returned %d results, want nil", len(results))
	}
	if got := len(p.Sent()); got != 0 {
		t.Fatalf("Sent() = %d after whole-batch failure, want 0 (nothing dispatched)", got)
	}
}
