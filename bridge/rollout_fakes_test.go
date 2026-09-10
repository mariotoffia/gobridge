package bridge

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// electionLeaseStore is a hand-rolled ports.LeaseStore modelling exactly the
// election semantics the rollout coordinator depends on, and nothing else:
// one owner at a time, a fencing Version that advances only on Acquire (never on
// Renew — the port contract), and stale-token rejection. It is deliberately NOT
// TTL-driven: expiry is triggered explicitly by the test via expire(), so no
// coordinator test depends on wall-clock timing.
type electionLeaseStore struct {
	mu      sync.Mutex
	owner   string
	version uint64
	free    bool // true once expire() ran: the next Acquire wins
}

func newElectionLeaseStore() *electionLeaseStore {
	return &electionLeaseStore{free: true}
}

// Owner reports the current holder, or "" when the lease is free.
func (s *electionLeaseStore) Owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

func (s *electionLeaseStore) Acquire(
	_ context.Context, _ string, ownerID string, _ time.Duration, _ map[string]string,
) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.free && s.owner != "" && s.owner != ownerID {
		return persistence.LeaseToken{}, shared.ErrAlreadyExists
	}
	s.version++
	s.owner = ownerID
	s.free = false
	return persistence.LeaseToken{Owner: ownerID, Version: s.version}, nil
}

func (s *electionLeaseStore) Renew(
	_ context.Context, _ string, token persistence.LeaseToken, _ time.Duration, _ map[string]string,
) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != token.Owner || s.version != token.Version {
		return persistence.LeaseToken{}, shared.ErrStaleFencingToken
	}
	// Renewal preserves the fencing version established at Acquire.
	return token, nil
}

func (s *electionLeaseStore) Release(_ context.Context, _ string, token persistence.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != token.Owner || s.version != token.Version {
		return shared.ErrStaleFencingToken
	}
	s.owner, s.free = "", true
	return nil
}

func (s *electionLeaseStore) Current(_ context.Context, _ string) (persistence.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == "" {
		return persistence.LeaseInfo{}, shared.ErrNotFound
	}
	return persistence.LeaseInfo{Owner: s.owner, Version: s.version}, nil
}

// depose simulates a takeover by another member: the incumbent's token becomes
// stale (Renew/Release reject it) and the lease is held by newOwner.
func (s *electionLeaseStore) depose(newOwner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.owner = newOwner
	s.free = false
}

// held reports the current owner, or "" when the lease is free.
func (s *electionLeaseStore) held() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.free {
		return ""
	}
	return s.owner
}

var _ ports.LeaseStore = (*electionLeaseStore)(nil)

// testRolloutConfig wires a complete, minimal rollout barrier for a Supervisor
// test: the given rollout store plus a fresh election lease store. Cadence knobs
// are left at zero so the defaults apply; tests that drive the loop supply their
// own via testRolloutConfigWith.
func testRolloutConfig(store ports.ClusterRolloutStore, memberID string) ClusterRolloutConfig {
	return ClusterRolloutConfig{
		Store:    store,
		Lease:    newElectionLeaseStore(),
		MemberID: memberID,
	}
}

// blackHoleRolloutStore wraps a real rollout store and swallows the call classes
// a test has black-holed: those calls block IGNORING their context until the
// test releases them, which is what an SDK call into a TCP black hole with no
// client-side timeout does. A store that merely returned an error would prove
// nothing — the barrier's danger is the call that never comes back at all.
type blackHoleRolloutStore struct {
	inner   *memoryrollout.Store
	release chan struct{}
	entered chan string

	mu    sync.Mutex
	holes map[string]bool
}

func newBlackHoleRolloutStore(inner *memoryrollout.Store) *blackHoleRolloutStore {
	return &blackHoleRolloutStore{
		inner:   inner,
		release: make(chan struct{}),
		entered: make(chan string, 64),
		holes:   map[string]bool{},
	}
}

// blackHole starts swallowing the named call classes ("read", "vote", "decide",
// "artifact"); freeAll releases every held call so no goroutine outlives the test.
func (s *blackHoleRolloutStore) blackHole(classes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range classes {
		s.holes[c] = true
	}
}

func (s *blackHoleRolloutStore) freeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holes = map[string]bool{}
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

func (s *blackHoleRolloutStore) hold(class string) {
	s.mu.Lock()
	held := s.holes[class]
	s.mu.Unlock()
	if !held {
		return
	}
	select {
	case s.entered <- class:
	default:
	}
	<-s.release
}

func (s *blackHoleRolloutStore) Propose(
	ctx context.Context, p persistence.RolloutProposal,
) (persistence.Rollout, error) {
	s.hold(rolloutOpPropose)
	return s.inner.Propose(ctx, p)
}

func (s *blackHoleRolloutStore) Ack(ctx context.Context, gen uint64, memberID, digest string) error {
	s.hold(rolloutOpVote)
	return s.inner.Ack(ctx, gen, memberID, digest)
}

