package sqs

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// subjectAttributeSize is the byte size the reserved "Subject" attribute
// contributes to the SQS message-size budget: attribute name + "String"
// data type + subject value, mirroring the name-inclusive accounting
// headersToAttributes applies to every other candidate.
func subjectAttributeSize(subject string) int {
	return len(sqsSubjectAttributeName) + len("String") + len(subject)
}

// attributeValue builds the SQS MessageAttributeValue for a header value
// and reports its approximate byte size (type name + value bytes). It
// returns ok=false for value types SQS cannot carry as an attribute.
func attributeValue(v any) (sqstypes.MessageAttributeValue, int, bool) {
	switch val := v.(type) {
	case string:
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(val),
		}, len("String") + len(val), true
	case []byte:
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("Binary"),
			BinaryValue: val,
		}, len("Binary") + len(val), true
	case int, int32, int64, float32, float64:
		s := fmt.Sprintf("%v", val)
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("Number"),
			StringValue: aws.String(s),
		}, len("Number") + len(s), true
	case time.Time:
		s := val.Format(time.RFC3339Nano)
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(s),
		}, len("String") + len(s), true
	case bool:
		s := fmt.Sprintf("%t", val)
		return sqstypes.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(s),
		}, len("String") + len(s), true
	default:
		return sqstypes.MessageAttributeValue{}, 0, false
	}
}

// isValidSQSAttributeName reports whether name is a legal SQS message
// attribute name: 1-256 chars from [A-Za-z0-9_.-], no AWS./Amazon.
// (case-insensitive) reserved prefix, and no leading, trailing or
// consecutive periods.
func isValidSQSAttributeName(name string) bool {
	if name == "" || len(name) > sqsMaxAttributeNameLen {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	if hasFoldPrefix(name, "aws.") || hasFoldPrefix(name, "amazon.") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// hasFoldPrefix reports whether s starts with prefix, case-insensitively.
func hasFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// extractFIFOFields pulls MessageGroupId and MessageDeduplicationId from
// envelope headers. Returns empty strings when not present.
func extractFIFOFields(headers map[string]any) (groupID, dedupID string) {
	if headers == nil {
		return "", ""
	}
	if v, ok := headers[messaging.HeaderOrderingKey]; ok {
		if s, ok := v.(string); ok {
			groupID = s
		}
	}
	if v, ok := headers[messaging.HeaderDeduplicationID]; ok {
		if s, ok := v.(string); ok {
			dedupID = s
		}
	}
	return groupID, dedupID
}

// generateDeduplicationID derives a stable FIFO dedup id from the
// envelope payload, subject and id. md5 is sufficient — SQS only uses
// the value as an opaque key for dedup, not for security.
func generateDeduplicationID(env *messaging.Envelope) string {
	h := md5.New()
	h.Write(env.Payload())
	h.Write([]byte(env.Subject()))
	if env.ID() != "" {
		h.Write([]byte(env.ID()))
	} else {
		h.Write([]byte(env.CreatedAt().String()))
	}
	return hex.EncodeToString(h.Sum(nil))
}
