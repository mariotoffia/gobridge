package sqs

import (
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
)

// attributesToHeaders converts SQS message attributes and system attributes
// into an Envelope headers map. Headers with the reserved x-bridge.* prefix
// are stripped to prevent injection from external sources.
func attributesToHeaders(
	msgAttrs map[string]sqstypes.MessageAttributeValue,
	sysAttrs map[string]string,
) map[string]any {
	h := make(map[string]any, len(msgAttrs)+len(sysAttrs))

	for k, attr := range msgAttrs {
		if domain.IsReservedHeader(k) {
			continue
		}
		switch {
		case attr.StringValue != nil:
			h[k] = *attr.StringValue
		case attr.BinaryValue != nil:
			h[k] = attr.BinaryValue
		}
	}

	for k, v := range sysAttrs {
		key := "sqs." + k
		if k == "SentTimestamp" {
			if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
				h[key] = time.UnixMilli(ms)
				continue
			}
		}
		if k == "ApproximateReceiveCount" {
			if n, err := strconv.Atoi(v); err == nil {
				h[key] = n
				continue
			}
		}
		h[key] = v
	}

	return h
}

// headersToAttributes converts Envelope headers into SQS message attributes.
// FIFO-specific fields (MessageGroupId, MessageDeduplicationId) are extracted
// via extractFIFOFields separately and must not appear in the attribute map.
func headersToAttributes(headers map[string]any) map[string]sqstypes.MessageAttributeValue {
	if len(headers) == 0 {
		return nil
	}

	attrs := make(map[string]sqstypes.MessageAttributeValue, len(headers))

	for k, v := range headers {
		if k == domain.HeaderOrderingKey || k == domain.HeaderDeduplicationID {
			continue
		}

		switch val := v.(type) {
		case string:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(val),
			}
		case []byte:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("Binary"),
				BinaryValue: val,
			}
		case int, int32, int64, float32, float64:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("Number"),
				StringValue: aws.String(fmt.Sprintf("%v", val)),
			}
		case time.Time:
			attrs[k] = sqstypes.MessageAttributeValue{
				DataType:    aws.String("String"),
				StringValue: aws.String(val.Format(time.RFC3339Nano)),
			}
		}
	}

	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

// extractFIFOFields pulls MessageGroupId and MessageDeduplicationId from
// envelope headers. Returns empty strings when not present.
func extractFIFOFields(headers map[string]any) (groupID, dedupID string) {
	if headers == nil {
		return "", ""
	}
	if v, ok := headers[domain.HeaderOrderingKey]; ok {
		if s, ok := v.(string); ok {
			groupID = s
		}
	}
	if v, ok := headers[domain.HeaderDeduplicationID]; ok {
		if s, ok := v.(string); ok {
			dedupID = s
		}
	}
	return groupID, dedupID
}
