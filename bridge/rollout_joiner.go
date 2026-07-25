package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The joiner rule (design §6): "a starting member adopts only the last Committed
// configuration; it never acks a rollout proposed before it joined".
//
// It exists because the candidate travels through each member's OWN config
// source (see the transport note in rollout_applier.go). The operator's change
// is durably written to that source BEFORE the barrier decides, so between the
// write and the decision the source's current document is a config the cohort
// has NOT agreed to run. Without this gate a member restarting in that window —
// or at any time after an ABORTED rollout, since the rejected document stays in
// the source until the operator rolls it back — would boot straight onto a
// config no other member is running. That is the mixed-version cohort goal G2
// forbids and, since ADR 0012 refuses clustered live reload outright today, it
// would be a safety REGRESSION rather than a limitation.
//
// The rule is deliberately narrow: it refuses only what the barrier has
// explicitly not-committed. In particular it does NOT require the boot config to
// equal the last committed digest, because the whole-cohort replacement
// procedure — still the mandated path for replacement-required deltas (§8) —
// legitimately boots every member onto a config no rollout ever carried.

// checkCoordinatedRolloutPreflight is the startup gate for a coordinated
// clustered deployment with a rollout store wired; every other shape is
// unaffected and returns nil. It runs before anything is built, so a
// misconfigured or unsafe boot fails loudly at startup rather than at the first
// live reload — which is to say, during a change, at night.
func (s *Supervisor) checkCoordinatedRolloutPreflight(ctx context.Context, cfg *ports.BridgeConfig) error {
	if s.rollout == nil || !coordinatedRollout(cfg) {
		return nil
	}
	if err := s.checkRolloutMembership(cfg); err != nil {
		return err
	}
	return s.checkRolloutJoinerRule(ctx, cfg)
}

// checkRolloutMembership requires this node's barrier member id to appear in the
// cohort roster. The blueprint validator cannot check this — the member id is
// composition-root wiring, not config — so admission happens here.
//
// A node outside its own roster can never Ack (the aggregate rejects a voter
// outside the frozen epoch), so every rollout it proposes is guaranteed to
// deadline-abort while blocking every other proposal for the whole TTL
// (invariant I1 permits one active rollout). Catching it at boot turns a 3am
// discovery into a startup failure.
func (s *Supervisor) checkRolloutMembership(cfg *ports.BridgeConfig) error {
	members := rolloutMembers(cfg)
	if len(members) == 0 {
		return fmt.Errorf("bridge: cluster.rollout: coordinated requires a non-empty "+
			"bridge.cluster.members roster — it is the membership epoch the rollout barrier freezes "+
			"and counts acknowledgements against, and an empty roster would let the barrier commit "+
			"with ZERO acknowledgements. Refusing to start (config_version=%d)", cfg.Version)
	}
	if !slices.Contains(members, s.rollout.memberID) {
		return fmt.Errorf("bridge: this node's rollout member id %q does not appear in the cohort "+
			"roster bridge.cluster.members %v, so it could never acknowledge a rollout and every "+
			"change it proposes would deadline-abort while blocking the cohort's next change for the "+
			"whole rollout TTL. Align bridge.WithClusterRollout's MemberID with the roster. Refusing "+
			"to start (config_version=%d)", s.rollout.memberID, members, cfg.Version)
	}
	return nil
}

// checkRolloutJoinerRule reports whether this node may boot on cfg.
//
// It fails CLOSED on a store error. The rollout store is a hard requirement of
// cluster.rollout: coordinated (a member cannot participate in the barrier
// without it), so an unreadable store means this node cannot tell a committed
// config from a rejected one — and booting on the wrong one splits the cohort.
func (s *Supervisor) checkRolloutJoinerRule(ctx context.Context, cfg *ports.BridgeConfig) error {
	r, err := s.rollout.store.Current(ctx)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return nil // no rollout has ever been proposed: nothing has been rejected
		}
		return fmt.Errorf("bridge: cluster.rollout: coordinated requires the rollout store to be "+
			"readable at startup so this node can tell a cohort-committed config from one the cohort "+
			"rejected; booting without that check could start this node on a config no other member "+
			"is running (a mixed-version cohort). Refusing to start (config_version=%d): %w",
			cfg.Version, err)
	}
	digest, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		return fmt.Errorf("bridge: cannot compute the boot config digest to check it against the " +
			"cluster rollout barrier; refusing to start")
	}
	if r.ConfigDigest() != digest {
		// The boot config is not the one this rollout carries, so this rollout
		// says nothing about it. An in-flight rollout for a DIFFERENT candidate
		// is the normal case for a member restarting mid-rollout: it boots the
		// old committed config and votes on the candidate through the applier.
		s.stageBootConfig(cfg, digest)
		return nil
	}
	switch r.State() {
	case persistence.RolloutCommitted:
		return nil // the cohort agreed to run exactly this config
	case persistence.RolloutAborted:
		return fmt.Errorf("bridge: refusing to start on a config the cluster rollout barrier ABORTED "+
			"(generation=%d config_version=%d reason=%q). The cohort is running the previous config, so "+
			"starting on this one would split the cluster across config versions. Roll the config source "+
			"back to the last committed document (or fix and re-propose the change), then start this "+
			"node; see docs/runbooks/cluster-config-rollout.md",
			r.Generation(), r.ConfigVersion(), r.Reason())
	default:
		return fmt.Errorf("bridge: refusing to start on a config whose cluster rollout is still "+
			"UNDECIDED (generation=%d state=%q config_version=%d). Starting now would apply the "+
			"candidate ahead of the all-member commit barrier — exactly the mixed-version cohort the "+
			"barrier prevents. Wait for the rollout to commit or abort, then start this node; "+
			"see docs/runbooks/cluster-config-rollout.md",
			r.Generation(), r.State(), r.ConfigVersion())
	}
}

// stageBootConfig stages the config this node booted on as its candidate.
//
// It exists because staging otherwise happens ONLY in the proposer path
// (rolloutBarrier.propose), which a booting member never runs: it receives its
// config as Run's `initial`, not as a change through Supervisor.apply. Without
// this, a member that restarted onto a document its peers have not yet proposed
// could never vote on the rollout that later carries that exact document — its
// own watcher will not re-deliver unchanged content, so the candidate would
// never be staged and every such rollout would deadline-abort with that member
// permanently silent.
//
// Staging is safe here for the same reason it is safe in the proposer: the
// content came through this node's own validated config-source load path, and
// the applier still verifies the digest and classifies the delta before voting.
func (s *Supervisor) stageBootConfig(cfg *ports.BridgeConfig, digest string) {
	frozen, err := cloneConfigForBuild(cfg)
	if err != nil {
		return // unfreezable config; the boot itself will fail on it shortly
	}
	s.rollout.stage(digest, frozen, cfg)
}

// configCanonicalBytesDigest is the candidate digest of cfg, or ok=false when
// cfg cannot be canonicalised (the caller must then fail closed).
func configCanonicalBytesDigest(cfg *ports.BridgeConfig) (string, bool) {
	raw, ok := configCanonicalBytes(cfg)
	if !ok {
		return "", false
	}
	return candidateConfigDigest(raw), true
}
