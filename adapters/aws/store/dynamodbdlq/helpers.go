package dynamodbdlq

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain"
)

func unmarshalEntry(item map[string]ddbtypes.AttributeValue) (domain.DLQEntry, error) {
	var e domain.DLQEntry

	pk := strAttr(item, attrPK)
	if len(pk) > 4 {
		e.ID = pk[4:] // strip "DLQ#" prefix
	}

	e.RouteID = strAttr(item, attrRouteID)
	e.BindingID = strAttr(item, attrBindingID)
	e.SessionID = strAttr(item, attrSessionID)
	e.SourceID = strAttr(item, attrSourceID)
	e.CorrelationID = strAttr(item, attrCorrelationID)
	e.Reason = strAttr(item, attrReason)
	e.Category = strAttr(item, attrCategory)
	e.ErrorCode = strAttr(item, attrErrorCode)
	e.LastError = strAttr(item, attrLastError)
	e.FailedAt = timeFromMillis(numAttrI64(item, attrFailedAt))
	e.Attempts = int(numAttrI64(item, attrAttempts))

	if envJSON := strAttr(item, attrEnvelopeJSON); envJSON != "" {
		if err := json.Unmarshal([]byte(envJSON), &e.Envelope); err != nil {
			return e, fmt.Errorf("dynamodbdlq: unmarshal envelope: %w", err)
		}
	}

	return e, nil
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
