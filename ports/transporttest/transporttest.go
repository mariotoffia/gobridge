// Package transporttest provides conformance test suites for the transport
// ports (ports.Delivery, ports.Receiver, ports.Sender). It mirrors
// ports/storetest: exported RunXxxConformanceTests entry points drive a set of
// t.Run subtests against an implementation the caller supplies through a
// factory, and a Caps descriptor lets a transport declare which optional
// capabilities it implements so the suite skips or inverts the relevant checks.
//
// The suites pin the canonical settlement state machine documented on
// ports.Delivery and the emit-callback contract documented on ports.Receiver.
// A wave-6 transport adapter wires its implementation in with, e.g.:
//
//	func TestSQSDeliveryConformance(t *testing.T) {
//	    transporttest.RunDeliveryConformanceTests(t, newSQSDeliveryProbe, transporttest.Caps{
//	        SupportsRetry:  true,
//	        SupportsExtend: true,
//	    })
//	}
//
// A fully-compliant reference transport lives in reference.go, and
// reference_test.go runs every suite against it to prove the kit itself is
// correct. The kit uses only the standard testing package (no testify), so it
// adds no test-only dependency to an adapter module that imports it.
package transporttest

import (
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Caps declares the optional capabilities a transport implements, so the
// conformance suites can invert or skip capability-gated assertions.
//
// There is deliberately no SupportsNack flag: ports.Delivery has no Nack
// method — Retry IS the negative acknowledgement (nack-with-redelivery), so
// SupportsRetry governs it.
type Caps struct {
	// SupportsRetry reports whether Delivery.Retry effects a real source
	// redelivery. When false, the suite asserts Retry returns an error
	// satisfying errors.Is(err, shared.ErrNotSupported) and does NOT latch
	// settlement (a subsequent Ack still settles).
	SupportsRetry bool

	// SupportsExtend reports whether Delivery.Extend prolongs ownership. When
	// false, the suite asserts Extend returns an error satisfying
	// errors.Is(err, shared.ErrNotSupported). When true, the suite asserts
	// Extend succeeds and does NOT settle the delivery.
	SupportsExtend bool

	// SupportsBatchSend reports whether the Sender under test also implements
	// ports.BatchSender. When true the sender suite additionally exercises the
	// SendBatch index-alignment contract.
	SupportsBatchSend bool
}

// Disposition is the broker-side settlement a Delivery has effected. The
// conformance suites read it through a probe to assert the settlement state
// machine without reaching into transport internals.
type Disposition int

const (
	// DispositionNone means no broker-side settle operation has taken effect:
	// the delivery is still unsettled.
	DispositionNone Disposition = iota
	// DispositionAcked means the message was acked/deleted at the broker
	// (positive settlement) and that disposition is latched.
	DispositionAcked
	// DispositionRetried means the message was returned for redelivery
	// (negative settlement) and that disposition is latched.
	DispositionRetried
)

// String renders a Disposition for test failure messages.
func (d Disposition) String() string {
	switch d {
	case DispositionNone:
		return "none"
	case DispositionAcked:
		return "acked"
	case DispositionRetried:
		return "retried"
	default:
		return "unknown"
	}
}

// makeEnvelope builds a valid envelope for conformance deliveries/sends. The
// ID is required (empty IDs are rejected by messaging construction), so callers
// pass a stable per-case id.
func makeEnvelope(id string) *messaging.Envelope {
	return messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      id,
		Subject: "transporttest.subject",
		Payload: []byte(`{"conformance":true}`),
		Headers: map[string]any{"x-test": "1"},
	})
}
