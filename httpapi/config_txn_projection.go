package httpapi

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
)

// A committed config must be the document, not a value only this process holds.
//
// The merged value a transaction produces is the running config with the
// overlay's JSON laid over it. That JSON cannot carry a single typed plugin
// option — the blueprint def types tag their Config `json:"-"` — so an entry the
// overlay names arrives with no decoded options at all, while the SAME entry read
// back from the document the commit wrote is decoded through the registry and
// carries the adapter's own defaults. The two values describe the same
// configuration and are not equal.
//
// On one node that difference is invisible, because a config source watcher
// re-emits the document a moment later and the runtime converges. In a cluster it
// is not. A coordinated cohort identifies a change by the canonical digest of the
// config, so a committing member that proposes the merged value proposes an
// identity no peer can reproduce: every peer loads the document, computes the
// other digest, finds a rollout in flight carrying a digest that is not its own,
// and refuses to join it. The rollout then deadline-aborts holding exactly one
// acknowledgement — the proposer's — and an operator sees a change that nobody
// applied and no member nacked.
//
// So the commit applies what it wrote.

// projectionOf returns committed as the config source itself produces it, or
// committed unchanged when that reading is not available or does not describe
// this commit. Falling back is safe: it is the value the commit would have
// applied anyway, and the config source's own watcher converges the runtime.
func (m *configTxnManager) projectionOf(ctx context.Context, committed *ports.BridgeConfig) *ports.BridgeConfig {
	projected, err := m.store.Load(ctx)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("config commit: the committed document could not be read back, so the merged "+
				"value is applied instead; in a coordinated cluster this member may propose a candidate "+
				"identity its peers cannot reproduce",
				"version", committed.Version, "error", err)
		}
		return committed
	}
	if projected == nil || projected.Version != committed.Version {
		// A concurrent writer advanced the shared document between this commit's
		// write and this read. That newer generation is not this transaction's to
		// apply — its own committer applies it, and this member's config source
		// watcher observes it — so apply what was committed here and let the
		// versions resolve through the normal path rather than skipping a
		// generation from inside a commit.
		if m.logger != nil {
			m.logger.Warn("config commit: the document read back is not the version just committed, so "+
				"the merged value is applied instead",
				"committed_version", committed.Version, "document_version", documentVersion(projected))
		}
		return committed
	}
	return projected
}

// documentVersion reports a config's version, or 0 when there is no config, so
// the log line above never has to guard a nil.
func documentVersion(cfg *ports.BridgeConfig) int {
	if cfg == nil {
		return 0
	}
	return cfg.Version
}
