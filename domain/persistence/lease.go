package persistence

import "time"

// LeaseToken is the fencing token returned by lease operations.
// The Version field is monotonically increasing and prevents stale owners
// from continuing to operate after a lease transfer.
type LeaseToken struct {
	Version uint64
	Owner   string
}

// LeaseInfo is a READ-ONLY SNAPSHOT of a lease's state as returned by
// LeaseStore.Current / Acquire / Renew. It is a plain DTO and carries no
// behavior: the LeaseStore — not LeaseInfo — owns the lease lifecycle and
// fencing invariants, enforcing them through conditional (compare-and-set)
// writes keyed on LeaseToken.Version. Mutating a returned LeaseInfo has no
// effect on stored state; in particular the Endpoints map is defensively
// copied at every LeaseStore boundary, so a returned LeaseInfo.Endpoints
// can never alias the store's internal state.
type LeaseInfo struct {
	LeaseID   string
	Owner     string
	Version   uint64
	ExpiresAt time.Time
	Endpoints map[string]string
}

// PeerInfo describes a remote gobridge instance derived from lease ownership.
type PeerInfo struct {
	InstanceID string
	Endpoints  map[string]string
}
