// Package awsutils provides test utilities for AWS service testing.
package awsutils

import (
	"errors"
	"net"
)

// DynamoDBErrors exposes helpers that simulate AWS DynamoDB error responses
// through the RoundTripperTransaction abstraction.
//
// Usage:
//
//	rt := NewRoundTripper(nil)
//	rt.Enable().Push(DynamoDBErrors{}.ProvisionedThroughputExceeded())
type DynamoDBErrors struct{}

// ============================================================================
// Retryable Errors
// ============================================================================

// ProvisionedThroughputExceeded simulates a DynamoDB ProvisionedThroughputExceededException.
// This occurs when request rate exceeds the provisioned throughput.
//
// NOTE: This is retryable
func (DynamoDBErrors) ProvisionedThroughputExceeded() RoundTripperTransaction {
	return newDynamoDBError(400, "ProvisionedThroughputExceededException", "Simulated provisioned throughput exceeded")
}

// Throttling simulates a DynamoDB ThrottlingException.
// This occurs when requests are being throttled.
//
// NOTE: This is retryable
func (DynamoDBErrors) Throttling() RoundTripperTransaction {
	return newDynamoDBError(400, "ThrottlingException", "Simulated throttling")
}

// RequestLimitExceeded simulates a DynamoDB RequestLimitExceeded error.
// This occurs when the account request limit is exceeded.
//
// NOTE: This is retryable
func (DynamoDBErrors) RequestLimitExceeded() RoundTripperTransaction {
	return newDynamoDBError(400, "RequestLimitExceeded", "Simulated account request limit exceeded")
}

// InternalServerError simulates a DynamoDB InternalServerError.
// This is a transient server-side error.
//
// NOTE: This is retryable
func (DynamoDBErrors) InternalServerError() RoundTripperTransaction {
	return newDynamoDBError(500, "InternalServerError", "Simulated internal server fault")
}

// ServiceUnavailable simulates a DynamoDB service unavailability.
//
// NOTE: This is retryable
func (DynamoDBErrors) ServiceUnavailable() RoundTripperTransaction {
	return newDynamoDBError(503, "ServiceUnavailable", "Simulated service unavailable")
}

// TransactionConflict simulates a DynamoDB TransactionConflictException.
// This occurs when there's a conflict with a concurrent transaction.
//
// NOTE: This is retryable
func (DynamoDBErrors) TransactionConflict() RoundTripperTransaction {
	return newDynamoDBError(400, "TransactionConflictException", "Simulated transaction conflict")
}

// ResourceInUse simulates a DynamoDB ResourceInUseException.
// This occurs when attempting to modify a resource that is in use.
//
// NOTE: This is retryable
func (DynamoDBErrors) ResourceInUse() RoundTripperTransaction {
	return newDynamoDBError(400, "ResourceInUseException", "Simulated resource in use")
}

// LimitExceeded simulates a DynamoDB LimitExceededException.
// This occurs when a limit is exceeded.
//
// NOTE: This is retryable
func (DynamoDBErrors) LimitExceeded() RoundTripperTransaction {
	return newDynamoDBError(400, "LimitExceededException", "Simulated limit exceeded")
}

// TableInTransition simulates a DynamoDB table in transition state.
// This occurs when the table is being created, deleted, or updated.
//
// NOTE: This is retryable
func (DynamoDBErrors) TableInTransition() RoundTripperTransaction {
	return newDynamoDBError(400, "ResourceInUseException", "Simulated table in transition")
}

// ============================================================================
// Non-Retryable Errors
// ============================================================================

// ResourceNotFound simulates a DynamoDB ResourceNotFoundException.
// This occurs when the requested resource does not exist.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ResourceNotFound() RoundTripperTransaction {
	return newDynamoDBError(400, "ResourceNotFoundException", "Simulated resource not found")
}

// TableNotFound simulates a DynamoDB table not found error.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) TableNotFound() RoundTripperTransaction {
	return newDynamoDBError(400, "ResourceNotFoundException", "Simulated table not found")
}

// ValidationException simulates a DynamoDB ValidationException.
// This occurs when the request parameters are invalid.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ValidationException() RoundTripperTransaction {
	return newDynamoDBError(400, "ValidationException", "Simulated validation failure")
}

// ConditionalCheckFailed simulates a DynamoDB ConditionalCheckFailedException.
// This occurs when a conditional expression evaluates to false.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ConditionalCheckFailed() RoundTripperTransaction {
	return newDynamoDBError(400, "ConditionalCheckFailedException", "Simulated conditional check failed")
}

