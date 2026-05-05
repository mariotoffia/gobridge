// ═══════════════════════════════════════════════
// Receiver Message Conversion Tests
//
// Validates convertMessage behaviour including BUG-4:
// missing envelope ID generation for ID-less messages.
// ═══════════════════════════════════════════════
package amqp10

import (
	"log/slog"
	"testing"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/ports"
)

// TestReceiver_ConvertMessage_MissingID exposes BUG-4: when an AMQP 1.0
// message has no MessageID property, the envelope ID should be auto-generated
// (like amqp091 does), not left empty.
func TestReceiver_ConvertMessage_MissingID(t *testing.T) {
	sess := newTestSession()
	sess.dial = mockDialFunc(&mockConn{}, nil)

	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/test", LinkCredit: 10},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	msg := &amqp.Message{
		Data: [][]byte{[]byte("payload")},
	}

	env := messageToEnvelope(msg, r.cfg.Address, r.clock())

	if env.ID == "" {
		t.Fatal("Envelope.ID should be auto-generated when message has no MessageID (BUG-4)")
	}
}

// TestReceiver_ConvertMessage_WithID validates that MessageID is preserved.
func TestReceiver_ConvertMessage_WithID(t *testing.T) {
	sess := newTestSession()
	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/test", LinkCredit: 10},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			MessageID: "msg-123",
		},
		Data: [][]byte{[]byte("payload")},
	}

	env := messageToEnvelope(msg, r.cfg.Address, r.clock())

	if env.ID != "msg-123" {
		t.Fatalf("Envelope.ID = %q, want %q", env.ID, "msg-123")
	}
}

// TestReceiver_ConvertMessage_ValueBodyExtraction validates value-body extraction.
func TestReceiver_ConvertMessage_ValueBodyExtraction(t *testing.T) {
	sess := newTestSession()
	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/test", LinkCredit: 10},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	msg := &amqp.Message{
		Value: []byte("value-body"),
	}

	env := messageToEnvelope(msg, r.cfg.Address, r.clock())

	if string(env.Payload) != "value-body" {
		t.Fatalf("Payload = %q, want %q", env.Payload, "value-body")
	}
}

// TestReceiver_ConvertMessage_ValueBodyNonBytes validates that a non-[]byte
// Value results in nil payload.
func TestReceiver_ConvertMessage_ValueBodyNonBytes(t *testing.T) {
	sess := newTestSession()
	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/test", LinkCredit: 10},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	msg := &amqp.Message{
		Value: "not-bytes",
	}

	env := messageToEnvelope(msg, r.cfg.Address, r.clock())

	if env.Payload != nil {
		t.Fatalf("Payload = %v, want nil for non-[]byte Value", env.Payload)
	}
}

// TestReceiver_ConvertMessage_Subject validates Subject mapping.
func TestReceiver_ConvertMessage_Subject(t *testing.T) {
	sess := newTestSession()
	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/test", LinkCredit: 10},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	subject := "events.user.created"
	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			Subject: &subject,
		},
		Data: [][]byte{[]byte("data")},
	}

	env := messageToEnvelope(msg, r.cfg.Address, r.clock())

	if env.Subject != subject {
		t.Fatalf("Subject = %q, want %q", env.Subject, subject)
	}
}

// TestReceiver_ConvertMessage_SubjectDefault validates Subject defaults to address.
func TestReceiver_ConvertMessage_SubjectDefault(t *testing.T) {
	sess := newTestSession()
	r := &Receiver{
		cfg:     ReceiverConfig{Address: "queue/default-subject", LinkCredit: 10},
		session: sess,
		logger:  slog.Default(),
		metrics: &ports.NoopExporter{},
	}

	msg := &amqp.Message{
		Data: [][]byte{[]byte("data")},
	}

	env := messageToEnvelope(msg, r.cfg.Address, r.clock())

	if env.Subject != "queue/default-subject" {
		t.Fatalf("Subject = %q, want %q", env.Subject, "queue/default-subject")
	}
}
