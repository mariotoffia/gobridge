package persistence

import (
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// OutboxStatus represents the state of an outbox record in the lifecycle
// state machine enforced by the OutboxRecord aggregate.
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxClaimed   OutboxStatus = "claimed"
	OutboxCompleted OutboxStatus = "completed"
	OutboxExpired   OutboxStatus = "expired"
)

// OutboxPartitionKey computes the outbox partition key from a record's
// session or binding identity. This is the canonical key used by
// OutboxStore.Persist, Claim, and QueryPending.
func OutboxPartitionKey(sessionID, bindingID string) string {
	if sessionID != "" {
		return "SESSION#" + sessionID
	}
	return "BINDING#" + bindingID
}

// OutboxSpec carries the immutable identity inputs required to construct
// a new OutboxRecord aggregate. Lifecycle state (Status, ClaimVersion,
// ReplayCount, …) is owned by the aggregate and cannot be set externally.
type OutboxSpec struct {
	ID              string
	RouteID         string
	EnvelopeID      string
	BindingID       string
	SessionID       string
	Address         string
	Envelope        messaging.Envelope
	DispatchHeaders map[string]any
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

// OutboxRecord is the aggregate root for a durable outbox entry on the
// reliable-egress path. Identity attributes are exported and immutable
// once the aggregate is constructed; lifecycle state is private and
// transitioned only through the aggregate's state-machine methods
// (Claim, Complete, Expire). The persistence boundary uses Snapshot /
// RehydrateFromSnapshot so storage adapters never mutate state directly.
type OutboxRecord struct {
	// Identity (immutable after construction).
	ID              string
	RouteID         string
	EnvelopeID      string
	BindingID       string
	SessionID       string
	Address         string
	Envelope        messaging.Envelope
	DispatchHeaders map[string]any
	CreatedAt       time.Time
	ExpiresAt       time.Time

	// Lifecycle state (mutated only through aggregate methods).
	status       OutboxStatus
	claimedBy    string
	claimedAt    time.Time
	claimVersion uint64
	replayCount  int
	completedAt  time.Time
}

// NewOutboxRecord constructs a new OutboxRecord aggregate in the Pending
// state. Returns a typed *shared.BridgeError when required identity
// fields are missing so the runtime can classify the failure without
// re-parsing string messages.
func NewOutboxRecord(spec OutboxSpec) (*OutboxRecord, *shared.BridgeError) {
	if spec.ID == "" {
		return nil, shared.ErrInvalidOutboxRecord.WithMessage("outbox record ID is required")
	}
	envelopeID := spec.EnvelopeID
	if envelopeID == "" {
		envelopeID = spec.Envelope.ID
	}
	if envelopeID == "" {
		return nil, shared.ErrInvalidOutboxRecord.WithMessage("outbox record envelope ID is required")
	}
	if spec.SessionID == "" && spec.BindingID == "" {
		return nil, shared.ErrInvalidOutboxRecord.
			WithMessage("outbox record requires session ID or binding ID for partition key")
	}
	return &OutboxRecord{
		ID:              spec.ID,
		RouteID:         spec.RouteID,
		EnvelopeID:      envelopeID,
		BindingID:       spec.BindingID,
		SessionID:       spec.SessionID,
		Address:         spec.Address,
		Envelope:        spec.Envelope,
		DispatchHeaders: spec.DispatchHeaders,
		CreatedAt:       spec.CreatedAt,
		ExpiresAt:       spec.ExpiresAt,
		status:          OutboxPending,
	}, nil
}

// Status returns the current lifecycle state of the aggregate.
func (r *OutboxRecord) Status() OutboxStatus { return r.status }

// ClaimedBy returns the owner that last successfully claimed the record.
// Empty string if the record has never been claimed.
func (r *OutboxRecord) ClaimedBy() string { return r.claimedBy }

// ClaimedAt returns the timestamp of the last successful claim.
func (r *OutboxRecord) ClaimedAt() time.Time { return r.claimedAt }

// ClaimVersion returns the fencing-token version recorded by the last
// successful claim. Used by stores to detect stale completers.
func (r *OutboxRecord) ClaimVersion() uint64 { return r.claimVersion }

// ReplayCount returns the number of times this record has been claimed
// (and therefore attempted). Incremented atomically inside Claim.
func (r *OutboxRecord) ReplayCount() int { return r.replayCount }

// CompletedAt returns the completion timestamp; zero if not completed.
func (r *OutboxRecord) CompletedAt() time.Time { return r.completedAt }

// IsClaimable reports whether the record may be claimed by a holder of
// fencing-token version currentTokenVersion. A record is claimable when
// it is Pending, or when it is Claimed by a strictly older fencing
// token (the previous owner's lease has been preempted).
func (r *OutboxRecord) IsClaimable(currentTokenVersion uint64) bool {
	switch r.status {
	case OutboxPending:
		return true
	case OutboxClaimed:
		return r.claimVersion < currentTokenVersion
	default:
		return false
	}
}

// Claim transitions the aggregate to Claimed for the supplied owner and
// fencing-token version. Returns shared.ErrOutboxNotClaimable when the
// record is in a terminal state or is already claimed under an equal-or-
// newer fencing token. ReplayCount is incremented on every successful
// claim so callers can enforce poison-message caps.
func (r *OutboxRecord) Claim(now time.Time, claimedBy string, tokenVersion uint64) *shared.BridgeError {
	if !r.IsClaimable(tokenVersion) {
		return shared.ErrOutboxNotClaimable.
			WithMessage("outbox record is not claimable in its current state").
			With("recordID", r.ID).
			With("status", string(r.status)).
			With("currentClaimVersion", r.claimVersion).
			With("givenTokenVersion", tokenVersion)
	}
	r.status = OutboxClaimed
	r.claimedBy = claimedBy
	r.claimedAt = now
	r.claimVersion = tokenVersion
	r.replayCount++
	return nil
}

// Complete transitions the aggregate from Claimed to Completed.
// Returns shared.ErrOutboxNotInClaimedState when invoked from any
// other state.
func (r *OutboxRecord) Complete(now time.Time) *shared.BridgeError {
	if r.status != OutboxClaimed {
		return shared.ErrOutboxNotInClaimedState.
			WithMessage("outbox record must be in claimed state to complete").
			With("recordID", r.ID).
			With("status", string(r.status))
	}
	r.status = OutboxCompleted
	r.completedAt = now
	return nil
}

// Expire transitions the aggregate to the Expired terminal state. Valid
// only from Pending or Claimed; returns shared.ErrOutboxAlreadyTerminal
// otherwise.
func (r *OutboxRecord) Expire(_ time.Time) *shared.BridgeError {
	switch r.status {
	case OutboxPending, OutboxClaimed:
		r.status = OutboxExpired
		return nil
	default:
		return shared.ErrOutboxAlreadyTerminal.
			WithMessage("outbox record is already in a terminal state").
			With("recordID", r.ID).
			With("status", string(r.status))
	}
}

// OutboxSnapshot is the persistence DTO used by storage adapters to
// serialize and rehydrate OutboxRecord aggregates without breaching the
// aggregate's encapsulation. It carries the full lifecycle state plus
// identity attributes.
type OutboxSnapshot struct {
	ID              string
	RouteID         string
	EnvelopeID      string
	BindingID       string
	SessionID       string
	Address         string
	Envelope        messaging.Envelope
	DispatchHeaders map[string]any
	Status          OutboxStatus
	ClaimedBy       string
	ClaimedAt       time.Time
	ClaimVersion    uint64
	ReplayCount     int
	CreatedAt       time.Time
	ExpiresAt       time.Time
	CompletedAt     time.Time
}

// Snapshot returns a value copy of the aggregate's full state for
// persistence. Callers must not assume the returned snapshot reflects
// concurrent in-memory mutations.
func (r *OutboxRecord) Snapshot() OutboxSnapshot {
	return OutboxSnapshot{
		ID:              r.ID,
		RouteID:         r.RouteID,
		EnvelopeID:      r.EnvelopeID,
		BindingID:       r.BindingID,
		SessionID:       r.SessionID,
		Address:         r.Address,
		Envelope:        r.Envelope,
		DispatchHeaders: r.DispatchHeaders,
		Status:          r.status,
		ClaimedBy:       r.claimedBy,
		ClaimedAt:       r.claimedAt,
		ClaimVersion:    r.claimVersion,
		ReplayCount:     r.replayCount,
		CreatedAt:       r.CreatedAt,
		ExpiresAt:       r.ExpiresAt,
		CompletedAt:     r.completedAt,
	}
}

// RehydrateFromSnapshot reconstructs an OutboxRecord aggregate from a
// persistence snapshot without invoking the lifecycle state machine.
// Used exclusively by storage adapters when materializing aggregates
// from durable storage; runtime code must use NewOutboxRecord.
func RehydrateFromSnapshot(s OutboxSnapshot) *OutboxRecord {
	status := s.Status
	if status == "" {
		status = OutboxPending
	}
	return &OutboxRecord{
		ID:              s.ID,
		RouteID:         s.RouteID,
		EnvelopeID:      s.EnvelopeID,
		BindingID:       s.BindingID,
		SessionID:       s.SessionID,
		Address:         s.Address,
		Envelope:        s.Envelope,
		DispatchHeaders: s.DispatchHeaders,
		CreatedAt:       s.CreatedAt,
		ExpiresAt:       s.ExpiresAt,
		status:          status,
		claimedBy:       s.ClaimedBy,
		claimedAt:       s.ClaimedAt,
		claimVersion:    s.ClaimVersion,
		replayCount:     s.ReplayCount,
		completedAt:     s.CompletedAt,
	}
}

// MustOutboxRecord is a panicking constructor convenience for tests and
// other contexts where the caller has known-valid inputs. Production
// code must use NewOutboxRecord and handle the typed BridgeError.
func MustOutboxRecord(spec OutboxSpec) *OutboxRecord {
	rec, err := NewOutboxRecord(spec)
	if err != nil {
		panic(err)
	}
	return rec
}
