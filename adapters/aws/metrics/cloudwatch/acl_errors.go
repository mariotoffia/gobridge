package cloudwatch

import (
	"errors"

	"github.com/aws/smithy-go"
)

// isPermanentPutError classifies a PutMetricData failure (MF-3).
//
// Permanent (return true): validation-class client faults such as
// InvalidParameterValue / MissingRequiredParameter — retrying the same
// batch can never succeed, so the caller must drop it (and count the
// drop) instead of requeueing it forever. One poison datum previously
// bricked the whole pipeline because PutMetricData is all-or-nothing
// and the failed batch was requeued unconditionally.
//
// Retryable (return false): throttling (even though it is a client
// fault), server faults (5xx), and anything that is not a modeled API
// error (network failures, timeouts, context cancellation) — the batch
// is requeued, bounded by MaxRetryDatums.
func isPermanentPutError(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		// Network / timeout / cancellation: retryable.
		return false
	}
	switch apiErr.ErrorCode() {
	case "Throttling", "ThrottlingException", "RequestThrottled",
		"RequestThrottledException", "TooManyRequestsException",
		"RequestLimitExceeded", "LimitExceededException",
		"ProvisionedThroughputExceededException", "SlowDown":
		return false
	}
	// Remaining client faults are validation-class rejections
	// (InvalidParameterValue, InvalidParameterCombination,
	// MissingRequiredParameter, InvalidFormat, ValidationError, ...):
	// permanent for this batch. Server faults and unknown faults are
	// retryable.
	return apiErr.ErrorFault() == smithy.FaultClient
}
