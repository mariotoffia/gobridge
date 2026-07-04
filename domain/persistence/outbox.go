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
// reliable-egress path. All aggregate state is private: identity
// attributes are immutable after construction and exposed only through
// read-only value accessors (ID, RouteID, EnvelopeID, BindingID,
// SessionID, Address, CreatedAt, ExpiresAt, DispatchHeaders as a deep
// copy, and the embedded envelope via Snapshot); lifecycle state is
// transitioned solely through the aggregate's state-machine methods
// (Claim, Complete, Expire). Storage adapters cross the persistence
// boundary via PersistenceSnapshot / RehydrateFromSnapshot and never
// touch fields directly, so caller-side mutation can never corrupt
// persisted or rehydrated state.
//
// aggregate-root
type OutboxRecord struct {
	// Identity (immutable after construction; read via accessors).
	id              string
	routeID         string
	envelopeID      string
	bindingID       string
	sessionID       string
	address         string
	envelope        messaging.Envelope
	dispatchHeaders map[string]any
	createdAt       time.Time
	expiresAt       time.Time

	// seq is the monotonic per-partition sequence assigned by the outbox
	// store at Persist time. It is zero until the record has been
	// persisted and rehydrated; the aggregate never assigns it. Stores
	// use it as the claim-order tiebreaker when CreatedAt collides at
	// millisecond resolution (UBIQUITOUS.md: Outbox / Drain ordering).
	seq uint64

	// Lifecycle state (mutated only through aggregate methods).
	status    OutboxStatus
	claimedBy string
	claimedAt time.Time

	// firstAttemptedAt is the wall-clock instant the drainer FIRST claimed
	// this record. It is stamped exactly once, inside Claim, the first time
	// the record leaves Pending, and is never moved by a later Release,
	// re-claim, or stale-claim reclaim by a newer fencing token. A claim
	// proves the drainer was alive and took ownership to attempt delivery —
	// even a claim that ends in a batch-deadline deferral counts — so this is
	// the honest clock the replay budget runs from. Zero for records
	// persisted before the replay-budget schema or never yet claimed; those
	// fall back to the CreatedAt age gate (poisonMinAge).
	firstAttemptedAt time.Time

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
		envelopeID = spec.Envelope.ID()
	}
	if envelopeID == "" {
		return nil, shared.ErrInvalidOutboxRecord.WithMessage("outbox record envelope ID is required")
	}
	if spec.SessionID == "" && spec.BindingID == "" {
		return nil, shared.ErrInvalidOutboxRecord.
			WithMessage("outbox record requires session ID or binding ID for partition key")
	}
	return &OutboxRecord{
		id:              spec.ID,
		routeID:         spec.RouteID,
		envelopeID:      envelopeID,
		bindingID:       spec.BindingID,
		sessionID:       spec.SessionID,
		address:         spec.Address,
		envelope:        *spec.Envelope.Clone(),
		dispatchHeaders: messaging.Headers(spec.DispatchHeaders).Snapshot(),
		createdAt:       spec.CreatedAt,
		expiresAt:       spec.ExpiresAt,
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

// FirstAttemptedAt returns the instant the drainer FIRST claimed this record,
// stamped once inside Claim and never moved by a later release or reclaim. It
// is the clock the replay budget runs from. Zero for records persisted before
// the replay-budget schema or never yet claimed; the drainer falls those back
// to the CreatedAt age gate (poisonMinAge).
func (r *OutboxRecord) FirstAttemptedAt() time.Time { return r.firstAttemptedAt }

// ClaimVersion returns the fencing-token version recorded by the last
// successful claim. Used by stores to detect stale completers.
func (r *OutboxRecord) ClaimVersion() uint64 { return r.claimVersion }

// ReplayCount returns the number of times this record has been claimed
// (and therefore attempted). Incremented atomically inside Claim.
func (r *OutboxRecord) ReplayCount() int { return r.replayCount }

// CompletedAt returns the completion timestamp; zero if not completed.
func (r *OutboxRecord) CompletedAt() time.Time { return r.completedAt }

// ID returns the record's immutable identity.
func (r *OutboxRecord) ID() string { return r.id }

// RouteID returns the ID of the route that produced this record.
func (r *OutboxRecord) RouteID() string { return r.routeID }

// EnvelopeID returns the ID of the embedded envelope.
func (r *OutboxRecord) EnvelopeID() string { return r.envelopeID }

// BindingID returns the destination binding ID for this record.
func (r *OutboxRecord) BindingID() string { return r.bindingID }

// SessionID returns the session ID component of the partition key.
func (r *OutboxRecord) SessionID() string { return r.sessionID }

// Address returns the transport destination address resolved at egress.
func (r *OutboxRecord) Address() string { return r.address }

// CreatedAt returns the time the record was created.
func (r *OutboxRecord) CreatedAt() time.Time { return r.createdAt }

// Seq returns the monotonic per-partition persistence sequence assigned
// by the outbox store when the record was persisted. Zero for records
// that have not round-tripped through a store (or rows persisted before
// the sequence column existed). Ordering is (CreatedAt, Seq): Seq only
// breaks ties between records created within the same millisecond.
func (r *OutboxRecord) Seq() uint64 { return r.seq }

// ExpiresAt returns the record's expiry time; zero when it never expires.
func (r *OutboxRecord) ExpiresAt() time.Time { return r.expiresAt }

// DispatchHeaders returns a deep copy of the per-message dispatch headers
// merged onto the outbound envelope at send time. The copy isolates the
// aggregate's internal map: mutating the result cannot corrupt the record.
// Returns nil when no dispatch headers were supplied.
func (r *OutboxRecord) DispatchHeaders() map[string]any {
	return messaging.Headers(r.dispatchHeaders).Snapshot()
}

// OrderingKey returns the per-message ordering key (the
// messaging.HeaderOrderingKey header carried by the embedded envelope)
// and whether a non-empty key is present. Unlike Snapshot it does NOT
// clone the envelope: it is a read-only peek used by the outbox drainer
// to group records for per-key in-order delivery on the hot path. The
// returned flag is false when the header is absent, non-string, or empty.
func (r *OutboxRecord) OrderingKey() (string, bool) {
	key, ok := r.envelope.Headers().GetString(messaging.HeaderOrderingKey)
	if !ok || key == "" {
		return "", false
	}
	return key, true
}

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
// newer fencing token.
//
// ReplayCount is incremented on every successful claim. It counts the number
// of times the record was CLAIMED, NOT the number of failed send attempts:
// batch-deadline deferrals (claim → release → re-claim) and stale-claim
// reclaims by a newer fencing token also increment it even though no send ever
// failed. Consequently replay-count exhaustion alone is not proof the record is
// poison — a chronically deferred record can cross the cap failure-free. That
// is why the drainer's poison decision AND-gates replay exhaustion with a
// replay-budget check (runtime/outbox: replayBudgetExhausted): a record is only
// routed to the DLQ once wall-clock time since FirstAttemptedAt exceeds
// RoutePolicy.ReplayBudget. Legacy records with a zero FirstAttemptedAt fall
// back to the older CreatedAt age gate (poisonMinAge). Callers enforcing a
// poison-message cap MUST apply the same budget gate rather than poisoning on
// replay count alone.
func (r *OutboxRecord) Claim(now time.Time, claimedBy string, tokenVersion uint64) *shared.BridgeError {
	if !r.IsClaimable(tokenVersion) {
		return shared.ErrOutboxNotClaimable.
			WithMessage("outbox record is not claimable in its current state").
			With("recordID", r.id).
			With("status", string(r.status)).
			With("currentClaimVersion", r.claimVersion).
			With("givenTokenVersion", tokenVersion)
	}
	r.status = OutboxClaimed
	r.claimedBy = claimedBy
	r.claimedAt = now
	if r.firstAttemptedAt.IsZero() {
		// Stamp the first-attempt instant exactly once, at the first claim.
		// A deferring claim still proves the drainer was alive and took
		// ownership, so the first claim — not the first successful send —
		// anchors the replay budget. Never moved by a later release/reclaim.
		r.firstAttemptedAt = now
	}
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
			With("recordID", r.id).
			With("status", string(r.status))
	}
	r.status = OutboxCompleted
	r.completedAt = now
	return nil
}

