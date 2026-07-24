package bridge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// evaluateProposal runs the node's pre-build applier gate for an observed
// rollout (design §6). It verifies the fetched candidate bytes against the
// recorded digest FIRST (F10), then classifies the delta (§8), returning an
// empty string when the node may build and Ack or a non-empty Nack reason
// otherwise. The build (and the Ack carrying its build digest) is the caller's
// job, performed only when the reason is empty.
//
// candidateCfg is the parsed form of candidateBytes; the parse necessarily ran
// in the caller. To keep hostile bytes from ever being decoded, the Phase-4
// applier MUST recompute and verify candidateConfigDigest on the raw bytes
// BEFORE it YAML-decodes them; the digest check here is then a redundant
// backstop, not the sole guard against decoding tampered input.
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
// (already enforced at the store, I3). The node records a generation only AFTER
// it has fully applied it (swap complete), so a crash between commit and swap
// (F7) leaves the generation re-adoptable on rejoin.
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
// with this digest; every applier recomputes it over the bytes it fetched and
// compares (F10). Both sides fetch the same immutable config-source row, so the
// bytes — and therefore the digest — are identical when untampered.
func candidateConfigDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// verifyCandidateDigest checks fetched candidate bytes against the digest
// recorded in the rollout row (invariant behind F10). A mismatch — tampering, a
// truncated fetch, or a superseded artifact — or an empty expected digest is an
// error: the node must Nack rather than build unverified bytes. The compare is
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
