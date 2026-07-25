package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Coordinated cluster rollout — the proposer half of the barrier
// (design cluster-config-rollout-protocol.md §6).
//
// A clustered node refuses every live config delta by default (ADR 0012). When
// the deployment opts into cluster.rollout: coordinated AND the delta is
// live-safe (§8), the delta is instead PROPOSED to the shared rollout store as a
// candidate generation with a frozen membership epoch, and the local apply is
// DEFERRED. No member swaps until the lease-elected coordinator observes acks
// covering the epoch and flips the rollout to Committed — that store-atomic flip
// is what keeps the cohort from running mixed versions.
//
// Every member proposes the delta it received from its own config source; the
// first Propose wins the conditional create (I1) and the rest join the same
// generation. The digest recorded in the rollout row is the cross-member
// agreement check: a member whose config source handed it different bytes
// computes a different digest, cannot verify the proposal, and Nacks (F10) —
// so a divergent config aborts the rollout instead of splitting the cohort.

const (
	// defaultRolloutTTL bounds how long a proposed rollout may stay unresolved
	// before the coordinator aborts it (F1). The design's sizing (§13 Q2) is
	// 2 × convergence budget floor + member build budget ≈ 5 minutes; NETCONF's
	// confirmed-commit default (600 s) is the same order of magnitude.
	defaultRolloutTTL = 5 * time.Minute
)

// ClusterRolloutConfig wires the coordinated rollout barrier into a Supervisor.
// It is opt-in: without it a clustered live reload keeps the ADR 0012 refusal,
// so an operator who has not configured the barrier sees no behavior change.
type ClusterRolloutConfig struct {
	// Store is the shared, conditional-write rollout store every member
	// coordinates through. Required.
	Store ports.ClusterRolloutStore

	// MemberID identifies this node within the membership epoch. It MUST be
	// stable for the node and distinct across the cohort — decideRollout
	// compares live membership against the frozen epoch by set equality, so a
	// duplicate or drifting ID aborts every rollout. Required.
	MemberID string

	// Membership returns the current cohort member IDs. It MUST return the same
	// set on every member and MUST NOT include duplicates. When nil, the
	// membership is derived from the applied config's bridge.cluster.endpoints.
	Membership func() []string

	// TTL bounds an unresolved rollout before the coordinator aborts it.
	// Zero selects defaultRolloutTTL.
	TTL time.Duration
}

// rolloutBarrier is the per-Supervisor proposer state.
type rolloutBarrier struct {
	store      ports.ClusterRolloutStore
	memberID   string
	membership func() []string
	ttl        time.Duration
}

// newRolloutBarrier validates the wiring and applies defaults. It returns nil
// when the configuration is incomplete, so a half-wired barrier degrades to the
// ADR 0012 refusal (fail closed) rather than silently proposing nothing.
func newRolloutBarrier(cfg ClusterRolloutConfig) *rolloutBarrier {
	if cfg.Store == nil || cfg.MemberID == "" {
		return nil
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultRolloutTTL
	}
	return &rolloutBarrier{
		store:      cfg.Store,
		memberID:   cfg.MemberID,
		membership: cfg.Membership,
		ttl:        ttl,
	}
}

// members resolves the membership epoch for a proposal. An explicit Membership
// function wins; otherwise the static bridge.cluster.endpoints keys of the
// applied config are used (the validated static-membership shape). It returns
// nil when membership cannot be determined — the caller MUST fail closed rather
// than propose against a guessed epoch.
func (b *rolloutBarrier) members(cfg *ports.BridgeConfig) []string {
	if b.membership != nil {
		return b.membership()
	}
	if cfg == nil || cfg.Bridge.Cluster == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Bridge.Cluster.Endpoints))
	for id := range cfg.Bridge.Cluster.Endpoints {
		out = append(out, id)
	}
	return out
}

// proposeCoordinatedRollout hands a live-safe clustered delta to the rollout
// barrier. It returns an error whenever the delta could NOT be proposed, and the
// caller MUST then fail closed — an unproposed delta that is reported as
// deferred would be acknowledged as committed while no member ever applies it.
//
// Fail-closed cases: no barrier wired (the operator opted into coordinated
// rollout in config but the process has no rollout store), or an undeterminable
// membership epoch — proposing against a guessed epoch would let the coordinator
// commit without every real member's ack.
func (s *Supervisor) proposeCoordinatedRollout(ctx context.Context, oldCfg, newCfg *ports.BridgeConfig) error {
	b := s.rollout
	if b == nil {
		return fmt.Errorf("bridge: cluster.rollout: coordinated is configured but this process has no "+
			"rollout store wired, so the delta cannot be proposed cluster-wide and the live reload is "+
			"refused (the running config keeps serving). Wire the rollout barrier "+
			"(bridge.WithClusterRollout) or perform a whole-cohort replacement "+
			"(docs/runbooks/cluster-config-rollout.md) (attempted_config_version=%d)", newCfg.Version)
	}
	members := b.members(oldCfg)
	if len(members) == 0 {
		return fmt.Errorf("bridge: cluster.rollout: coordinated is configured but the cohort membership "+
			"is unknown (no bridge.cluster.endpoints and no membership source), so a rollout cannot "+
			"freeze a membership epoch and the live reload is refused (the running config keeps "+
			"serving) (attempted_config_version=%d)", newCfg.Version)
	}
	if err := b.propose(ctx, newCfg, members); err != nil {
		return fmt.Errorf("bridge: proposing the coordinated cluster rollout failed, so the live reload "+
			"is refused and the running config keeps serving (attempted_config_version=%d): %w",
			newCfg.Version, err)
	}
	return nil
}

// propose stages newCfg as a candidate generation. It is idempotent across the
// cohort: when another member already proposed this generation the conditional
// create fails with shared.ErrAlreadyExists, which is SUCCESS for this node — it
// simply joins the rollout the peer opened. Any other error fails closed.
func (b *rolloutBarrier) propose(ctx context.Context, newCfg *ports.BridgeConfig, members []string) error {
	raw, ok := configCanonicalBytes(newCfg)
	if !ok {
		return fmt.Errorf("bridge: cannot compute the candidate config digest for a coordinated rollout")
	}
	_, err := b.store.Propose(ctx, persistence.RolloutProposal{
		ProposerID:    b.memberID,
		ConfigDigest:  candidateConfigDigest(raw),
		ConfigVersion: newCfg.Version,
		Members:       members,
		TTL:           b.ttl,
	})
	if err != nil && !errors.Is(err, shared.ErrAlreadyExists) {
		return err
	}
	return nil
}
