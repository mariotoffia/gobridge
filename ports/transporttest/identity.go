package transporttest

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// SourceIdentity is one adapter's ingress conversion, reduced to the two calls
// the envelope-identity contract is stated in terms of.
//
// An adapter supplies it by driving its OWN transport-native conversion — the
// same code path its Receiver uses to build a Delivery — so the suite asserts
// production behaviour rather than a test double.
type SourceIdentity struct {
	// Redeliver converts THE SAME source message again: the broker redelivering
	// one message after a reconnect, a failover, or an unsettled visibility
	// window. Repeated calls must reproduce the message byte for byte, so any
	// state the transport carries (message id, correlation data, delivery tag)
	// is identical to the previous call.
	Redeliver func() *messaging.Envelope

	// Distinct converts the n-th DISTINCT source message (n is 0-based): a
	// different message from the same source, whatever "different" means for
	// this transport.
	Distinct func(n int) *messaging.Envelope
}

// distinctIdentityCount is how many distinct source messages the uniqueness
// check converts. Large enough that a collapsing derivation (a constant, a
// topic-derived value, a truncated hash) shows up; small enough to stay well
// inside the unit-test budget.
const distinctIdentityCount = 16

// RunSourceIdentityConformanceTests runs the envelope-identity contract
// documented on ports.Receiver against one source adapter's ingress conversion.
//
// It pins the two properties the runtime's dedup and replay cap depend on:
// an identity that is stable across redelivery (or is declared unstable through
// messaging.HeaderGeneratedID), and identities that do not collide between
// distinct messages of one source. An adapter wires it in with, e.g.:
//
//	func TestSQSSourceIdentityConformance(t *testing.T) {
//	    transporttest.RunSourceIdentityConformanceTests(t, newSQSIdentityProbe)
//	}
func RunSourceIdentityConformanceTests(t *testing.T, factory func(t *testing.T) SourceIdentity) {
	t.Helper()

	t.Run("IdentityIsNonEmpty", func(t *testing.T) {
		probe := factory(t)
		if id := probe.Redeliver().ID(); id == "" {
			t.Fatal("Envelope.ID is empty; the outbox and replay ledger key on it")
		}
	})

	t.Run("IdentityIsStableAcrossRedeliveryOrDeclaredGenerated", func(t *testing.T) {
		probe := factory(t)
		first, second := probe.Redeliver(), probe.Redeliver()

		if first.ID() == second.ID() {
			return
		}
		// An adapter-minted identity is allowed to change per redelivery ONLY
		// while saying so: the marker is what makes the runtime's replay cap
		// terminate the message instead of looping on a fresh key every time.
		if _, declared := messaging.GetHeaderString(first.Headers(), messaging.HeaderGeneratedID); !declared {
			t.Fatalf("redelivery of one source message produced ids %q then %q without stamping %s; "+
				"dedup and the replay cap both key on a stable identity",
				first.ID(), second.ID(), messaging.HeaderGeneratedID)
		}
	})

	t.Run("DistinctMessagesGetDistinctIdentities", func(t *testing.T) {
		probe := factory(t)
		seen := make(map[string]int, distinctIdentityCount)
		for n := range distinctIdentityCount {
			id := probe.Distinct(n).ID()
			if id == "" {
				t.Fatalf("distinct source message %d produced an empty Envelope.ID", n)
			}
			if previous, collide := seen[id]; collide {
				t.Fatalf("distinct source messages %d and %d share Envelope.ID %q; "+
					"the outbox holds one record per identity, so the second message is suppressed",
					previous, n, id)
			}
			seen[id] = n
		}
	})
}
