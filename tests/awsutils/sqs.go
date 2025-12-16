// Package awsutils provides test utilities for AWS service testing.
package awsutils

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
)

// SqsErrors exposes helpers that simulate AWS SQS error responses
// through the RoundTripperTransaction abstraction.
//
// Usage:
//
//	rt := NewRoundTripper(nil)
//	rt.Enable().Push(SqsErrors{}.OverLimit())
type SqsErrors struct{}

// ============================================================================
// Retryable Errors
// ============================================================================

// OverLimit simulates an SQS OverLimit error (throttling).
// This occurs when too many requests are made in a short period.
//
// NOTE: This is retryable
func (SqsErrors) OverLimit() RoundTripperTransaction {
	return newSqsError(400, "Sender", "OverLimit", "Simulated over limit")
}

// ServiceUnavailable simulates a transient SQS service outage.
//
// NOTE: This is retryable
func (SqsErrors) ServiceUnavailable() RoundTripperTransaction {
	return newSqsError(503, "Receiver", "ServiceUnavailable", "Simulated service unavailable")
}

// InternalError simulates a transient SQS internal server fault.
//
// NOTE: This is retryable
func (SqsErrors) InternalError() RoundTripperTransaction {
	return newSqsError(500, "Receiver", "InternalError", "Simulated internal server error")
}

// ThrottlingException simulates SQS rate limiting.
//
// NOTE: This is retryable
func (SqsErrors) ThrottlingException() RoundTripperTransaction {
	return newSqsError(400, "Sender", "ThrottlingException", "Simulated throttling")
}

// RequestThrottled simulates SQS request throttling at the account level.
//
// NOTE: This is retryable
func (SqsErrors) RequestThrottled() RoundTripperTransaction {
	return newSqsError(400, "Sender", "RequestThrottled", "Simulated account request throttled")
}

// ============================================================================
// Non-Retryable Errors
// ============================================================================

// QueueDoesNotExist simulates an operation performed against a missing queue.
//
// NOTE: This is NOT retryable
func (SqsErrors) QueueDoesNotExist() RoundTripperTransaction {
	return newSqsError(400, "Sender", "QueueDoesNotExist", "Simulated queue missing")
}

// QueueDeletedRecently simulates SQS rejecting creation of a queue
// that was recently deleted.
//
// NOTE: This is NOT retryable
func (SqsErrors) QueueDeletedRecently() RoundTripperTransaction {
	return newSqsError(400, "Sender", "QueueDeletedRecently", "Simulated queue deleted recently")
}

// ReceiptHandleInvalid simulates acknowledgement with an invalid receipt handle.
//
// NOTE: This is NOT retryable
func (SqsErrors) ReceiptHandleInvalid() RoundTripperTransaction {
	return newSqsError(400, "Sender", "ReceiptHandleIsInvalid", "Simulated receipt handle invalid")
}

// MessageNotInflight simulates acknowledgement of a message that is not in-flight.
//
// NOTE: This is NOT retryable
func (SqsErrors) MessageNotInflight() RoundTripperTransaction {
	return newSqsError(400, "Sender", "MessageNotInflight", "Simulated message not in flight")
}

// BatchEntryIdsNotDistinct simulates a malformed batch request (duplicate entry IDs).
//
// NOTE: This is NOT retryable
func (SqsErrors) BatchEntryIdsNotDistinct() RoundTripperTransaction {
	return newSqsError(400, "Sender", "BatchEntryIdsNotDistinct", "Simulated duplicate batch entry ids")
}

// TooManyEntriesInBatchRequest simulates a batch request that exceeds the allowed entry count.
//
// NOTE: This is NOT retryable
func (SqsErrors) TooManyEntriesInBatchRequest() RoundTripperTransaction {
	return newSqsError(400, "Sender", "TooManyEntriesInBatchRequest", "Simulated too many entries in batch")
}

// InvalidMessageContents simulates a send operation where the message body or attributes are invalid.
//
// NOTE: This is NOT retryable
func (SqsErrors) InvalidMessageContents() RoundTripperTransaction {
	return newSqsError(400, "Sender", "InvalidMessageContents", "Simulated invalid message contents")
}

