//go:build !race

package validation_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

// Test_TierB_Validation_UnknownSQSQueue covers matrix row 11.
// Matrix wording:
//
//	"yaml references SQS queue 'X' but no such entry in QueueRegistry.
//	 Add: registry.AddQueue(\"X\", queue)"
func Test_TierB_Validation_UnknownSQSQueue(t *testing.T) {
	stack := newStack(t)
	cfg := sqsReceiverNamed("orders-in")
	reg := registry.NewQueueRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got,
		"\"orders-in\"",
		"no such entry in QueueRegistry",
		"AddQueue(\"orders-in\"",
	) {
		t.Fatalf("missing matrix row 11 substrings; got: %v", got)
	}
}

// Test_TierB_Validation_UnknownSSMParameter covers matrix row 12.
// Matrix wording:
//
//	"yaml references SSM parameter '/path' but no such entry in
//	 SsmParamRegistry. Add: registry.AddParameter(\"/path\", param)"
func Test_TierB_Validation_UnknownSSMParameter(t *testing.T) {
	stack := newStack(t)
	cfg := ssmReceiverWithCreds("pms://bridge/missing")
	reg := registry.NewSsmParamRegistry()

	validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, SsmParamRegistry: reg})

	got := errorMessages(t, stack)
	if !containsAll(t, got,
		"\"/bridge/missing\"",
		"no such entry in SsmParamRegistry",
		"AddParameter(\"/bridge/missing\"",
	) {
		t.Fatalf("missing matrix row 12 substrings; got: %v", got)
	}
	for _, m := range got {
		if !strings.Contains(m, "AddParameter") {
			continue
		}
		if !strings.Contains(m, "/bridge/missing") {
			t.Fatalf("AddParameter hint must echo the path; got: %s", m)
		}
	}
}
