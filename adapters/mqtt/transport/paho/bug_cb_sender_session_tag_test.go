package paho

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// RES-MQTT-CBTAG: CircuitBreakerSender circuit_open metric is missing
//                 the session_id tag emitted on every other failure path.
//
// Defect:
//
//	Sender.Send tags MetricMQTTPublishFailures with
//	    {Key: shared.TagKeySessionID, Value: <client_id>}
//	on every failure (line 78 / 89 of sender.go). Operators rely on
//	this tag to attribute publish failures to a specific MQTT client.
//
//	CircuitBreakerSender.Send (cb_sender.go) instead emits ONLY
//	    {Key: "reason", Value: "circuit_open"}
//	when the breaker rejects the call. The session_id is dropped so the
//	failure cannot be correlated with its source session.
//
// Fix:
//
//	Add the session_id tag alongside the reason tag, matching the
//	Sender.Send tagging convention.
// ═══════════════════════════════════════════════════════════════════════════

// TestRes_CBSender_CircuitOpen_TagsSessionID asserts that when the
// circuit breaker is open, the emitted publish-failure metric carries
// BOTH a reason=circuit_open tag AND a session_id tag identifying the
// underlying MQTT session.
func TestRes_CBSender_CircuitOpen_TagsSessionID(t *testing.T) {
	rec := &ports.RecordingExporter{}

	const clientID = "res-cbtag-client-42"

	// Build a sender with a real Session so the inner sender can read
	// session.opts.ClientID for tagging.
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   clientID,
	}, connectivity.SessionEphemeral, nil, rec)

	inner := NewSender(sess, SenderOptions{Timeout: 1 * time.Second})

	br := circuitbreaker.NewBreaker("res-cbtag-breaker", circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Minute,
		CountError:       shared.IsRecoverableError,
	}, nil)
	cbs := NewCircuitBreakerSender(inner, br)

	// Force the underlying breaker open by simulating a recoverable
	// failure. Pre-flight a call so the breaker counts the trip.
	_ = cbs.breaker.BeforeRequest()
	cbs.breaker.AfterRequest(shared.ErrUnavailable.Wrap(context.Canceled))

	if err := cbs.Send(context.Background(), ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e",
		Subject: "t/cbtag",
		Payload: []byte("p"),
	})}); err == nil {
		t.Fatal("expected error from open circuit")
	}

	entries := rec.FindEntries(shared.MetricMQTTPublishFailures)
	if len(entries) == 0 {
		t.Fatal("expected at least one publish-failure metric")
	}

	var (
		reasonOK   bool
		sessionOK  bool
		sessionVal string
	)
	for _, e := range entries {
		var seenReason, seenSession bool
		for _, tag := range e.Tags {
			if tag.Key == "reason" && tag.Value == "circuit_open" {
				seenReason = true
			}
			if tag.Key == shared.TagKeySessionID {
				seenSession = true
				sessionVal = tag.Value
			}
		}
		if seenReason {
			reasonOK = true
			if seenSession {
				sessionOK = true
			}
		}
	}

	if !reasonOK {
		t.Fatal("missing reason=circuit_open tag (regression in existing fix)")
	}
	if !sessionOK {
		t.Fatalf("BUG-CBTAG: circuit_open publish-failure metric is missing "+
			"a %s tag — operators cannot correlate failures with the source "+
			"session. Add the tag in CircuitBreakerSender.Send.",
			shared.TagKeySessionID)
	}
	if sessionVal != clientID {
		t.Errorf("BUG-CBTAG: session_id tag value = %q, want %q", sessionVal, clientID)
	}
}

// TestRes_CBSender_CircuitOpen_NoSessionID_StillNonEmptyTag verifies the
// fallback when the inner sender has no session: the tag should still be
// present (with a stable placeholder) rather than dropped entirely. This
// keeps metric labels homogeneous in time-series storage.
func TestRes_CBSender_CircuitOpen_NoSession_TagPresent(t *testing.T) {
	rec := &ports.RecordingExporter{}

	cfg := circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Minute,
		CountError:       func(error) bool { return true },
	}
	breaker := circuitbreaker.NewBreaker("test-cb-nosession", cfg, nil)
	_ = breaker.BeforeRequest()
	breaker.AfterRequest(shared.ErrUnavailable)

	cbs := &CircuitBreakerSender{
		inner:   &Sender{metrics: rec}, // no session
		breaker: breaker,
		metrics: rec,
	}

	_ = cbs.Send(context.Background(), ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: "e2", Subject: "t/2"})})

	entries := rec.FindEntries(shared.MetricMQTTPublishFailures)
	if len(entries) == 0 {
		t.Fatal("expected publish-failure metric")
	}

	for _, e := range entries {
		hasSession := false
		for _, tag := range e.Tags {
			if tag.Key == shared.TagKeySessionID {
				hasSession = true
			}
		}
		if !hasSession {
			t.Fatalf("BUG-CBTAG: circuit_open metric must include a %s tag "+
				"even when the session is unavailable (use empty string)",
				shared.TagKeySessionID)
		}
	}
}
