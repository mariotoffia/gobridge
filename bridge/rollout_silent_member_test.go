package bridge

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// A rollout that runs out of votes must say WHICH member never cast one.
//
// The count on its own — "1/3 acks" — is the one thing an operator already knows
// by the time they are reading it: the rollout failed. What they cannot get from
// anywhere else is the address of the member to go and look at, because a member
// that never answered leaves no trace at all in the shared row. Naming it in the
// coordinator's reason, and having that member publish its own account of why it
// stayed silent, is the whole diagnosis path.

// TestDecideRollout_DeadlineAbortNamesTheMembersThatNeverVoted pins the abort
// reason as an address rather than a count.
func TestDecideRollout_DeadlineAbortNamesTheMembersThatNeverVoted(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b", "c"}, deadline, "a")

	action, reason := decideRollout(r, []string{"a", "b", "c"}, deadline.Add(time.Second))

	require.Equal(t, rolloutActionAbort, action)
	require.Contains(t, reason, "1/3 acks")
	require.Contains(t, reason, "b, c", "the abort must name the members that never voted")
	require.NotContains(t, reason, "a,", "a member that voted is not one of the silent ones")
}

// TestDecideRollout_ConfirmWindowRevertNamesTheMembersThatNeverConverged is the
// same rule on the confirm window: the cohort goes back because somebody could
// not run the change, and the reason has to say who.
func TestDecideRollout_ConfirmWindowRevertNamesTheMembersThatNeverConverged(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	r := mkStaging(t, []string{"a", "b", "c"}, deadline, "a", "b", "c")
	confirmDeadline := rolloutBase.Add(time.Minute)
	r, err := r.WithProvisionalCommit(persistence.LeaseToken{Owner: "coordinator", Version: 1}, confirmDeadline)
	require.Nil(t, err)
	r, err = r.WithConverged("a", rolloutBase)
	require.Nil(t, err)

	action, reason := decideRollout(r, []string{"a", "b", "c"}, confirmDeadline.Add(time.Second))

	require.Equal(t, rolloutActionRevert, action)
	require.Contains(t, reason, "1/3 members converged")
	require.Contains(t, reason, "b, c", "the revert must name the members that never converged")
}

// TestNotVotingReason_NamesTheCauseThisMemberKnows covers the other half: the
// silent member's own account. Only that member can distinguish a barrier that
// refused the delta from a config source that never delivered it, and the two
// need different operator actions.
func TestNotVotingReason_NamesTheCauseThisMemberKnows(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	staging := mkStaging(t, []string{"a", "b"}, deadline, "a")

	t.Run("a member that voted has nothing to explain", func(t *testing.T) {
		require.Empty(t, notVotingReason(staging, "a", true, ""))
	})

	t.Run("a lagging config source is the benign case", func(t *testing.T) {
		reason := notVotingReason(staging, "b", false, "")
		require.Contains(t, reason, "config source has not delivered the candidate")
	})

	t.Run("a refused delta outranks the lagging one", func(t *testing.T) {
		reason := notVotingReason(staging, "b", false, "a DIFFERENT rollout is in flight")
		require.Contains(t, reason, "refused to carry the delta")
		require.Contains(t, reason, "a DIFFERENT rollout is in flight")
	})

	t.Run("a member outside the epoch is a roster problem", func(t *testing.T) {
		reason := notVotingReason(staging, "z", false, "")
		require.Contains(t, reason, "frozen membership epoch")
	})

	t.Run("a resolved rollout explains nothing", func(t *testing.T) {
		aborted, err := staging.WithAbort(persistence.LeaseToken{Owner: "coordinator", Version: 1}, "deadline")
		require.Nil(t, err)
		require.Empty(t, notVotingReason(aborted, "b", false, "refused"))
	})
}

// TestRolloutObserver_PublishesWhyThisMemberIsSilent pins the path from a refused
// proposal to the deep-health block an operator reads, and the retraction: once
// the member votes, the stale refusal must not keep accusing it.
func TestRolloutObserver_PublishesWhyThisMemberIsSilent(t *testing.T) {
	deadline := rolloutBase.Add(5 * time.Minute)
	obs := newRolloutObserver(nil, nil, time.Second, "b")

	obs.noteProposal(errors.New("this delta digests differently here"))
	obs.observe(mkStaging(t, []string{"a", "b"}, deadline, "a"), "b", false, false)
	require.Contains(t, obs.status().NotVoting, "this delta digests differently here")

	obs.observe(mkStaging(t, []string{"a", "b"}, deadline, "a", "b"), "b", true, false)
	require.Empty(t, obs.status().NotVoting, "a member that voted retracts its own excuse")

	obs.noteProposal(errors.New("refused again"))
	obs.noteProposal(nil)
	obs.observe(mkStaging(t, []string{"a", "b"}, deadline, "a"), "b", false, false)
	require.Contains(t, obs.status().NotVoting, "config source has not delivered",
		"a proposal that succeeded clears the refusal")
}
