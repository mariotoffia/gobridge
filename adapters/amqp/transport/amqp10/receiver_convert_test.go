// ═══════════════════════════════════════════════
// Receiver Message Conversion Tests
//
// Validates convertMessage behaviour including BUG-4:
// missing envelope ID generation for ID-less messages.
// ═══════════════════════════════════════════════
package amqp10

import (
	"errors"
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

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if env.ID() == "" {
		t.Fatal("Envelope.ID() should be auto-generated when message has no MessageID (BUG-4)")
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

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if env.ID() != "msg-123" {
		t.Fatalf("Envelope.ID() = %q, want %q", env.ID(), "msg-123")
	}
}

// TestReceiver_ConvertMessage_ValueBodyExtraction validates []byte
// value-body extraction.
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

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if string(env.Payload()) != "value-body" {
		t.Fatalf("Payload = %q, want %q", env.Payload(), "value-body")
	}
}

// TestReceiver_ConvertMessage_ValueBodyString validates that a STRING
// amqp-value body (what Qpid-JMS/Artemis TextMessage produces) is
// converted to bytes rather than forwarded as an empty payload
// (finding 1).
func TestReceiver_ConvertMessage_ValueBodyString(t *testing.T) {
	msg := &amqp.Message{Value: "hello-text"}

	env, err := messageToEnvelope(msg, nil)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if string(env.Payload()) != "hello-text" {
		t.Fatalf("Payload = %q, want %q", env.Payload(), "hello-text")
	}
}

// TestReceiver_ConvertMessage_MultiSectionData validates that a body
// carried in MULTIPLE data sections is concatenated, not truncated to
// the first section (finding 1: previously Data[0] silently dropped the
// remaining sections).
func TestReceiver_ConvertMessage_MultiSectionData(t *testing.T) {
	msg := &amqp.Message{
		Data: [][]byte{[]byte("part-one:"), []byte("part-two:"), []byte("part-three")},
	}

	env, err := messageToEnvelope(msg, nil)
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}
	if got, want := string(env.Payload()), "part-one:part-two:part-three"; got != want {
		t.Fatalf("Payload = %q, want %q (all data sections concatenated)", got, want)
	}
}

// TestReceiver_ConvertMessage_ValueBodyUnrepresentable pins the corrected
// behaviour for finding 1: a non-string/[]byte amqp-value body cannot be
// represented as a byte payload, so conversion REJECTS the message
// (errUnrepresentableBody) instead of forwarding an empty envelope and
// Acking-then-deleting the source (irrecoverable body loss). The receive
// loop settles such a message through the errIngressRejected path.
func TestReceiver_ConvertMessage_ValueBodyUnrepresentable(t *testing.T) {
	cases := map[string]any{
		"map":    map[string]any{"k": "v"},
		"number": int64(42),
		"list":   []any{1, 2, 3},
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			msg := &amqp.Message{Value: val}
			env, err := messageToEnvelope(msg, nil)
			if err == nil {
				t.Fatalf("messageToEnvelope returned nil error and env=%v; want rejection for unrepresentable body", env)
			}
			if !errors.Is(err, errUnrepresentableBody) {
				t.Fatalf("error = %v, want errUnrepresentableBody", err)
			}
			if env != nil {
				t.Fatalf("env = %v, want nil (message rejected, never forwarded empty)", env)
			}
		})
	}
}

// TestReceiver_ConvertMessage_SequenceBodyUnrepresentable pins that an
// amqp-sequence-only body is rejected rather than forwarded empty
// (finding 1).
func TestReceiver_ConvertMessage_SequenceBodyUnrepresentable(t *testing.T) {
	msg := &amqp.Message{Sequence: [][]any{{"a", "b"}}}
	_, err := messageToEnvelope(msg, nil)
	if !errors.Is(err, errUnrepresentableBody) {
		t.Fatalf("error = %v, want errUnrepresentableBody", err)
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

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if env.Subject() != subject {
		t.Fatalf("Subject = %q, want %q", env.Subject(), subject)
	}
}

// TestReceiver_ConvertMessage_SubjectAbsent validates that a message
// without Properties.Subject yields an Envelope with empty Subject —
// the receiver does not fall back to the configured link address.
func TestReceiver_ConvertMessage_SubjectAbsent(t *testing.T) {
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

	env, err := messageToEnvelope(msg, r.clock())
	if err != nil {
		t.Fatalf("messageToEnvelope: %v", err)
	}

	if env.Subject() != "" {
		t.Fatalf("Subject = %q, want empty (no link-address fallback)", env.Subject())
	}
}
