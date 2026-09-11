//go:build !race

package validation_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
)

// Test_TierB_Validation_UnknownSQSQueue covers matrix row 11.
// Matrix wording:
//
//	"SQS queue 'X' is not in Queues;
//	 add Queues[\"X\"] = queue"
func Test_TierB_Validation_UnknownSQSQueue(t *testing.T) {
	stack := newStack(t)
	cfg := sqsReceiverNamed("orders-in")
	reg := registry.NewQueueRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got,
		"\"orders-in\"",
		"is not in Queues",
		"Queues[\"orders-in\"]",
	) {
		t.Fatalf("missing matrix row 11 substrings; got: %v", got)
	}
}

// Test_TierB_Validation_UnknownSSMParameter covers matrix row 12.
// Matrix wording:
//
//	"yaml references SSM parameter path '/path' but it is not in Secrets.
//	 Fix: add Secrets[\"/path\"] = param"
func Test_TierB_Validation_UnknownSSMParameter(t *testing.T) {
	stack := newStack(t)
	cfg := ssmReceiverWithCreds("pms://bridge/missing")
	reg := registry.NewSsmParamRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, SsmParamRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got,
		"\"/bridge/missing\"",
		"is not in Secrets",
		"Secrets[\"/bridge/missing\"]",
	) {
		t.Fatalf("missing matrix row 12 substrings; got: %v", got)
	}
	for _, m := range got {
		if !strings.Contains(m, "Secrets[") {
			continue
		}
		if !strings.Contains(m, "/bridge/missing") {
			t.Fatalf("Secrets hint must echo the path; got: %s", m)
		}
	}
}