// Release returns a Claimed record to Pending so the same owner can
// re-claim and retry it on the next drain after a transient egress
// failure, without waiting for a fencing-version bump or wall-clock
// stale-claim timeout. It does NOT change replayCount (the next Claim
// increments it, preserving the poison-message cap). Returns
// shared.ErrOutboxNotInClaimedState when invoked from any other state.
func (r *OutboxRecord) Release(_ time.Time) *shared.BridgeError {
	if r.status != OutboxClaimed {
		return shared.ErrOutboxNotInClaimedState.
			WithMessage("outbox record must be in claimed state to release").
			With("recordID", r.id).
			With("status", string(r.status))
	}
	r.status = OutboxPending
	r.claimedBy = ""
	r.claimedAt = time.Time{}
	// claimVersion is intentionally left as-is: it is irrelevant once the
	// record is Pending and is overwritten by the next Claim.
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
			With("recordID", r.id).
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
	// FirstAttemptedAt is the instant the drainer first claimed the record.
	// Zero for records persisted before the replay-budget schema or never
	// yet claimed. Stores must round-trip it verbatim (zero stays zero) and
	// MUST NOT now-stamp it on marshal/unmarshal.
	FirstAttemptedAt time.Time
	ClaimVersion     uint64
	ReplayCount      int
	CreatedAt        time.Time
	ExpiresAt        time.Time
	CompletedAt      time.Time
	// Seq is the monotonic per-partition sequence the store assigned at
	// Persist. Stores populate it when rehydrating; it is ignored on the
	// initial Persist (the store allocates it).
	Seq uint64
}