func (s *blackHoleRolloutStore) Nack(ctx context.Context, gen uint64, memberID, reason string) error {
	s.hold(rolloutOpVote)
	return s.inner.Nack(ctx, gen, memberID, reason)
}

func (s *blackHoleRolloutStore) Commit(ctx context.Context, gen uint64, tok persistence.LeaseToken) error {
	s.hold(rolloutOpDecide)
	return s.inner.Commit(ctx, gen, tok)
}

func (s *blackHoleRolloutStore) Converge(ctx context.Context, gen uint64, memberID string) error {
	s.hold(rolloutOpVote)
	return s.inner.Converge(ctx, gen, memberID)
}

func (s *blackHoleRolloutStore) Confirm(ctx context.Context, gen uint64, tok persistence.LeaseToken) error {
	s.hold(rolloutOpDecide)
	return s.inner.Confirm(ctx, gen, tok)
}

func (s *blackHoleRolloutStore) Revert(
	ctx context.Context, gen uint64, tok persistence.LeaseToken, reason string,
) error {
	s.hold(rolloutOpDecide)
	return s.inner.Revert(ctx, gen, tok, reason)
}

func (s *blackHoleRolloutStore) Abort(
	ctx context.Context, gen uint64, tok persistence.LeaseToken, reason string,
) error {
	s.hold(rolloutOpDecide)
	return s.inner.Abort(ctx, gen, tok, reason)
}

func (s *blackHoleRolloutStore) Current(ctx context.Context) (persistence.Rollout, error) {
	s.hold(rolloutOpRead)
	return s.inner.Current(ctx)
}

func (s *blackHoleRolloutStore) PutCommittedConfig(
	ctx context.Context, cfg persistence.CommittedRolloutConfig,
) error {
	s.hold(rolloutOpArtifact)
	return s.inner.PutCommittedConfig(ctx, cfg)
}

func (s *blackHoleRolloutStore) CommittedConfig(ctx context.Context) (persistence.CommittedRolloutConfig, error) {
	s.hold(rolloutOpRead)
	return s.inner.CommittedConfig(ctx)
}

var (
	_ ports.ClusterRolloutStore         = (*blackHoleRolloutStore)(nil)
	_ ports.ClusterCommittedConfigStore = (*blackHoleRolloutStore)(nil)
)

// artifactFaultStore wraps a real rollout store and injects the two committed-
// artifact failure modes HIGH-7 is about: a write that ERRORS, and — worse — a
// write that reports success while persisting nothing (the silent no-op a
// conditional write degrades into when the caller's assumptions are wrong). Both
// must be retried, and neither may latch the member's "artifact recorded" state.
type artifactFaultStore struct {
	*memoryrollout.Store

	mu sync.Mutex
	// failures counts down: while positive each write fails and decrements.
	// Negative means fail forever.
	failures int
	// lying makes writes report success without persisting anything.
	lying bool
	// writes counts every PutCommittedConfig call, successful or not.
	writes int
}

func newArtifactFaultStore(failures int, lying bool) *artifactFaultStore {
	return &artifactFaultStore{Store: memoryrollout.NewStore(), failures: failures, lying: lying}
}

func (s *artifactFaultStore) PutCommittedConfig(
	ctx context.Context, cfg persistence.CommittedRolloutConfig,
) error {
	s.mu.Lock()
	s.writes++
	lying := s.lying
	fail := s.failures != 0
	if s.failures > 0 {
		s.failures--
	}
	s.mu.Unlock()
	switch {
	case lying:
		return nil // reported durable, wrote nothing
	case fail:
		return shared.ErrUnavailable
	default:
		return s.Store.PutCommittedConfig(ctx, cfg)
	}
}

// writeCount reports how many artifact writes were attempted.
func (s *artifactFaultStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

var (
	_ ports.ClusterRolloutStore         = (*artifactFaultStore)(nil)
	_ ports.ClusterCommittedConfigStore = (*artifactFaultStore)(nil)
)

// deadlineRecordingLeaseStore records the budget each lease call was given, so a
// test can assert the CALLER's bound rather than measuring wall-clock duration.
type deadlineRecordingLeaseStore struct {
	*electionLeaseStore

	mu              sync.Mutex
	releaseBudget   time.Duration
	releaseObserved bool
}

func newDeadlineRecordingLeaseStore() *deadlineRecordingLeaseStore {
	return &deadlineRecordingLeaseStore{electionLeaseStore: newElectionLeaseStore()}
}

func (s *deadlineRecordingLeaseStore) Release(
	ctx context.Context, id string, token persistence.LeaseToken,
) error {
	if deadline, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.releaseBudget = time.Until(deadline)
		s.releaseObserved = true
		s.mu.Unlock()
	}
	return s.electionLeaseStore.Release(ctx, id, token)
}

// releaseBound reports the deadline budget the last Release was given.
func (s *deadlineRecordingLeaseStore) releaseBound() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseBudget, s.releaseObserved
}

var _ ports.LeaseStore = (*deadlineRecordingLeaseStore)(nil)
