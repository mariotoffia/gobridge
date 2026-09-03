package bridge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// What a node admits, and what it checks that against: the pre-build proposal
// gate, the candidate digest that is the cohort's agreement primitive, and the
// local generation high-water mark. Split out of rollout_applier.go, which owns
// the observe/vote/adopt drive itself.

// evaluateProposal runs the node's pre-build applier gate for an observed
// rollout (ADR 0013). It verifies the candidate against the recorded digest
// FIRST, then classifies the delta (§8), returning an empty string when
// the node may build and Ack or a non-empty Nack reason otherwise. The build
// (and the Ack carrying its build digest) is the caller's job, performed only
// when the reason is empty.
func evaluateProposal(oldCfg, candidateCfg *ports.BridgeConfig, candidateBytes []byte, expectedDigest string) string {
	if err := verifyCandidateDigest(candidateBytes, expectedDigest); err != nil {
		return err.Error()
	}
	if class, reason := classifyRolloutDelta(oldCfg, candidateCfg); class == rolloutReplacementRequired {
		return reason
	}
	return ""
}

// nodeRolloutGate is a node's local high-water mark over rollout generations
// (research §3: "each node is itself a token-checking resource"). Generations
// are globally monotonic — there is one active rollout at a time — so a single
// generation counter is a total order and subsumes the coordinator fencing epoch
// (already enforced at the store).
//
// It is deliberately IN-MEMORY ONLY. A durable high-water was considered (design
// §11 Phase 4) and is not needed: across a restart the same guarantee is
// reconstructed from state that is already durable and already authoritative —
//
//   - the store admits exactly one active rollout and hands back only the
//     newest generation through Current, so there is no channel by which a
//     STALE generation could reach a restarted node at all;
//   - Ack/Commit against a non-current generation is rejected by the store
//     (ErrNotFound), so a late vote cannot land either;
//   - re-adoption of an ALREADY-applied generation is caught by content, not by
//     a counter: rolloutApplier.adopt compares the committed candidate against
//     the running config and records the generation without swapping when they
//     match (see there).
//
// A durable counter would therefore add a per-node write path and its failure
// modes while removing no reachable failure. The in-memory mark still does real
// work WITHIN a process lifetime: it makes repeated observations of the same
// committed generation idempotent without re-deriving content equality.
type nodeRolloutGate struct {
	applied uint64 // highest fully-applied generation; 0 = none yet
}

// admits reports whether gen is strictly newer than the highest generation this
// node has applied. A stale generation (a deposed coordinator's late push) and
// an already-applied generation (a re-fired notification) are both rejected, so
// application is idempotent and never rewinds.
func (g *nodeRolloutGate) admits(gen uint64) bool {
	return gen > g.applied
}

// record advances the high-water mark to gen once the node has fully applied it.
// It is monotonic: a stale generation never lowers the mark.
func (g *nodeRolloutGate) record(gen uint64) {
	if gen > g.applied {
		g.applied = gen
	}
}

// candidateConfigDigest is the canonical digest of a candidate config artifact:
// the hex-encoded SHA-256 of its exact bytes. The proposer stamps a rollout row
// with this digest; every applier recomputes it over the candidate its own
// config source delivered and compares.
//
// Determinism across members (ADR 0013) holds because the
// digest input, configCanonicalBytes, is a pure function of the config document:
// it JSON-encodes shared.RevealSecrets(cfg), and a shared.Secret is a literal
// value carried in the config bytes — GoBridge performs NO per-node
// interpolation (no env expansion, no lazy secret resolution) on the config load
// path, so two members loading the same document canonicalise identically. The
// remaining input is the plugin registry, which decodes the typed plugin
// payloads; a cohort whose members register different plugin sets would diverge
// here, and does so LOUDLY (a Nack naming the digest mismatch) rather than
// silently. TestCandidateConfigDigest_IsStableAcrossIndependentLoads pins the
// property; UC-CR7 (Phase 5) proves it across real processes.
func candidateConfigDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// verifyCandidateDigest checks a candidate against the digest recorded in the
// rollout row. A mismatch — a divergent config source, a
// superseded artifact — or an empty expected digest is an error: the node must
// Nack rather than build a candidate the cohort did not agree on. The compare is
// constant-time; the inputs are not secret, but it costs nothing and avoids a
// length/early-exit oracle.
func verifyCandidateDigest(raw []byte, expected string) error {
	if expected == "" {
		return shared.ErrRolloutDigestMismatch.WithMessage("rollout row carries no candidate digest to verify against")
	}
	got := candidateConfigDigest(raw)
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return shared.ErrRolloutDigestMismatch.
			WithMessage("candidate config digest mismatch").
			With("expected", expected).With("got", got)
	}
	return nil
}
