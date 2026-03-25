package domain

import "time"

// LeaseToken is the fencing token returned by lease operations.
// The Version field is monotonically increasing and prevents stale owners
// from continuing to operate after a lease transfer.
type LeaseToken struct {
	Version uint64
	Owner   string
}

// LeaseInfo describes the current state of a lease.
type LeaseInfo struct {
	LeaseID   string
	Owner     string
	Version   uint64
	ExpiresAt time.Time
}
