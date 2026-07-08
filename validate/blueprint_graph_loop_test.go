package validate_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

// TestValidateBlueprintGraph_SameSessionLoopWarns covers the Chunk-1 finding
// that a same-broker feedback loop (a binding publishing to an address a
// receiver on the same session subscribes to) was not surfaced. The static
// exact-match case must now emit a warning.
func TestValidateBlueprintGraph_SameSessionLoopWarns(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Sessions: []ports.SessionDef{
			{ID: "sess1", Transport: "mqtt"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", SessionID: "sess1", Topics: []ports.SubscriptionDef{{Topic: "loop/topic"}}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", SessionID: "sess1"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", SessionID: "sess1", Address: "loop/topic"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx1", DeliveryMode: "direct_hold", Bindings: []string{"b1"}},
		},
	}

	res := validate.ValidateBlueprintGraph(cfg)
	if res == nil {
		t.Fatal("expected non-nil result carrying the loop warning")
	}
	if res.HasErrors() {
		t.Fatalf("expected no errors, got: %v", res.Error())
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "feedback loop") {
		t.Fatalf("expected same-session feedback-loop warning, got: %v", res.Warnings)
	}
}

// TestValidateBlueprintGraph_NoLoopWhenAddressesDiffer guards against false
// positives: a distinct send address on the same session must not warn, and a
// matching address on a DIFFERENT session must not warn either.
func TestValidateBlueprintGraph_NoLoopWhenAddressesDiffer(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Sessions: []ports.SessionDef{
			{ID: "sess1", Transport: "mqtt"},
			{ID: "sess2", Transport: "mqtt"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", SessionID: "sess1", Topics: []ports.SubscriptionDef{{Topic: "in/topic"}}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", SessionID: "sess1"}, // same session, different address
			{ID: "tx2", SessionID: "sess2"}, // different session, same address
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", SessionID: "sess1", Address: "out/topic"},
			{ID: "b2", SenderID: "tx2", SessionID: "sess2", Address: "in/topic"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx1", DeliveryMode: "shared_outbox", Bindings: []string{"b1", "b2"}},
		},
	}

	res := validate.ValidateBlueprintGraph(cfg)
	warnings := ""
	if res != nil {
		warnings = strings.Join(res.Warnings, "\n")
	}
	if strings.Contains(warnings, "feedback loop") {
		t.Fatalf("did not expect a feedback-loop warning, got: %v", warnings)
	}
}
