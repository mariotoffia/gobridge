package dynamodboutbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// This file is the marshaling half of the DynamoDB ACL: it owns the
// conversion between persistence.OutboxRecord and the DynamoDB item
// shape, plus the low-level attribute accessor helpers. Query/update
// orchestration lives in acl_store.go.

// --- marshaling ---

// skSeparator delimits the escaped envelope and binding components inside a
// record sort key. It is the raw '#'; a literal '#' inside either component
// is percent-escaped (see escapeSKComponent), so the FIRST raw '#' after
// skPrefix always marks the component boundary.
const skSeparator = "#"

// sortKey builds the INJECTIVE composite sort key for a record. The envelope
// and binding components are percent-escaped before being joined with the
// skSeparator, so distinct (envelope, binding) pairs can never collapse onto
// the same key.
//
// c13-sk-collision: RAW concatenation ("OUTBOX#"+env+"#"+binding) was NOT
// injective. Within one partition, (env="order", binding="eu#prod") and
// (env="order#eu", binding="prod") both produced "OUTBOX#order#eu#prod", so
// the second DISTINCT record hit attribute_not_exists(SK), was treated as an
// idempotent duplicate, and was acked and DROPPED — silent data loss.
// Envelope IDs are producer-controlled on every transport, so a colliding
// '#'-bearing ID can arrive externally; the collision must be closed HERE, in
// the marshaler, not relied upon to be rejected by out-of-module config
// validation.
//
// Escaping only '%' and '#' keeps the common case (neither component contains
// '#' or '%') byte-for-byte identical to the historical key, so a rolling
// deploy preserves attribute_not_exists(SK) idempotency for existing records
// — only the previously-colliding '#'-bearing keys change shape.
func sortKey(envelopeID, bindingID string) string {
	return skPrefix + escapeSKComponent(envelopeID) + skSeparator + escapeSKComponent(bindingID)
}

// parseSortKey is the inverse of sortKey: it recovers the exact
// (envelopeID, bindingID) pair a sort key was built from, undoing the
// percent-escaping. ok is false when sk is not a well-formed record sort key
// (e.g. the FENCE row, which has no skPrefix/separator). The record's own
// envelope_id/binding_id attributes remain the authoritative source on the
// unmarshal path; parseSortKey exists to prove the SK encoding is injective
// and round-trippable and for key-level diagnostics.
func parseSortKey(sk string) (envelopeID, bindingID string, ok bool) {
	rest, ok := strings.CutPrefix(sk, skPrefix)
	if !ok {
		return "", "", false
	}
	// The separator is the FIRST raw '#': every '#' inside a component was
	// escaped to "%23", so a raw '#' in rest can only be the delimiter.
	idx := strings.Index(rest, skSeparator)
	if idx < 0 {
		return "", "", false
	}
	return unescapeSKComponent(rest[:idx]), unescapeSKComponent(rest[idx+len(skSeparator):]), true
}

// escapeSKComponent percent-escapes the two characters that would otherwise
// make the composite sort key ambiguous: the '#' separator and the '%'
// escape marker itself. '%' MUST be escaped first so a literal '%' in the
// input cannot masquerade as an escape sequence on decode.
func escapeSKComponent(s string) string {
	if !strings.ContainsAny(s, "%#") {
		return s
	}
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "#", "%23")
	return s
}

