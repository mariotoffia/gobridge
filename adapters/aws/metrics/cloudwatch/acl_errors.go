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
// fault), transient credential-rotation faults (expired/not-yet-propagated
// tokens during ECS/IAM rotation), server faults (5xx), and anything that
// is not a modeled API error (network failures, timeouts, context
// cancellation) — the batch is requeued, bounded by MaxRetryDatums.
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
	case "ExpiredTokenException", "InvalidClientTokenId",
		"UnrecognizedClientException":
		// Transient credential-rotation faults: while an ECS task role / IAM
		// credential rotates, the in-flight signer briefly presents an
		// expired or not-yet-propagated token. These are client faults but
		// clear within seconds once fresh credentials load, so retry (bounded
		// by MaxRetryDatums) instead of permanently dropping the batch — a
		// drop would also lose the ExporterDropped/Rejected loss counters
		// riding in that batch, hiding the blip entirely.
		return false
	}
	// Remaining client faults are validation-class rejections
	// (InvalidParameterValue, InvalidParameterCombination,
	// MissingRequiredParameter, InvalidFormat, ValidationError, ...):
	// permanent for this batch. Server faults and unknown faults are
	// retryable.
	return apiErr.ErrorFault() == smithy.FaultClient
}
