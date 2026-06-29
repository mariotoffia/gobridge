package dynamodboutbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/persistence"
)

// This file is the marshaling half of the DynamoDB ACL: it owns the
// conversion between persistence.OutboxRecord and the DynamoDB item
// shape, plus the low-level attribute accessor helpers. Query/update
// orchestration lives in acl_store.go.

// --- marshaling ---

func sortKey(envelopeID, bindingID string) string {
	return skPrefix + envelopeID + "#" + bindingID
}

func partitionKey(r *persistence.OutboxRecord) string {
	return persistence.OutboxPartitionKey(r.SessionID(), r.BindingID())
}

func marshalRecord(r *persistence.OutboxRecord, now time.Time, compactGrace time.Duration) (map[string]ddbtypes.AttributeValue, error) {
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
	// Omit claimed_by and claimed_at for unclaimed records — DynamoDB
	// sparse GSI semantics: items without the GSI key attributes are
	// excluded from the ClaimedByIndex, which is exactly what we want.

	if dh := r.DispatchHeaders(); dh != nil {
		hdrJSON, err := json.Marshal(dh)
		if err != nil {
			return nil, fmt.Errorf("dynamodboutbox: marshal headers: %w", err)
		}
		item["headers_json"] = &ddbtypes.AttributeValueMemberS{Value: string(hdrJSON)}
	}

	if !expiresAt.IsZero() {
		item["ttl"] = &ddbtypes.AttributeValueMemberN{
			Value: i64(expiresAt.Add(compactGrace).Unix()),
		}
	}

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
		CreatedAt:    timeFromMillis(numAttrI64(item, "created_at")),
		ClaimedAt:    timeFromMillis(numAttrI64(item, "claimed_at")),
		CompletedAt:  timeFromMillis(numAttrI64(item, "completed_at")),
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
