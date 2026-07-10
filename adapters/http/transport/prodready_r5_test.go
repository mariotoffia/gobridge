package transport_test

// Black-box tests for R5 issue 4: programmatic configs passed straight to
// the factory's NewReceiver/NewSender must be run through Config.Validate,
// not just the YAML decode path. Before the fix a caller could hand a
// contradictory or out-of-range Config to the constructor and have it
// silently accepted, letting effectiveAcceptZeroDeliveryLoss pick a
// behavior Validate was written to reject.

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A Config that both demands a transient error on zero delivery AND opts
// into accepting the loss is self-contradictory; Validate rejects it. The
// programmatic NewSender path must enforce that, not just the YAML loader.
func TestNewSender_RejectsContradictoryZeroDeliveryConfig(t *testing.T) {
	_, err := transport.NewFactory().NewSender(context.Background(), ports.SenderSpec{
		ID: "contradiction",
		Config: transport.Config{
			Mode:                 "sse",
			FailOnZeroDelivery:   true,
			AtMostOnceAcceptLoss: true,
		},
	}, nil)
	if err == nil {
		t.Fatal("NewSender must reject fail_on_zero_delivery + at_most_once_accept_loss, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error must name the mutual-exclusion contradiction, got %v", err)
	}
}

// The non-contradictory opt-in must still build — the guard rejects only
// the contradiction, not a lone accept-loss.
func TestNewSender_AcceptsLoneAcceptLoss(t *testing.T) {
	s, err := transport.NewFactory().NewSender(context.Background(), ports.SenderSpec{
		ID:     "lone-accept-loss",
		Config: transport.Config{Mode: "sse", AtMostOnceAcceptLoss: true},
	}, nil)
	if err != nil {
		t.Fatalf("a lone at_most_once_accept_loss is valid, got %v", err)
	}
	if s == nil {
		t.Fatal("expected a sender")
	}
}

// The programmatic NewReceiver path must validate too: an inline API key
// below the 16-char security floor is rejected at build time rather than
// silently accepted (it would have bypassed the YAML loader's Validate).
func TestNewReceiver_ValidatesProgrammaticConfig(t *testing.T) {
	_, err := transport.NewFactory().NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "short-key",
		Config: transport.Config{APIKey: shared.NewSecret("short")},
	}, nil)
	if err == nil {
		t.Fatal("NewReceiver must reject an inline api_key below the 16-char floor, got nil")
	}
	if !strings.Contains(err.Error(), "api_key is too short") {
		t.Fatalf("error must name the too-short api_key, got %v", err)
	}
}
