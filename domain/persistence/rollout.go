package persistence

import (
	"maps"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// RolloutState is the lifecycle state of a coordinated cluster config rollout.
//
//	Proposed ──ack──▶ Staging ──commit(all acks)──▶ Committed  (terminal)
//	   │                 │
//	   └────abort────────┴────────────────────────▶ Aborted    (terminal)
//
// Proposed is the freshly-opened barrier before any member has acked. The
// first Ack moves it to Staging. A Commit is only legal from Staging once every
// membership-epoch member has acked (invariant I2). Abort is legal from either
// non-terminal state. Committed and Aborted are terminal (invariant I4).
type RolloutState string

const (
	// RolloutProposed is a freshly-opened rollout barrier, no acks yet.
	RolloutProposed RolloutState = "proposed"
	// RolloutStaging is an in-progress rollout: at least one member has acked
	// but the all-member barrier is not yet satisfied.
	RolloutStaging RolloutState = "staging"
	// RolloutCommitted is the terminal success state: every member acked and
	// the coordinator committed the new config generation.
	RolloutCommitted RolloutState = "committed"
	// RolloutAborted is the terminal failure state: the coordinator aborted
	// (deadline, nack, or operator) before all members acked.
	RolloutAborted RolloutState = "aborted"
)

// IsTerminal reports whether the state is Committed or Aborted -- a decided
// rollout that admits no further ack/commit/abort transition (invariant I4).
func (s RolloutState) IsTerminal() bool {
	return s == RolloutCommitted || s == RolloutAborted
}

// RolloutAck is a single member's acknowledgement that it has staged (loaded
// and validated, but not yet activated) the proposed config generation. It is
// a plain value carried in Rollout.Acks, keyed by MemberID.
type RolloutAck struct {
	// MemberID is the cluster member that acked. Must be a member of the
	// rollout's frozen membership epoch.
	MemberID string
	// BuildDigest is the digest of the config build the member actually
	// staged, so the coordinator can detect a member that converged to the
	// wrong artifact.
	BuildDigest string
	// At is the time the member acked.
	At time.Time
}

// RolloutProposal is the input to open a rollout. It is validated by NewRollout;
// the store assigns the monotonic generation, not the proposer.
type RolloutProposal struct {
	// ProposerID is the coordinator member opening the rollout.
	ProposerID string
	// ConfigDigest is the digest of the proposed config artifact.
	ConfigDigest string
	// ConfigVersion is the monotonic config version being rolled out.
	ConfigVersion int
	// Members is the membership snapshot (epoch) that must all ack before the
	// rollout can commit. Frozen at propose time; deduped and sorted by
	// NewRollout for a deterministic barrier.
	Members []string
	// TTL bounds how long the rollout may stay open before the coordinator
	// aborts it on deadline. The store converts it to an absolute deadline.
	TTL time.Duration
}

// Rollout is the coordination state of one cluster config rollout: an immutable
// value whose transitions (WithAck / WithNack / WithCommit / WithAbort) return
// a NEW Rollout, never mutating the receiver. The store persists it and applies
// transitions under its own compare-and-set; the invariants I2/I3/I4/I5 live
// here so they hold for every store backend without duplication.
//
// aggregate-root
type Rollout struct {
	generation      uint64
	state           RolloutState
	configDigest    string
	configVersion   int
	membershipEpoch []string
	acks            map[string]RolloutAck
	nacks           map[string]string
	reason          string
	deadline        time.Time
	// coordVersion is the fencing high-water mark: the lease-token version of
	// the coordinator that last flipped the rollout to a terminal state. Zero
	// while non-terminal. A Commit/Abort carrying a strictly lower token is
	// rejected as stale (invariant I3).
	//
	// SCOPE — because it is zero until the FIRST decision, I3 rejects a stale
	// RE-decision, not a stale first decision: before any coordinator has
	// decided, every valid token passes the fence, so a deposed coordinator can
	// still decide first. Closing that needs the row to learn the live
	// coordinator epoch before a decision (an explicit claim write — a protocol
	// addition). The residual is fail-safe: a zombie Commit still requires the
	// full ack barrier (I2), a zombie Abort just keeps the old config serving,
	// and bridge.firstSideEffectAllowed's lock-delay makes a successor wait a
	// full lease TTL before acting. Pinned by
	// TestRolloutFencingIsRejectionOfReDecisionOnly.
	coordVersion uint64
}

// NewRollout opens a Proposed rollout for the given generation and proposal.
// It validates the proposal (invariant inputs) and freezes a deterministic
// (deduped, sorted) membership epoch. The deadline is supplied by the caller
// (the store, from its clock + proposal TTL) so the domain stays clock-free.
func NewRollout(generation uint64, p RolloutProposal, deadline time.Time) (Rollout, *shared.BridgeError) {
	if generation == 0 {
		return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout generation must be > 0")
	}
	if p.ProposerID == "" {
		return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout proposer id is required")
	}
	if p.ConfigDigest == "" {
		return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout config digest is required")
	}
	if len(p.Members) == 0 {
		return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout membership epoch is empty")
	}

	epoch := slices.Clone(p.Members)
	slices.Sort(epoch)
	for i, m := range epoch {
		if m == "" {
			return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout membership epoch has an empty member id")
		}
		if i > 0 && epoch[i-1] == m {
			return Rollout{}, shared.ErrInvalidRolloutProposal.
				WithMessage("rollout membership epoch has a duplicate member").With("member", m)
		}
	}

	return Rollout{
		generation:      generation,
		state:           RolloutProposed,
		configDigest:    p.ConfigDigest,
		configVersion:   p.ConfigVersion,
		membershipEpoch: epoch,
		acks:            make(map[string]RolloutAck),
		nacks:           make(map[string]string),
		deadline:        deadline,
	}, nil
}

// Generation returns the monotonic rollout generation.
func (r Rollout) Generation() uint64 { return r.generation }

// State returns the current lifecycle state.
func (r Rollout) State() RolloutState { return r.state }

// ConfigDigest returns the digest of the proposed config artifact.
func (r Rollout) ConfigDigest() string { return r.configDigest }

// ConfigVersion returns the config version being rolled out.
func (r Rollout) ConfigVersion() int { return r.configVersion }

// MembershipEpoch returns a copy of the frozen membership epoch (sorted).
func (r Rollout) MembershipEpoch() []string { return slices.Clone(r.membershipEpoch) }

// Acks returns a copy of the recorded acks keyed by member id.
func (r Rollout) Acks() map[string]RolloutAck { return maps.Clone(r.acks) }

// Nacks returns a copy of the recorded nacks keyed by member id.
func (r Rollout) Nacks() map[string]string { return maps.Clone(r.nacks) }

// Reason returns the abort reason (empty unless aborted).
func (r Rollout) Reason() string { return r.reason }

// Deadline returns the absolute deadline after which the rollout should abort.
func (r Rollout) Deadline() time.Time { return r.deadline }

// CoordinatorVersion returns the fencing high-water mark (see coordVersion).
func (r Rollout) CoordinatorVersion() uint64 { return r.coordVersion }

// CanCommit reports whether the rollout satisfies the all-member barrier
// (invariant I2): it is in Staging and every membership-epoch member has acked.
func (r Rollout) CanCommit() bool {
	if r.state != RolloutStaging || len(r.acks) != len(r.membershipEpoch) {
		return false
	}
	for _, m := range r.membershipEpoch {
		if _, ok := r.acks[m]; !ok {
			return false
		}
	}
	return true
}

// WithAck records member's acknowledgement of the proposed generation and, if
// this is the first ack, advances Proposed → Staging. Rejects an ack from a
// member outside the frozen epoch, a second ack/nack from the same member
// (invariant I5), an empty build digest, or any ack against a terminal rollout
// (invariant I4).
func (r Rollout) WithAck(memberID, buildDigest string, at time.Time) (Rollout, *shared.BridgeError) {
	if r.state.IsTerminal() {
		return r, shared.ErrRolloutTerminal.WithMessage("cannot ack a decided rollout").With("state", string(r.state))
	}
	if err := r.checkVoter(memberID); err != nil {
		return r, err
	}
	if buildDigest == "" {
		return r, shared.ErrRolloutAckRejected.WithMessage("ack build digest is required").With("member", memberID)
	}

	nr := r.clone()
	nr.acks[memberID] = RolloutAck{MemberID: memberID, BuildDigest: buildDigest, At: at}
	if nr.state == RolloutProposed {
		nr.state = RolloutStaging
	}
	return nr, nil
}

// WithNack records member's rejection of the proposed generation without
// terminating the rollout: a nack is a signal to the coordinator, which then
// decides to abort. Same voter rules as WithAck (invariant I5).
func (r Rollout) WithNack(memberID, reason string) (Rollout, *shared.BridgeError) {
	if r.state.IsTerminal() {
		return r, shared.ErrRolloutTerminal.WithMessage("cannot nack a decided rollout").With("state", string(r.state))
	}
	if err := r.checkVoter(memberID); err != nil {
		return r, err
	}

	nr := r.clone()
	nr.nacks[memberID] = reason
	return nr, nil
}

// WithCommit commits the rollout under the coordinator's fencing token. The
// token gate (invariant I3) runs first: an invalid or stale token is rejected
// regardless of state. A same-or-newer token against an already-committed
// rollout is an idempotent no-op (design goal G3). Commit requires the barrier
// (invariant I2); committing an aborted rollout is terminal-illegal (I4).
func (r Rollout) WithCommit(tok LeaseToken) (Rollout, *shared.BridgeError) {
	if err := r.checkFence(tok, "commit"); err != nil {
		return r, err
	}
	switch r.state {
	case RolloutCommitted:
		return r, nil // idempotent: same-or-newer coordinator re-committing
	case RolloutAborted:
		return r, shared.ErrRolloutTerminal.WithMessage("cannot commit an aborted rollout")
	default:
		if !r.CanCommit() {
			return r, shared.ErrRolloutNotCommittable.
				With("state", string(r.state)).With("acks", len(r.acks)).With("epoch", len(r.membershipEpoch))
		}
		nr := r.clone()
		nr.state = RolloutCommitted
		nr.coordVersion = tok.Version
		return nr, nil
	}
}

// WithAbort aborts the rollout under the coordinator's fencing token, recording
// reason. Same fencing gate as WithCommit (invariant I3). A same-or-newer token
// against an already-aborted rollout is an idempotent no-op; aborting a
// committed rollout is terminal-illegal (invariant I4).
func (r Rollout) WithAbort(tok LeaseToken, reason string) (Rollout, *shared.BridgeError) {
	if err := r.checkFence(tok, "abort"); err != nil {
		return r, err
	}
	switch r.state {
	case RolloutAborted:
		return r, nil // idempotent: same-or-newer coordinator re-aborting
	case RolloutCommitted:
		return r, shared.ErrRolloutTerminal.WithMessage("cannot abort a committed rollout")
	default:
		nr := r.clone()
		nr.state = RolloutAborted
		nr.reason = reason
		nr.coordVersion = tok.Version
		return nr, nil
	}
}

// checkVoter enforces the shared Ack/Nack preconditions (invariant I5): the
// member is in the frozen epoch and has not already acked or nacked.
func (r Rollout) checkVoter(memberID string) *shared.BridgeError {
	if !slices.Contains(r.membershipEpoch, memberID) {
		return shared.ErrRolloutAckRejected.WithMessage("member is not in the rollout epoch").With("member", memberID)
	}
	if _, acked := r.acks[memberID]; acked {
		return shared.ErrRolloutAckRejected.WithMessage("member already acked").With("member", memberID)
	}
	if _, nacked := r.nacks[memberID]; nacked {
		return shared.ErrRolloutAckRejected.WithMessage("member already nacked").With("member", memberID)
	}
	return nil
}

// checkFence enforces the fencing gate (invariant I3): the token must be valid
// and carry a version >= the recorded coordinator high-water mark. A lower
// version is a deposed coordinator and is rejected as stale.
func (r Rollout) checkFence(tok LeaseToken, op string) *shared.BridgeError {
	if !tok.Valid() || tok.Version < r.coordVersion {
		return shared.ErrStaleFencingToken.
			WithMessage("rollout "+op+" rejected: invalid or stale fencing token").
			With("givenOwner", tok.Owner).With("givenVersion", tok.Version).With("coordinatorVersion", r.coordVersion)
	}
	return nil
}

// clone returns a shallow copy with fresh ack/nack maps so a transition never
// mutates the receiver's maps. membershipEpoch is never mutated post-construction
// and is safely shared.
func (r Rollout) clone() Rollout {
	nr := r
	nr.acks = maps.Clone(r.acks)
	nr.nacks = maps.Clone(r.nacks)
	return nr
}