// ItemCollectionSizeLimitExceeded simulates a DynamoDB ItemCollectionSizeLimitExceededException.
// This occurs when the item collection size exceeds the limit.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ItemCollectionSizeLimitExceeded() RoundTripperTransaction {
	return newDynamoDBError(400, "ItemCollectionSizeLimitExceededException", "Simulated item collection size exceeded")
}

// AccessDenied simulates a DynamoDB AccessDeniedException.
// This occurs when the caller doesn't have permission.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) AccessDenied() RoundTripperTransaction {
	return newDynamoDBError(403, "AccessDeniedException", "Simulated access denied")
}

// UnrecognizedClient simulates a DynamoDB UnrecognizedClientException.
// This occurs when the credentials are invalid.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) UnrecognizedClient() RoundTripperTransaction {
	return newDynamoDBError(403, "UnrecognizedClientException", "Simulated bad credentials")
}

// ExpiredIterator simulates a DynamoDB ExpiredIteratorException.
// This occurs when the stream iterator has expired.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ExpiredIterator() RoundTripperTransaction {
	return newDynamoDBError(400, "ExpiredIteratorException", "Simulated expired stream iterator")
}

// TransactionCanceled simulates a DynamoDB TransactionCanceledException.
// This occurs when a transaction is canceled.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) TransactionCanceled() RoundTripperTransaction {
	return newDynamoDBError(400, "TransactionCanceledException", "Simulated transaction canceled")
}

// IdempotentParameterMismatch simulates a DynamoDB IdempotentParameterMismatchException.
// This occurs when an idempotent request has mismatched parameters.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) IdempotentParameterMismatch() RoundTripperTransaction {
	return newDynamoDBError(400, "IdempotentParameterMismatchException", "Simulated idempotent parameter mismatch")
}

// BackupInUse simulates a DynamoDB BackupInUseException.
// This occurs when a backup is already in use.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) BackupInUse() RoundTripperTransaction {
	return newDynamoDBError(400, "BackupInUseException", "Simulated backup in use")
}

// BackupNotFound simulates a DynamoDB BackupNotFoundException.
// This occurs when the specified backup doesn't exist.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) BackupNotFound() RoundTripperTransaction {
	return newDynamoDBError(400, "BackupNotFoundException", "Simulated backup not found")
}

// ContinuousBackupsUnavailable simulates a DynamoDB ContinuousBackupsUnavailableException.
// This occurs when continuous backups are not available.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ContinuousBackupsUnavailable() RoundTripperTransaction {
	return newDynamoDBError(400, "ContinuousBackupsUnavailableException", "Simulated continuous backups unavailable")
}

// GlobalTableAlreadyExists simulates a DynamoDB GlobalTableAlreadyExistsException.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) GlobalTableAlreadyExists() RoundTripperTransaction {
	return newDynamoDBError(400, "GlobalTableAlreadyExistsException", "Simulated global table already exists")
}

// GlobalTableNotFound simulates a DynamoDB GlobalTableNotFoundException.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) GlobalTableNotFound() RoundTripperTransaction {
	return newDynamoDBError(400, "GlobalTableNotFoundException", "Simulated global table not found")
}

// IndexNotFound simulates a DynamoDB index not found error.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) IndexNotFound() RoundTripperTransaction {
	return newDynamoDBError(400, "ResourceNotFoundException", "Simulated index not found")
}

// ReplicaAlreadyExists simulates a DynamoDB ReplicaAlreadyExistsException.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ReplicaAlreadyExists() RoundTripperTransaction {
	return newDynamoDBError(400, "ReplicaAlreadyExistsException", "Simulated replica already exists")
}

// ReplicaNotFound simulates a DynamoDB ReplicaNotFoundException.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) ReplicaNotFound() RoundTripperTransaction {
	return newDynamoDBError(400, "ReplicaNotFoundException", "Simulated replica not found")
}

// TableAlreadyExists simulates a DynamoDB TableAlreadyExistsException.
//
// NOTE: This is NOT retryable
func (DynamoDBErrors) TableAlreadyExists() RoundTripperTransaction {
	return newDynamoDBError(400, "TableAlreadyExistsException", "Simulated table already exists")
}

// ============================================================================
// Network Errors
// ============================================================================

