package dynamodbdlq

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/routing"
)

func unmarshalEntry(item map[string]ddbtypes.AttributeValue) (routing.DLQEntry, error) {
	var spec routing.DLQEntrySpec

	pk := strAttr(item, attrPK)
	if len(pk) > 4 {
		spec.ID = pk[4:] // strip "DLQ#" prefix
	}

	spec.RouteID = strAttr(item, attrRouteID)
	spec.BindingID = strAttr(item, attrBindingID)
	spec.SessionID = strAttr(item, attrSessionID)
	spec.SourceID = strAttr(item, attrSourceID)
	spec.CorrelationID = strAttr(item, attrCorrelationID)
	spec.Address = strAttr(item, attrAddress)
	spec.Reason = strAttr(item, attrReason)
	spec.Category = strAttr(item, attrCategory)
	spec.ErrorCode = strAttr(item, attrErrorCode)
	spec.LastError = strAttr(item, attrLastError)
	spec.FailedAt = timeFromMillis(numAttrI64(item, attrFailedAt))
	spec.Attempts = int(numAttrI64(item, attrAttempts))

	if envJSON := strAttr(item, attrEnvelopeJSON); envJSON != "" {
		if err := json.Unmarshal([]byte(envJSON), &spec.Envelope); err != nil {
			return routing.DLQEntry{}, fmt.Errorf("dynamodbdlq: unmarshal envelope: %w", err)
		}
	}

	spec.RedriveMode = routing.RedriveMode(strAttr(item, attrRedriveMode))
	if infoJSON := strAttr(item, attrExtraInfo); infoJSON != "" {
		if err := json.Unmarshal([]byte(infoJSON), &spec.ExtraInfo); err != nil {
			return routing.DLQEntry{}, fmt.Errorf("dynamodbdlq: unmarshal extra_info: %w", err)
		}
	}

	// RehydrateDLQEntry: the envelope was freshly decoded and is already
	// owned, so the entry takes it without a redundant clone.
	return routing.RehydrateDLQEntry(spec), nil
}

// putRedriveAttrs adds the optional redrive fields (ADR 0019) to an item: a
// manual record with no facts carries neither attribute, exactly like an item a
// release before them wrote.
func putRedriveAttrs(item map[string]ddbtypes.AttributeValue, entry routing.DLQEntry) error {
	if mode := entry.RedriveMode(); mode != routing.RedriveManual {
		item[attrRedriveMode] = &ddbtypes.AttributeValueMemberS{Value: string(mode)}
	}
	if info := entry.ExtraInfo(); len(info) > 0 {
		b, err := json.Marshal(info)
		if err != nil {
			return fmt.Errorf("dynamodbdlq: marshal extra_info: %w", err)
		}
		item[attrExtraInfo] = &ddbtypes.AttributeValueMemberS{Value: string(b)}
	}
	return nil
}

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

func i64(n int64) string {
	return strconv.FormatInt(n, 10)
}

func timeFromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func dlqKey(entryID string) string {
	return "DLQ#" + entryID
}
