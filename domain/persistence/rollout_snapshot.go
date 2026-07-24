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

	if s.State.IsTerminal() != (s.CoordinatorVersion > 0) {
		return Rollout{}, shared.ErrInvalidRolloutProposal.
			WithMessage("rollout terminal state and coordinator version disagree").
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
	}, nil
}

// valid reports whether the state is one of the four known lifecycle values --
// a corruption guard for a rehydrated state string.
func (s RolloutState) valid() bool {
	switch s {
	case RolloutProposed, RolloutStaging, RolloutCommitted, RolloutAborted:
		return true
	default:
		return false
	}
}