// NetworkError returns a transaction that simulates a network connection error.
// This is useful for testing connection recovery behavior.
//
// NOTE: This is retryable
func (DynamoDBErrors) NetworkError() RoundTripperTransaction {
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
func (DynamoDBErrors) ConnectionReset() RoundTripperTransaction {
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
func (DynamoDBErrors) DNSError() RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: &net.DNSError{
			Err:  "no such host",
			Name: "dynamodb.us-east-1.amazonaws.com",
		},
	}
}

// Timeout returns a transaction that simulates a request timeout.
//
// NOTE: This is retryable
func (DynamoDBErrors) Timeout() RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: timeoutError{},
	}
}

// ============================================================================
// Custom Error Helpers
// ============================================================================

// CustomError creates a transaction with a custom DynamoDB error code and message.
func (DynamoDBErrors) CustomError(status int, code, message string) RoundTripperTransaction {
	return newDynamoDBError(status, code, message)
}

// CustomNetworkError creates a transaction with a custom network error.
func (DynamoDBErrors) CustomNetworkError(err error) RoundTripperTransaction {
	return RoundTripperTransaction{
		Error: err,
	}
}

// ============================================================================
// Success Response Helpers
// ============================================================================

// PutItemSuccess creates a successful PutItem response.
func (DynamoDBErrors) PutItemSuccess() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{}`,
	}
}

// GetItemSuccess creates a successful GetItem response with an item.
func (DynamoDBErrors) GetItemSuccess(itemJSON string) RoundTripperTransaction {
	if itemJSON == "" {
		itemJSON = `{"Item":{"pk":{"S":"test"},"sk":{"S":"item"}}}`
	}
	return RoundTripperTransaction{
		Status: 200,
		Body:   itemJSON,
	}
}

// GetItemEmpty creates a successful GetItem response with no item (not found).
func (DynamoDBErrors) GetItemEmpty() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{}`,
	}
}

// QuerySuccess creates a successful Query response.
func (DynamoDBErrors) QuerySuccess(itemsJSON string) RoundTripperTransaction {
	if itemsJSON == "" {
		itemsJSON = `{"Items":[],"Count":0,"ScannedCount":0}`
	}
	return RoundTripperTransaction{
		Status: 200,
		Body:   itemsJSON,
	}
}

// DeleteItemSuccess creates a successful DeleteItem response.
func (DynamoDBErrors) DeleteItemSuccess() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{}`,
	}
}

// UpdateItemSuccess creates a successful UpdateItem response.
func (DynamoDBErrors) UpdateItemSuccess() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{}`,
	}
}

// BatchWriteItemSuccess creates a successful BatchWriteItem response.
func (DynamoDBErrors) BatchWriteItemSuccess() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{"UnprocessedItems":{}}`,
	}
}

// BatchGetItemSuccess creates a successful BatchGetItem response.
func (DynamoDBErrors) BatchGetItemSuccess(responsesJSON string) RoundTripperTransaction {
	if responsesJSON == "" {
		responsesJSON = `{"Responses":{},"UnprocessedKeys":{}}`
	}
	return RoundTripperTransaction{
		Status: 200,
		Body:   responsesJSON,
	}
}

// TransactWriteItemsSuccess creates a successful TransactWriteItems response.
func (DynamoDBErrors) TransactWriteItemsSuccess() RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: 200,
		Body:   `{}`,
	}
}

// TransactGetItemsSuccess creates a successful TransactGetItems response.
func (DynamoDBErrors) TransactGetItemsSuccess(responsesJSON string) RoundTripperTransaction {
	if responsesJSON == "" {
		responsesJSON = `{"Responses":[]}`
	}
	return RoundTripperTransaction{
		Status: 200,
		Body:   responsesJSON,
	}
}

// ============================================================================
// Internal Helpers
// ============================================================================

// newDynamoDBError creates a RoundTripperTransaction for a DynamoDB error response.
func newDynamoDBError(status int, code, message string) RoundTripperTransaction {
	return RoundTripperTransaction{
		Status: status,
		Body:   dynamoDBErrorBody(code, message),
	}
}

// dynamoDBErrorBody creates the JSON body for a DynamoDB error response.
// DynamoDB uses the format: {"__type":"com.amazonaws.dynamodb.v20120810#ErrorCode","message":"..."}
func dynamoDBErrorBody(code, message string) string {
	return `{"__type":"com.amazonaws.dynamodb.v20120810#` + code + `","message":"` + message + `"}`
}
