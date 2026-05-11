package servicebus

import (
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestASBReceivedToEnvelope_PreservesPopulatedMessageID confirms the
// happy path: a non-empty broker MessageID survives unchanged through
// the ACL.
func TestASBReceivedToEnvelope_PreservesPopulatedMessageID(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	msg := &azservicebus.ReceivedMessage{
		MessageID: "asb-msg-42",
		Body:      []byte("payload"),
	}
	env, err := receivedToEnvelope(msg, clk)
	if err != nil {
		t.Fatalf("receivedToEnvelope: %v", err)
	}
	if env.ID != "asb-msg-42" {
		t.Errorf("ID = %q, want %q", env.ID, "asb-msg-42")
	}
}

// TestASBReceivedToEnvelope_EmptyMessageIDDoesNotCollapse pins the
// H-3 root regression: 100 empty-ID messages MUST NOT collide on a
// single literal ID.
func TestASBReceivedToEnvelope_EmptyMessageIDDoesNotCollapse(t *testing.T) {
	clk := clocktest.NewAt(time.Now().UTC())
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		env, err := receivedToEnvelope(&azservicebus.ReceivedMessage{Body: []byte("b")}, clk)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if env.ID == "" || env.ID == "test-envelope" {
			t.Fatalf("regression: env.ID = %q", env.ID)
		}
		if _, dup := seen[env.ID]; dup {
			t.Fatalf("duplicate generated ID at iter %d: %q", i, env.ID)
		}
		seen[env.ID] = struct{}{}
	}
}

// TestASBReceivedToEnvelope_CreatedAtFromClock guarantees that the
// envelope's CreatedAt is derived from the injected clock and not
// from time.Now() directly — required so receiver tests stay
// deterministic.
func TestASBReceivedToEnvelope_CreatedAtFromClock(t *testing.T) {
	fixed := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clocktest.NewAt(fixed)
	env, err := receivedToEnvelope(&azservicebus.ReceivedMessage{
		MessageID: "id",
		Body:      []byte("b"),
	}, clk)
	if err != nil {
		t.Fatalf("receivedToEnvelope: %v", err)
	}
	if !env.CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want %v", env.CreatedAt, fixed)
	}
}

// TestASBReceivedToEnvelope_StripsReservedApplicationProperties is
// the security-critical invariant: caller-supplied x-bridge.* keys in
// ApplicationProperties cannot pierce the ACL.
func TestASBReceivedToEnvelope_StripsReservedApplicationProperties(t *testing.T) {
	clk := clocktest.NewAt(time.Now().UTC())
	msg := &azservicebus.ReceivedMessage{
		MessageID: "id",
		Body:      []byte("b"),
		ApplicationProperties: map[string]any{
			messaging.HeaderRouteID:  "forged-route",
			messaging.HeaderTenantID: "forged-tenant",
			"X-Bridge.Causation-Id":  "forged",
			"safe":                   "yes",
		},
	}
	env, err := receivedToEnvelope(msg, clk)
	if err != nil {
		t.Fatalf("receivedToEnvelope: %v", err)
	}
	for key := range env.Headers() {
		if messaging.IsReservedHeader(key) {
			t.Errorf("reserved header survived: %q", key)
		}
	}
	if env.Headers()["safe"] != "yes" {
		t.Errorf("non-reserved header dropped: %v", env.Headers()["safe"])
	}
}

// TestASBWrapEnvelopeErr_ClassifiesInvalidPayload validates the
// classifier maps NewEnvelope's ErrInvalidEnvelopeID to a permanent
// INVALID_PAYLOAD BridgeError so the receive loop drops poison
// messages instead of retrying.
func TestASBWrapEnvelopeErr_ClassifiesInvalidPayload(t *testing.T) {
	be := wrapEnvelopeErr(messaging.ErrInvalidEnvelopeID)
	if be == nil {
		t.Fatal("nil BridgeError")
	}
	if be.Code != shared.ErrCodeInvalidPayload {
		t.Errorf("Code = %q, want %q", be.Code, shared.ErrCodeInvalidPayload)
	}
	if !errors.Is(be, messaging.ErrInvalidEnvelopeID) {
		t.Errorf("error chain lost ErrInvalidEnvelopeID")
	}
}

// TestASBReceivedToEnvelope_StaticSignature locks the (env, error)
// signature added in fix-round 1. A regression that drops the error
// return — re-introducing the silent MustEnvelope masking — will
// fail to compile here.
func TestASBReceivedToEnvelope_StaticSignature(t *testing.T) {
	var (
		_ *messaging.Envelope
		_ error
	)
	env, err := receivedToEnvelope(&azservicebus.ReceivedMessage{
		MessageID: "id",
		Body:      []byte("b"),
	}, clocktest.NewAt(time.Now()))
	if err != nil || env == nil {
		t.Fatalf("unexpected: env=%v err=%v", env, err)
	}
}
