package persistence

import (
	"maps"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// RolloutSnapshot is the flat, exported projection of a Rollout aggregate: the
// serialization boundary a durable store adapter marshals to and from its
// backend (e.g. a DynamoDB item). Snapshot produces one from a live aggregate;
// RehydrateRollout reconstitutes a live aggregate from one, re-checking the
// aggregate's invariants because the bytes crossed a trust boundary (a store
// row can be corrupt or truncated). It is a data-transfer value, not a domain
// type: the store owns how it maps to storage attributes.
type RolloutSnapshot struct {
	Generation         uint64
	State              RolloutState
	ConfigDigest       string
	ConfigVersion      int
	MembershipEpoch    []string
	Acks               map[string]RolloutAck
	Nacks              map[string]string
	Reason             string
	Deadline           time.Time
	CoordinatorVersion uint64
	// Confirm window (design §8.1). ConfirmWindow is frozen at Propose;
	// ConfirmDeadline is stamped at a provisional commit (zero otherwise);
	// Converged records post-swap convergence keyed by member id.
	ConfirmWindow   time.Duration
	ConfirmDeadline time.Time
	Converged       map[string]RolloutConverged
}

// Snapshot returns a flat, deep-copied projection of the rollout suitable for a
// store to serialize. The copies mean a caller mutating the returned maps/slice
// cannot corrupt the immutable aggregate.
func (r Rollout) Snapshot() RolloutSnapshot {
	return RolloutSnapshot{
		Generation:         r.generation,
		State:              r.state,
		ConfigDigest:       r.configDigest,
		ConfigVersion:      r.configVersion,
		MembershipEpoch:    slices.Clone(r.membershipEpoch),
		Acks:               maps.Clone(r.acks),
		Nacks:              maps.Clone(r.nacks),
		Reason:             r.reason,
		Deadline:           r.deadline,
		CoordinatorVersion: r.coordVersion,
		ConfirmWindow:      r.confirmWindow,
		ConfirmDeadline:    r.confirmDeadline,
		Converged:          maps.Clone(r.converged),
	}
}

// RehydrateRollout reconstitutes a Rollout aggregate from a persisted snapshot.
// Unlike NewRollout it does not run creation-time rules (a rehydrated rollout
// may already be Staging/terminal); instead it fails closed on any corruption
// that would yield an aggregate whose invariants no longer hold, so a store can
// never act on a malformed row. The guards mirror the domain invariants:
//   - generation > 0 and a known lifecycle state;
//   - a non-empty, member-valid, duplicate-free membership epoch (I-epoch);
//   - every ack/nack references an epoch member (invariant I5);
//   - terminal state iff a coordinator fencing version is recorded (I3/I4);
//   - Proposed carries no acks and Staging carries at least one (the first ack
//     is what advances Proposed -> Staging).
//
// The returned aggregate owns fresh maps/slice, so it never aliases the caller's
// snapshot.
func RehydrateRollout(s RolloutSnapshot) (Rollout, *shared.BridgeError) {
	if s.Generation == 0 {
		return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout generation must be > 0")
	}
	if !s.State.valid() {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("unknown rollout state").With("state", string(s.State))
	}
	if len(s.MembershipEpoch) == 0 {
		return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("rollout membership epoch is empty")
	}

	epoch := slices.Clone(s.MembershipEpoch)
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

	acks := maps.Clone(s.Acks)
	if acks == nil {
		acks = make(map[string]RolloutAck)
	}
	for m := range acks {
		if !slices.Contains(epoch, m) {
			return Rollout{}, shared.ErrInvalidRolloutProposal.
				WithMessage("rollout ack from a non-member").With("member", m)
		}
	}
	nacks := maps.Clone(s.Nacks)
	if nacks == nil {
		nacks = make(map[string]string)
	}
	for m := range nacks {
		if !slices.Contains(epoch, m) {
			return Rollout{}, shared.ErrInvalidRolloutProposal.
				WithMessage("rollout nack from a non-member").With("member", m)
		}
	}

	// "Decided" (a coordinator flipped it) is exactly the set of states that no
	// longer accept votes: Committed (provisional or final), Aborted, Confirmed,
	// Reverted. Each of those records a coordinator fencing version; the two
	// pre-commit states must not. (Committed carries a version even under a
	// confirm window, though it is not yet terminal.)
	if !s.State.acceptsVotes() != (s.CoordinatorVersion > 0) {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("rollout decided state and coordinator version disagree").
			With("state", string(s.State)).With("coordinatorVersion", s.CoordinatorVersion)
	}
	switch s.State {
	case RolloutProposed:
		if len(acks) > 0 {
			return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("proposed rollout cannot carry acks")
		}
	case RolloutStaging:
		if len(acks) == 0 {
			return Rollout{}, shared.ErrInvalidRolloutProposal.WithMessage("staging rollout must carry at least one ack")
		}
	}

	converged := maps.Clone(s.Converged)
	if converged == nil {
		converged = make(map[string]RolloutConverged)
	}
	for m := range converged {
		if !slices.Contains(epoch, m) {
			return Rollout{}, shared.ErrInvalidRolloutProposal.
				WithMessage("rollout convergence from a non-member").With("member", m)
		}
	}
	// Confirm-window coherence (design §8.1): convergence and a confirm deadline
	// only exist post-commit; a Confirmed rollout must carry whole-epoch
	// convergence.
	postCommit := s.State == RolloutCommitted || s.State == RolloutConfirmed || s.State == RolloutReverted
	if len(converged) > 0 && !postCommit {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("convergence recorded before commit").With("state", string(s.State))
	}
	if !s.ConfirmDeadline.IsZero() && !postCommit {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("confirm deadline stamped before commit").With("state", string(s.State))
	}
	if s.State == RolloutConfirmed && len(converged) != len(epoch) {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("confirmed rollout is missing whole-epoch convergence").
			With("converged", len(converged)).With("epoch", len(epoch))
	}
	// A Committed row's provisional-vs-final nature rests ENTIRELY on
	// confirmDeadline (IsTerminal), so a windowed commit MUST carry a deadline and a
	// base commit MUST NOT. A corrupt/dropped deadline that left confirmWindow > 0
	// would otherwise rehydrate as a terminal final commit — silently skipping the
	// whole confirm barrier — so fail closed on the mismatch (design §8.1).
	if s.State == RolloutCommitted && (s.ConfirmWindow > 0) != !s.ConfirmDeadline.IsZero() {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("committed rollout confirm window and confirm deadline disagree").
			With("confirmWindow", s.ConfirmWindow.String()).With("hasConfirmDeadline", !s.ConfirmDeadline.IsZero())
	}

	return Rollout{
		generation:      s.Generation,
		state:           s.State,
		configDigest:    s.ConfigDigest,
		configVersion:   s.ConfigVersion,
		membershipEpoch: epoch,
		acks:            acks,
		nacks:           nacks,
		reason:          s.Reason,
		deadline:        s.Deadline,
		coordVersion:    s.CoordinatorVersion,
		confirmWindow:   s.ConfirmWindow,
		confirmDeadline: s.ConfirmDeadline,
		converged:       converged,
	}, nil
}

// valid reports whether the state is one of the four known lifecycle values --
// a corruption guard for a rehydrated state string.
func (s RolloutState) valid() bool {
	switch s {
	case RolloutProposed, RolloutStaging, RolloutCommitted, RolloutAborted, RolloutConfirmed, RolloutReverted:
		return true
	default:
		return false
	}
}
