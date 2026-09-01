package bridge

import (
	"context"
	"sync"
	"time"

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
