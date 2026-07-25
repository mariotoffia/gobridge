package persistence

import "github.com/mariotoffia/gobridge/domain/shared"

// CommittedRolloutConfig is the cohort's durable last-committed configuration
// artifact: the exact config bytes the cohort last committed through the barrier
// (or the pre-rollout baseline seed), plus the generation and version that
// produced them. It exists to answer, durably and independent of whatever
// volatile candidate the config source's `current` slot may hold, "what config
// should a member actually run?" — the missing piece flagged in the rollout
// design's Phase-4 residual.
//
// It is the artifact behind option (a): a member fetches these BYTES rather than
// re-deriving them, so a member that (re)joins after a restart boots on the
// committed config (closing the restart-into-window and restart-after-abort
// residuals), and a member that missed a commit reconciles to it (closing the
// commit-overwritten window). Generation is monotonic — 0 is the baseline seed,
// a real commit uses the rollout generation — and Digest lets a reader verify
// the bytes it fetched (it recomputes the digest and compares, F10-style).
//
// It is a flat data-transfer value (like RolloutSnapshot), not an aggregate: the
// store owns how it maps to durable attributes; Validate is the shared
// fail-closed guard both the write path (reject a malformed artifact) and the
// read path (reject a corrupt/truncated row) run.
type CommittedRolloutConfig struct {
	// Generation is the rollout generation whose commit produced this artifact;
	// 0 is the pre-rollout baseline seed. Monotonic: a Put only advances it.
	Generation uint64
	// ConfigVersion is the committed config's version.
	ConfigVersion int
	// ConfigBytes is the exact committed config document a member boots on.
	ConfigBytes []byte
	// Digest is the reader-verifiable digest of ConfigBytes (hex SHA-256 of the
	// canonical bytes, as the applier computes it).
	Digest string
}

// Validate fails closed on an artifact that could not be a config to run:
// missing bytes (a member would build nothing) or a missing digest (a reader
// could not verify the bytes it fetched). Generation 0 is the valid baseline
// seed, so generation is intentionally unchecked.
func (c CommittedRolloutConfig) Validate() *shared.BridgeError {
	if len(c.ConfigBytes) == 0 {
		return shared.ErrInvalidConfig.WithMessage("committed rollout config has no bytes")
	}
	if c.Digest == "" {
		return shared.ErrInvalidConfig.WithMessage("committed rollout config has no digest")
	}
	if c.ConfigVersion < 0 {
		// A negative version is nonsense the version-gated boot resolution compares
		// against; reject it so a buggy producer cannot persist an unusable artifact.
		return shared.ErrInvalidConfig.WithMessage("committed rollout config version is negative").
			With("version", c.ConfigVersion)
	}
	return nil
}