// PurgeQueueInProgress simulates a purge already in progress on the target queue.
//
// NOTE: This is NOT retryable
func (SqsErrors) PurgeQueueInProgress() RoundTripperTransaction {
	return newSqsError(403, "Sender", "PurgeQueueInProgress", "Simulated purge already in progress")
}

// UnsupportedOperation simulates an attempt to invoke an operation
// that the queue does not support.
//
// NOTE: This is NOT retryable
func (SqsErrors) UnsupportedOperation() RoundTripperTransaction {
	return newSqsError(400, "Sender", "UnsupportedOperation", "Simulated unsupported operation")
}

// AccessDenied simulates an access denied error.
//
// NOTE: This is NOT retryable
func (SqsErrors) AccessDenied() RoundTripperTransaction {
	return newSqsError(403, "Sender", "AccessDenied", "Simulated access denied")
}

// EmptyBatchRequest simulates an empty batch request error.
//
// NOTE: This is NOT retryable
func (SqsErrors) EmptyBatchRequest() RoundTripperTransaction {
	return newSqsError(400, "Sender", "EmptyBatchRequest", "Simulated empty batch request")
}

// BatchRequestTooLong simulates a batch request that exceeds size limits.
//
// NOTE: This is NOT retryable
func (SqsErrors) BatchRequestTooLong() RoundTripperTransaction {
	return newSqsError(400, "Sender", "BatchRequestTooLong", "Simulated batch request too long")
}

// ============================================================================
// Network Errors
// ============================================================================

// NetworkError returns a transaction that simulates a network connection error.
// This is useful for testing connection recovery behavior.
//
// NOTE: This is retryable
func (SqsErrors) NetworkError() RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: errors.New("connection refused"),
		},
	}
}

// ConnectionReset returns a transaction that simulates a connection reset error.
//
// NOTE: This is retryable
func (SqsErrors) ConnectionReset() RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: &net.OpError{
			Op:  "read",
			Net: "tcp",
			Err: errors.New("connection reset by peer"),
		},
	}
}

// DNSError returns a transaction that simulates a DNS resolution failure.
//
// NOTE: This is retryable
func (SqsErrors) DNSError() RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: &net.DNSError{
			Err:  "no such host",
			Name: "sqs.us-east-1.amazonaws.com",
		},
	}
}

// Timeout returns a transaction that simulates a request timeout.
//
// NOTE: This is retryable
func (SqsErrors) Timeout() RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: timeoutError{},
	}
}

// ============================================================================
// Custom Error Helpers
// ============================================================================

// CustomError creates a transaction with a custom error code and message.
func (SqsErrors) CustomError(status int, errorType, code, message string) RoundTripperTransaction {
	return newSqsError(status, errorType, code, message)
}

// CustomNetworkError creates a transaction with a custom network error.
func (SqsErrors) CustomNetworkError(err error) RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: err,
	}
}

// ============================================================================
// Success Response Helpers
// ============================================================================

// SendMessageSuccess creates a successful SendMessage response.
func (SqsErrors) SendMessageSuccess(messageID string) RoundTripperTransaction {
	body := map[string]string{
		"MessageId":        messageID,
		"MD5OfMessageBody": "d41d8cd98f00b204e9800998ecf8427e",
	}
	data, _ := json.Marshal(body)
	return RoundTripperTransaction{
		Status: 200,
		Body:   string(data),
	}
}

// ReceiveMessageEmpty creates an empty ReceiveMessage response (no messages).
func (SqsErrors) ReceiveMessageEmpty() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{"Messages":[]}`,
	}
}

// ============================================================================
// Internal Helpers
// ============================================================================

// newSqsError creates a RoundTripperTransaction for an SQS error response.
func newSqsError(status int, errorType, code, message string) RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: status,
		Body:   sqsErrorBody(errorType, code, message),
	}
}

// sqsErrorBody creates the JSON body for an SQS error response.
func sqsErrorBody(errorType, code, message string) string {
	payload := struct {
		Type    string `json:"__type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Type:    composeSqsErrorType(errorType, code),
		Code:    code,
		Message: message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return `{"__type":"serialization#Failure","message":"failed to marshal sqs error payload"}`
	}

	return string(body)
}

// composeSqsErrorType creates the __type field value for SQS errors.
func composeSqsErrorType(errorType, code string) string {
	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		return code
	}
	return errorType + "#" + code
}

// timeoutError implements net.Error for timeout simulation.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
