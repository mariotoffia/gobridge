package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// ClusterRolloutStore coordinates a cluster-wide, barrier-gated config rollout
// through a single shared source of truth. It is the store side of the
// coordinated rollout protocol: there is at most ONE active rollout at a time,
// generations are monotonic, and every state transition is a conditional write
// so no node-to-node RPC is needed -- the store IS the coordination channel.
//
// Implementations MUST enforce, via conditional writes / compare-and-set:
//
//   - I1 (single active rollout): Propose succeeds only when there is no active
//     (Proposed or Staging) rollout. A Propose while one is active returns
//     ErrAlreadyExists. Propose after the last rollout reached a terminal state
//     opens a NEW rollout at the next generation (strictly monotonic).
//
//   - I2 (all-member barrier): Commit succeeds only from Staging with every
//     membership-epoch member acked. Otherwise ErrRolloutNotCommittable. This
//     is enforced by the domain (persistence.Rollout.WithCommit); the store
//     applies it under its CAS.
//
//   - I3 (fencing): Commit and Abort carry the coordinator's lease token. A
//     token that is invalid, or whose version is below the version that last
//     decided this rollout, is rejected with ErrStaleFencingToken -- a deposed
//     coordinator cannot OVERRIDE a decision the live one already made. A
//     same-or-newer token that re-decides in the SAME direction is an idempotent
//     no-op success (a coordinator that crashed mid-decide may safely resume:
//     goal G3). Note the scope: the recorded version is zero until the first
//     decision, so I3 does not fence the FIRST decision -- see the coordVersion
//     doc on persistence.Rollout for why that residual is fail-safe.
//
//   - I4 (terminal-immutable): once Committed or Aborted, a rollout admits no
//     ack/nack and no cross-direction decision (commit-of-aborted /
//     abort-of-committed) -> ErrRolloutTerminal.
//
//   - I5 (ack-at-most-once): Ack/Nack from a member outside the frozen epoch,
//     or a second vote from a member that already acked or nacked, is rejected
//     with ErrRolloutAckRejected.
//
// The generation argument on Ack/Nack/Commit/Abort targets a SPECIFIC rollout;
// a call whose generation is not the current active generation returns
// ErrNotFound (the referenced rollout is gone -- e.g. a slow member acking a
// superseded generation). Current returns ErrNotFound when no rollout has ever
// been proposed.
//
// Returned persistence.Rollout values (from Propose and Current) are read-only
// snapshots; implementations MUST NOT hand back an aliased view a caller could
// mutate to corrupt store state. Mutating methods return only an error (the
// interface stays minimal per project rule); a coordinator observes the
// resulting state through Current.
type ClusterRolloutStore interface {
	// Propose opens a new rollout at the next monotonic generation from the
	// given proposal, returning it in the Proposed state. Returns
	// ErrAlreadyExists if a rollout is currently active (I1), or
	// ErrInvalidRolloutProposal for a malformed proposal.
	Propose(ctx context.Context, proposal persistence.RolloutProposal) (persistence.Rollout, error)

	// Ack records memberID's acknowledgement (with the digest of the build it
	// staged) of the given generation, advancing Proposed -> Staging on the
	// first ack. See I4/I5 for rejections; ErrNotFound if generation is not the
	// active rollout.
	Ack(ctx context.Context, generation uint64, memberID, buildDigest string) error

	// Nack records memberID's rejection of the given generation without
	// terminating the rollout (the coordinator decides to abort). Same
	// rejections as Ack.
	Nack(ctx context.Context, generation uint64, memberID, reason string) error

	// Commit commits the given generation under the coordinator's fencing
	// token. Requires the barrier (I2); enforces fencing (I3) and
	// terminal-immutability (I4). Idempotent under a same-or-newer token.
	Commit(ctx context.Context, generation uint64, token persistence.LeaseToken) error

	// Abort aborts the given generation under the coordinator's fencing token,
	// recording reason. Enforces fencing (I3) and terminal-immutability (I4).
	// Idempotent under a same-or-newer token.
	Abort(ctx context.Context, generation uint64, token persistence.LeaseToken, reason string) error

	// Current returns the current (active or last-decided) rollout snapshot, or
	// ErrNotFound if none has ever been proposed.
	Current(ctx context.Context) (persistence.Rollout, error)
}

// ClusterCommittedConfigStore is the durable last-committed configuration
// artifact (design Phase-4 residual): the exact config bytes a (re)joining member
// boots on and a member that missed a commit reconciles to. It is a SEPARATE port
// from ClusterRolloutStore (kept small per the project's interface rule) because
// it is a distinct concern with a distinct lifecycle: the active rollout row is
// single-active, digest-only, and overwritten on the next Propose, whereas the
// committed artifact carries bytes and MUST survive across proposals. The same
// backing store typically implements both (one DynamoDB table, two rows).
type ClusterCommittedConfigStore interface {
	// PutCommittedConfig durably records the cohort's last-committed config
	// artifact. Implementations MUST enforce, via conditional write / CAS:
	//
	//   - Monotonicity: a Put whose Generation is STRICTLY GREATER than the stored
	//     one overwrites it; a Put whose Generation is LOWER is a no-op success (a
	//     stale writer -- e.g. a booting member seeding the baseline generation 0
	//     after a commit already advanced -- must never regress the artifact).
	//
	//   - Idempotence at the same Generation: the same generation with the SAME
	//     digest is a no-op success (every member commits the same config, so N
	//     members each write the artifact at commit without conflicting). The same
	//     generation with a DIFFERENT digest is corruption -- two configs cannot
	//     share a committed generation -- and returns ErrRolloutDigestMismatch.
	//     NOTE: this conflict is only detectable at the CURRENT generation; a
	//     divergent config for an ALREADY-SUPERSEDED generation is swallowed by the
	//     lower-generation no-op rule (it cannot regress the newer artifact anyway).
	//
	// A malformed artifact (no bytes / no digest) is rejected with
	// ErrInvalidConfig (CommittedRolloutConfig.Validate).
	PutCommittedConfig(ctx context.Context, cfg persistence.CommittedRolloutConfig) error

	// CommittedConfig returns the cohort's last-committed config artifact, or
	// ErrNotFound if nothing has been committed or seeded yet.
	CommittedConfig(ctx context.Context) (persistence.CommittedRolloutConfig, error)
}
