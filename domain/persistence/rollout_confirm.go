package persistence

import (
	"maps"
	"slices"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Confirm window (ADR 0014 — NETCONF/NSO confirmed-commit) additions to the
// Rollout aggregate: a windowed commit is PROVISIONAL, members Converge, and a
// fenced coordinator Confirms (whole-epoch converged) or Reverts (deadline). These
// layer onto the base state machine in rollout.go; the terminality of a windowed
// Committed rollout (Rollout.IsTerminal) and the shared commit helper live there.

// RolloutConverged records that a member reached convergence (its post-swap
// readiness check passed) on the provisionally-committed generation. It
// is the confirm-window counterpart to RolloutAck: an Ack proves validated+built,
// a RolloutConverged proves converged-against-the-real-broker.
type RolloutConverged struct {
	// MemberID is the cluster member that converged. Must be an epoch member.
	MemberID string
	// At is the time the member recorded convergence.
	At time.Time
}

// ConfirmWindow returns the confirm window frozen at Propose. Zero means the base
// protocol (Commit is final).
func (r Rollout) ConfirmWindow() time.Duration { return r.confirmWindow }

// ConfirmDeadline returns the absolute confirm deadline stamped at a provisional
// commit, or the zero time for a base-protocol commit / any pre-commit state.
func (r Rollout) ConfirmDeadline() time.Time { return r.confirmDeadline }

// Converged returns a copy of the recorded convergence records keyed by member id.
func (r Rollout) Converged() map[string]RolloutConverged { return maps.Clone(r.converged) }

// CanConfirm reports whether the confirm barrier is satisfied: the
// rollout is provisionally committed (active confirm window) and every
// membership-epoch member has recorded convergence.
func (r Rollout) CanConfirm() bool {
	if r.state != RolloutCommitted || r.confirmDeadline.IsZero() {
		return false
	}
	if len(r.converged) != len(r.membershipEpoch) {
		return false
	}
	for _, m := range r.membershipEpoch {
		if _, ok := r.converged[m]; !ok {
			return false
		}
	}
	return true
}

// WithProvisionalCommit commits the rollout PROVISIONALLY under a confirm window:
// the state becomes Committed but non-terminal, and confirmDeadline is stamped
// (supplied by the store from its clock + the frozen window). Every other rule is
// identical to WithCommit (barrier, fencing, idempotency). A caller passes
// the zero time to get a final commit -- use WithCommit for that.
func (r Rollout) WithProvisionalCommit(tok LeaseToken, confirmDeadline time.Time) (Rollout, *shared.BridgeError) {
	return r.commit(tok, confirmDeadline)
}

// WithConverged records member's post-swap convergence on a provisionally-
// committed generation. Legal only while Committed with an active confirm window;
// rejects a non-epoch member or a second convergence, a
// base-protocol (terminal) commit, or any pre-commit / decided state.
func (r Rollout) WithConverged(memberID string, at time.Time) (Rollout, *shared.BridgeError) {
	if r.IsTerminal() {
		return r, shared.ErrRolloutTerminal.
			WithMessage("cannot record convergence on a decided rollout").With("state", string(r.state))
	}
	if r.state != RolloutCommitted {
		return r, shared.ErrRolloutNotConfirmable.
			WithMessage("cannot record convergence before the rollout is committed").With("state", string(r.state))
	}
	if !slices.Contains(r.membershipEpoch, memberID) {
		return r, shared.ErrRolloutAckRejected.
			WithMessage("member is not in the rollout epoch").With("member", memberID)
	}
	if _, ok := r.converged[memberID]; ok {
		return r, shared.ErrRolloutAckRejected.WithMessage("member already converged").With("member", memberID)
	}
	nr := r.clone()
	nr.converged[memberID] = RolloutConverged{MemberID: memberID, At: at}
	return nr, nil
}

// WithConfirm confirms a provisionally-committed rollout under the coordinator's
// fencing token. The fence runs first. Requires the confirm barrier
// (an active window plus every epoch member converged). An already-confirmed
// rollout under a same-or-newer token is an idempotent no-op; confirming a
// reverted/aborted rollout, or a base-protocol commit, is terminal-illegal.
func (r Rollout) WithConfirm(tok LeaseToken) (Rollout, *shared.BridgeError) {
	if err := r.checkFence(tok, "confirm"); err != nil {
		return r, err
	}
	switch r.state {
	case RolloutConfirmed:
		return r, nil // idempotent
	case RolloutAborted, RolloutReverted:
		return r, shared.ErrRolloutTerminal.WithMessage("cannot confirm a decided rollout").With("state", string(r.state))
	case RolloutCommitted:
		if r.confirmDeadline.IsZero() {
			return r, shared.ErrRolloutTerminal.WithMessage("cannot confirm a base-protocol (final) commit")
		}
		if !r.CanConfirm() {
			return r, shared.ErrRolloutNotConfirmable.
				With("converged", len(r.converged)).With("epoch", len(r.membershipEpoch))
		}
		nr := r.clone()
		nr.state = RolloutConfirmed
		nr.coordVersion = tok.Version
		return nr, nil
	default:
		return r, shared.ErrRolloutNotConfirmable.
			WithMessage("cannot confirm a rollout that is not committed").With("state", string(r.state))
	}
}

// WithRevert reverts a provisionally-committed rollout under the coordinator's
// fencing token, recording reason (deadman outcome). Same fencing gate as
// WithConfirm. Legal only from a provisional (windowed) Committed state; an
// already-reverted rollout under a same-or-newer token is idempotent; reverting a
// confirmed/aborted rollout or a base-protocol commit is illegal.
func (r Rollout) WithRevert(tok LeaseToken, reason string) (Rollout, *shared.BridgeError) {
	if err := r.checkFence(tok, "revert"); err != nil {
		return r, err
	}
	switch r.state {
	case RolloutReverted:
		return r, nil // idempotent
	case RolloutConfirmed, RolloutAborted:
		return r, shared.ErrRolloutTerminal.WithMessage("cannot revert a decided rollout").With("state", string(r.state))
	case RolloutCommitted:
		if r.confirmDeadline.IsZero() {
			return r, shared.ErrRolloutTerminal.WithMessage("cannot revert a base-protocol (final) commit")
		}
		nr := r.clone()
		nr.state = RolloutReverted
		nr.reason = reason
		nr.coordVersion = tok.Version
		return nr, nil
	default:
		return r, shared.ErrRolloutTerminal.
			WithMessage("cannot revert a rollout that is not committed").With("state", string(r.state))
	}
}
