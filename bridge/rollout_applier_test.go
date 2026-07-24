package bridge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestVerifyCandidateDigest_MatchAndTamper validates F10: a node verifies the
// candidate bytes it fetched against the digest recorded in the rollout row
// before building. Untampered bytes pass; a single changed byte fails, so the
// node will Nack rather than build a substituted config.
func TestVerifyCandidateDigest_MatchAndTamper(t *testing.T) {
	raw := []byte("bridge:\n  id: demo\n  deployment_mode: clustered\n")
	digest := candidateConfigDigest(raw)
	require.NotEmpty(t, digest)

	require.NoError(t, verifyCandidateDigest(raw, digest))

	tampered := []byte("bridge:\n  id: evil\n  deployment_mode: clustered\n")
	require.Error(t, verifyCandidateDigest(tampered, digest), "tampered bytes must fail the digest check")
}

// TestVerifyCandidateDigest_RejectsEmptyExpected validates that a missing
// expected digest is a hard failure, not a pass — a rollout row without a digest
// must never be treated as verified.
func TestVerifyCandidateDigest_RejectsEmptyExpected(t *testing.T) {
	require.Error(t, verifyCandidateDigest([]byte("anything"), ""))
}

// TestCandidateConfigDigest_Deterministic validates the digest is a pure
// function of the bytes — the proposer and every applier must derive the same
// digest from the same artifact for the barrier to mean anything.
func TestCandidateConfigDigest_Deterministic(t *testing.T) {
	raw := []byte("some canonical config bytes")
	require.Equal(t, candidateConfigDigest(raw), candidateConfigDigest(raw))
	require.NotEqual(t, candidateConfigDigest(raw), candidateConfigDigest([]byte("other")))
}

// TestNodeRolloutGate_AdmitsNewerRejectsStale validates the node's high-water
// guard (research §3): a node applies each generation once and rejects anything
// not strictly newer. This makes a deposed coordinator's late push of an older
// generation a no-op at the node, not just at the store, and makes a re-fired
// notification for an already-applied generation idempotent.
func TestNodeRolloutGate_AdmitsNewerRejectsStale(t *testing.T) {
	var g nodeRolloutGate

	require.True(t, g.admits(1), "a fresh node admits the first generation")

	g.record(5)
	require.True(t, g.admits(6), "a strictly newer generation is admitted")
	require.False(t, g.admits(5), "the already-applied generation is idempotently skipped")
	require.False(t, g.admits(4), "a deposed coordinator's late push of an older gen is rejected")
}

// TestNodeRolloutGate_RecordIsMonotonic validates that recording an older
// generation never lowers the high-water — a late push cannot rewind the node.
func TestNodeRolloutGate_RecordIsMonotonic(t *testing.T) {
	var g nodeRolloutGate
	g.record(5)
	g.record(3) // stale
	require.False(t, g.admits(4), "high-water must not have been rewound to 3")
	require.True(t, g.admits(6))
}

// TestNodeRolloutGate_F7ReadoptAfterCrashBeforeSwap validates F7: a node that
// committed generation N but crashed before swapping recorded no high-water for
// N (recording happens after the swap completes). On rejoin its high-water is
// still N-1, so it re-adopts the committed generation N rather than rejecting it.
func TestNodeRolloutGate_F7ReadoptAfterCrashBeforeSwap(t *testing.T) {
	var g nodeRolloutGate
	g.record(4) // last generation fully applied (swapped) before the crash

	// Committed generation is 5; the node crashed after commit, before its swap,
	// so it never recorded 5. On rejoin it must admit (re-adopt) 5.
	require.True(t, g.admits(5), "a crash before swap must leave gen N re-adoptable on rejoin")
}

// TestEvaluateProposal_AckableWhenLiveSafeAndDigestMatches validates the happy
// pre-build gate: a live-safe delta whose fetched bytes match the recorded
// digest is ackable (empty nack reason) and the node proceeds to build.
func TestEvaluateProposal_AckableWhenLiveSafeAndDigestMatches(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg.Bindings[0].Address = "topic/changed" // benign, live-safe
	bytes := []byte("candidate-artifact-bytes")

	reason := evaluateProposal(oldCfg, candidateCfg, bytes, candidateConfigDigest(bytes))

	require.Empty(t, reason)
}

// TestEvaluateProposal_NackOnDigestMismatch validates F10: mismatched bytes are
// Nacked before any classification or build.
func TestEvaluateProposal_NackOnDigestMismatch(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg.Bindings[0].Address = "topic/changed"

	reason := evaluateProposal(oldCfg, candidateCfg, []byte("tampered"), candidateConfigDigest([]byte("original")))

	require.NotEmpty(t, reason)
}

// TestEvaluateProposal_NackOnReplacementRequired validates §8: a
// replacement-required delta is Nacked with the class reason even when the
// bytes verify — ADR 0012's whole-cohort replacement still governs.
func TestEvaluateProposal_NackOnReplacementRequired(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg.Stores.Lease = &ports.StoreConfig{Type: "dynamodb"} // store target change
	bytes := []byte("candidate-artifact-bytes")

	reason := evaluateProposal(oldCfg, candidateCfg, bytes, candidateConfigDigest(bytes))

	require.NotEmpty(t, reason)
}

// TestEvaluateProposal_DigestCheckedBeforeClassify pins the safety ordering:
// when the bytes are tampered AND the delta is replacement-required, the digest
// failure wins — untrusted bytes must never be parsed and classified.
func TestEvaluateProposal_DigestCheckedBeforeClassify(t *testing.T) {
	oldCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg := supervisorTestConfigWithSession("r1", "sess")
	candidateCfg.Stores.Lease = &ports.StoreConfig{Type: "dynamodb"}

	reason := evaluateProposal(oldCfg, candidateCfg, []byte("tampered"), candidateConfigDigest([]byte("original")))

	require.Contains(t, reason, "digest", "digest failure must take precedence over classification")
}
