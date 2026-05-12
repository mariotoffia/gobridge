package events

import "time"

// Canonical event types for the persistence/lease aggregate.
const (
	TypeLeaseAcquired = "persistence.lease.acquired"
	TypeLeaseRenewed  = "persistence.lease.renewed"
	TypeLeaseLost     = "persistence.lease.lost"
)

// Schema versions for lease events.
const (
	SchemaLeaseAcquiredV1 SchemaVersion = "1.0.0"
	SchemaLeaseRenewedV1  SchemaVersion = "1.0.0"
	SchemaLeaseLostV1     SchemaVersion = "1.0.0"
)

// LeaseAcquired is emitted when an instance successfully acquires a
// lease under the recorded fencing-token Version. Owner is the
// instance identifier the lease store recorded.
type LeaseAcquired struct {
	Header
	Owner     string    `json:"owner"`
	Version   uint64    `json:"version"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewLeaseAcquired constructs the event.
func NewLeaseAcquired(
	eventID, leaseID string,
	occurredAt time.Time,
	owner string,
	version uint64,
	expiresAt time.Time,
) LeaseAcquired {
	return LeaseAcquired{
		Header: newHeader(eventID, TypeLeaseAcquired, leaseID,
			occurredAt, SchemaLeaseAcquiredV1),
		Owner:     owner,
		Version:   version,
		ExpiresAt: expiresAt.UTC(),
	}
}

// LeaseRenewed is emitted when the current owner extends its lease.
// The fencing Version is unchanged; only ExpiresAt advances.
type LeaseRenewed struct {
	Header
	Owner     string    `json:"owner"`
	Version   uint64    `json:"version"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewLeaseRenewed constructs the event.
func NewLeaseRenewed(
	eventID, leaseID string,
	occurredAt time.Time,
	owner string,
	version uint64,
	expiresAt time.Time,
) LeaseRenewed {
	return LeaseRenewed{
		Header: newHeader(eventID, TypeLeaseRenewed, leaseID,
			occurredAt, SchemaLeaseRenewedV1),
		Owner:     owner,
		Version:   version,
		ExpiresAt: expiresAt.UTC(),
	}
}

// LeaseLost is emitted when an owner loses its lease (preempted,
// expired, or voluntarily relinquished). PreviousOwner is the
// instance that held the lease prior to the transition; Reason is a
// short, machine-stable token (e.g. "expired", "preempted",
// "released") suitable for filtering downstream.
type LeaseLost struct {
	Header
	PreviousOwner   string `json:"previous_owner"`
	PreviousVersion uint64 `json:"previous_version"`
	Reason          string `json:"reason"`
}

// NewLeaseLost constructs the event.
func NewLeaseLost(
	eventID, leaseID string,
	occurredAt time.Time,
	previousOwner string,
	previousVersion uint64,
	reason string,
) LeaseLost {
	return LeaseLost{
		Header: newHeader(eventID, TypeLeaseLost, leaseID,
			occurredAt, SchemaLeaseLostV1),
		PreviousOwner:   previousOwner,
		PreviousVersion: previousVersion,
		Reason:          reason,
	}
}