// unescapeSKComponent reverses escapeSKComponent in a single left-to-right
// pass so that a literal "%23" in the original component (encoded as
// "%2523") round-trips exactly, which naive sequential ReplaceAll decoding
// would corrupt.
func unescapeSKComponent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			switch s[i+1 : i+3] {
			case "23":
				b.WriteByte('#')
				i += 2
				continue
			case "25":
				b.WriteByte('%')
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func partitionKey(r *persistence.OutboxRecord) string {
	return persistence.OutboxPartitionKey(r.SessionID(), r.BindingID())
}

// claimSortKey encodes (createdAtMillis, seq) as a zero-padded,
// lexicographically-sortable string so the ClaimIndex GSI (range=claim_sort)
// orders a partition's records by ascending (CreatedAt, Seq) — the
// ports.OutboxStore claim-ordering contract. 19 digits cover any non-negative
// int64 created_at in millis; 20 digits cover any uint64 seq. A record
// persisted before the sequence existed carries seq 0 and sorts first within
// its millisecond, matching the exhaustive-scan tiebreak.
//
// PRECONDITION: createdAtMillis >= 0. Zero-padded %019d is
// lexically==numerically ordered ONLY for non-negative values — a negative
// millis renders with a leading '-' and would sort AFTER every positive one,
// silently corrupting the age ordering. marshalRecord upholds this by
// substituting `now` for a zero/unset CreatedAt, and real record timestamps
// are post-epoch; the clamp below is a defensive backstop so a pathological
// negative can never poison the index ordering.
func claimSortKey(createdAtMillis int64, seq uint64) string {
	if createdAtMillis < 0 {
		createdAtMillis = 0
	}
	return fmt.Sprintf("%019d#%020d", createdAtMillis, seq)
}

func marshalRecord(r *persistence.OutboxRecord, now time.Time) (map[string]ddbtypes.AttributeValue, error) {
	createdAt := r.CreatedAt()
	if createdAt.IsZero() {
		createdAt = now
	}
	expiresAt := r.ExpiresAt()

	envJSON, err := json.Marshal(r.Snapshot())
	if err != nil {
		return nil, fmt.Errorf("dynamodboutbox: marshal envelope: %w", err)
	}

	item := map[string]ddbtypes.AttributeValue{
		"PK":            &ddbtypes.AttributeValueMemberS{Value: partitionKey(r)},
		"SK":            &ddbtypes.AttributeValueMemberS{Value: sortKey(r.EnvelopeID(), r.BindingID())},
		"record_id":     &ddbtypes.AttributeValueMemberS{Value: r.ID()},
		"route_id":      &ddbtypes.AttributeValueMemberS{Value: r.RouteID()},
		"envelope_id":   &ddbtypes.AttributeValueMemberS{Value: r.EnvelopeID()},
		"binding_id":    &ddbtypes.AttributeValueMemberS{Value: r.BindingID()},
		"session_id":    &ddbtypes.AttributeValueMemberS{Value: r.SessionID()},
		"address":       &ddbtypes.AttributeValueMemberS{Value: r.Address()},
		"envelope_json": &ddbtypes.AttributeValueMemberS{Value: string(envJSON)},
		"status":        &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		"claim_version": &ddbtypes.AttributeValueMemberN{Value: "0"},
		"replay_count":  &ddbtypes.AttributeValueMemberN{Value: "0"},
		"created_at":    &ddbtypes.AttributeValueMemberN{Value: i64(createdAt.UnixMilli())},
		"expires_at":    &ddbtypes.AttributeValueMemberN{Value: i64(millisOrZero(expiresAt))},
		"completed_at":  &ddbtypes.AttributeValueMemberN{Value: "0"},
	}
	// claimed_by and claimed_at are omitted for unclaimed records; Claim
	// sets them and Release removes them again. The seq attribute is
	// stamped by Persist after allocating the per-partition sequence.
	//
	// first_attempted_at is likewise omitted while zero (never claimed):
	// Claim stamps it once via if_not_exists and it is never moved after, so an
	// item without the attribute is one that has never been claimed and
	// unmarshalRecord reads it back as the zero time (numAttrI64 → 0).
	if fa := r.FirstAttemptedAt(); !fa.IsZero() {
		item["first_attempted_at"] = &ddbtypes.AttributeValueMemberN{Value: i64(fa.UnixMilli())}
	}

	// ordering_key is denormalised out of the envelope so a claim can evaluate
	// the head-of-line rule without unmarshalling every scanned record. It is
	// written only when the envelope carries one, so keyless records add no
	// attribute, and it is never rewritten — the envelope is immutable, so the
	// key cannot change under a record. Records written before this attribute
	// existed are read back through the envelope fallback in orderingKeyOfItem.
	if key, ok := r.OrderingKey(); ok {
		item[attrOrderingKey] = &ddbtypes.AttributeValueMemberS{Value: key}
	}

	// has_expiry is the sparse ExpiryIndex hash key: present only on
	// records that can ever expire, so Expire scans exactly its candidate
	// set. Terminal transitions (Complete/Expire) remove it.
	if !expiresAt.IsZero() {
		item[attrHasExpiry] = &ddbtypes.AttributeValueMemberS{Value: hasExpiryFlag}
	}

	if dh := r.DispatchHeaders(); dh != nil {
		hdrJSON, err := json.Marshal(dh)
		if err != nil {
			return nil, fmt.Errorf("dynamodboutbox: marshal headers: %w", err)
		}
		item["headers_json"] = &ddbtypes.AttributeValueMemberS{Value: string(hdrJSON)}
	}

	// Pending (and claimed) records carry NO ttl. DynamoDB item-TTL is a
	// physical-compaction convenience for TERMINAL records only: Complete and
	// Expire each stamp a fresh #ttl (now + compactGrace) at the terminal
	// transition. A ttl on a still-undelivered record would let DynamoDB reap
	// live work — e.g. an on_expired=dlq record that expired during an egress
	// outage, which the drainer defers to the claim path instead of sweeping
	// (runtime/outbox/loop.go maybeExpire). memory and sqlite never evict pending
	// rows either; omitting the pending ttl keeps the three outbox stores
	// consistent and loss-free.

	return item, nil
}

func unmarshalRecord(item map[string]ddbtypes.AttributeValue) (*persistence.OutboxRecord, error) {
	snap := persistence.OutboxSnapshot{
		ID:           strAttr(item, "record_id"),
		RouteID:      strAttr(item, "route_id"),
		EnvelopeID:   strAttr(item, "envelope_id"),
		BindingID:    strAttr(item, "binding_id"),
		SessionID:    strAttr(item, "session_id"),
		Address:      strAttr(item, "address"),
		Status:       persistence.OutboxStatus(strAttr(item, "status")),
		ClaimedBy:    strAttr(item, "claimed_by"),
		ClaimVersion: numAttrU64(item, "claim_version"),
		ReplayCount:  int(numAttrI64(item, "replay_count")),
		Seq:          numAttrU64(item, "seq"),
		CreatedAt:    timeFromMillis(numAttrI64(item, "created_at")),
		ClaimedAt:    timeFromMillis(numAttrI64(item, "claimed_at")),
		// Missing attribute (legacy or never-claimed) → numAttrI64 returns 0 →
		// timeFromMillis(0) is the zero time. Never now-stamped here.
		FirstAttemptedAt: timeFromMillis(numAttrI64(item, "first_attempted_at")),
		CompletedAt:      timeFromMillis(numAttrI64(item, "completed_at")),
	}

	if expiresMs := numAttrI64(item, "expires_at"); expiresMs > 0 {
		snap.ExpiresAt = timeFromMillis(expiresMs)
	}

	if envJSON := strAttr(item, "envelope_json"); envJSON != "" {
		if err := json.Unmarshal([]byte(envJSON), &snap.Envelope); err != nil {
			return nil, fmt.Errorf("dynamodboutbox: unmarshal envelope: %w", err)
		}
	}

	if hdrJSON := strAttr(item, "headers_json"); hdrJSON != "" {
		if err := json.Unmarshal([]byte(hdrJSON), &snap.DispatchHeaders); err != nil {
			return nil, fmt.Errorf("dynamodboutbox: unmarshal headers: %w", err)
		}
	}

	return persistence.RehydrateFromSnapshot(snap), nil
}

// --- attribute helpers ---

func strAttr(item map[string]ddbtypes.AttributeValue, key string) string {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func numAttrI64(item map[string]ddbtypes.AttributeValue, key string) int64 {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberN); ok {
		n, _ := strconv.ParseInt(v.Value, 10, 64)
		return n
	}
	return 0
}

func numAttrU64(item map[string]ddbtypes.AttributeValue, key string) uint64 {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberN); ok {
		n, _ := strconv.ParseUint(v.Value, 10, 64)
		return n
	}
	return 0
}

func i64(n int64) string {
	return strconv.FormatInt(n, 10)
}

func u64(n uint64) string {
	return strconv.FormatUint(n, 10)
}

func millisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeFromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