// Snapshot returns a deep copy of the aggregate's embedded envelope.
// Use it whenever the envelope must outlive the aggregate or be handed
// to mutator code (DLQ writer, retry submitter) so that subsequent
// mutations cannot corrupt the persisted aggregate state. This satisfies
// the DDD R5 aggregate-boundary rule: callers receive an isolated
// messaging.Envelope rather than a shared reference.
func (r *OutboxRecord) Snapshot() *messaging.Envelope {
	return r.envelope.Clone()
}

// PersistenceSnapshot returns a value copy of the aggregate's full state for
// persistence. Callers must not assume the returned snapshot reflects
// concurrent in-memory mutations. The embedded Envelope is deep-cloned so
// the snapshot can be serialized or rehydrated without aliasing the
// aggregate's internal state.
func (r *OutboxRecord) PersistenceSnapshot() OutboxSnapshot {
	return OutboxSnapshot{
		ID:               r.id,
		RouteID:          r.routeID,
		EnvelopeID:       r.envelopeID,
		BindingID:        r.bindingID,
		SessionID:        r.sessionID,
		Address:          r.address,
		Envelope:         *r.envelope.Clone(),
		DispatchHeaders:  messaging.Headers(r.dispatchHeaders).Snapshot(),
		Status:           r.status,
		ClaimedBy:        r.claimedBy,
		ClaimedAt:        r.claimedAt,
		FirstAttemptedAt: r.firstAttemptedAt,
		ClaimVersion:     r.claimVersion,
		ReplayCount:      r.replayCount,
		CreatedAt:        r.createdAt,
		ExpiresAt:        r.expiresAt,
		CompletedAt:      r.completedAt,
		Seq:              r.seq,
	}
}

// RehydrateFromSnapshot reconstructs an OutboxRecord aggregate from a
// persistence snapshot without invoking the lifecycle state machine.
// Used exclusively by storage adapters when materializing aggregates
// from durable storage; runtime code must use NewOutboxRecord. The
// snapshot's Envelope is deep-cloned so the rehydrated aggregate does
// not alias state owned by the storage adapter.
func RehydrateFromSnapshot(s OutboxSnapshot) *OutboxRecord {
	status := s.Status
	if status == "" {
		status = OutboxPending
	}
	return &OutboxRecord{
		id:               s.ID,
		routeID:          s.RouteID,
		envelopeID:       s.EnvelopeID,
		bindingID:        s.BindingID,
		sessionID:        s.SessionID,
		address:          s.Address,
		envelope:         *s.Envelope.Clone(),
		dispatchHeaders:  messaging.Headers(s.DispatchHeaders).Snapshot(),
		createdAt:        s.CreatedAt,
		expiresAt:        s.ExpiresAt,
		seq:              s.Seq,
		status:           status,
		claimedBy:        s.ClaimedBy,
		claimedAt:        s.ClaimedAt,
		firstAttemptedAt: s.FirstAttemptedAt,
		claimVersion:     s.ClaimVersion,
		replayCount:      s.ReplayCount,
		completedAt:      s.CompletedAt,
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
