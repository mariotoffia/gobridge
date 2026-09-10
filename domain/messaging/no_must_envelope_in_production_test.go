package messaging_test

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// The rule — production code must use messaging.NewEnvelope and
// surface the error so caller-supplied input can be classified into a
// BridgeError, rather than panicking or silently substituting a
// placeholder ID — is enforced by the forbidigo rule
// '^messaging\.MustEnvelope(WithReserved)?$' in .golangci.yml, not by a
// test. `make lint` names the offending file and line directly.
//
// The exemptions are the forbidigo path rules already in that file:
// _test.go, ports/storetest/ and ports/transporttest/ (the conformance
// suites, which build known-valid fixtures and import "testing" despite
// lacking the _test.go suffix).
//
// What remains here is the part a linter cannot check: the runtime
// BEHAVIOUR of the two constructors.

// TestNewEnvelopeReturnsError pins the public API contract: the
// production-safe constructor must return (*Envelope, error). A
// regression that swaps it back to a panic-on-empty signature will
// fail to compile here.
func TestNewEnvelopeReturnsError(t *testing.T) {
	env, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:      "id",
		Subject: "s",
	}, time.Now())
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env == nil || env.ID() != "id" {
		t.Fatalf("unexpected env: %+v", env)
	}

	// Empty ID MUST surface as an error, not a panic and not a
	// literal substitution.
	_, err = messaging.NewEnvelope(messaging.EnvelopeInput{Subject: "s"}, time.Now())
	if err == nil {
		t.Fatal("NewEnvelope with empty ID should error, got nil")
	}
}

// TestMustEnvelopeProducesUniqueFallbackIDs guards the regression
// at the helper level: even MustEnvelope must NOT collapse empty IDs
// onto a single literal. The compromise (counter-based fallback)
// keeps each invocation distinct so test fixtures can never mask
// production bugs by accidentally reusing the same envelope ID.
func TestMustEnvelopeProducesUniqueFallbackIDs(t *testing.T) {
	seen := map[string]struct{}{}
	for i := range 50 {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "s"})
		if env.ID() == "" || env.ID() == "test-envelope" {
			t.Fatalf("regression: env.ID = %q", env.ID())
		}
		if _, dup := seen[env.ID()]; dup {
			t.Fatalf("duplicate fallback ID at iter %d: %q", i, env.ID())
		}
		seen[env.ID()] = struct{}{}
	}
}
