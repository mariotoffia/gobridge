package ssm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	smithy "github.com/aws/smithy-go"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// SSM SDK error classification.
//
// This file is the error-classification half of the SSM ACL: it owns
// the only references to ssmtypes for error sentinel checks and is
// the single point that maps AWS SDK errors to shared.BridgeError
// kinds.
//
// Classification is behaviour-driven so callers (and resilience
// wrappers) can react correctly instead of treating every failure as a
// generic "unavailable": throttling is retryable, auth failures are
// terminal, KMS failures point at key configuration, and context
// cancellation/deadline surfaces as a timeout.

func mapAWSError(err error) error {
	if err == nil {
		return nil
	}

	// Context cancellation / deadline first: these are not service faults
	// and must not be masked as auth/throttle classifications.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return shared.ErrTimeout.Wrap(fmt.Errorf("ssm: %w", err))
	}

	var paramNotFound *ssmtypes.ParameterNotFound
	if errors.As(err, &paramNotFound) {
		return shared.ErrNotFound.WithMessage("SSM parameter not found")
	}

	var paramAlreadyExists *ssmtypes.ParameterAlreadyExists
	if errors.As(err, &paramAlreadyExists) {
		return shared.ErrAlreadyExists.WithMessage("SSM parameter already exists")
	}

	var tooManyUpdates *ssmtypes.TooManyUpdates
	if errors.As(err, &tooManyUpdates) {
		return shared.ErrThrottled.Wrap(fmt.Errorf("ssm: %w", err))
	}

	// Fall back to the protocol-agnostic API error code for exceptions that
	// have no dedicated typed model in the SSM SDK (throttling, access
	// denied, KMS faults).
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if mapped := classifyAPICode(apiErr.ErrorCode(), err); mapped != nil {
			return mapped
		}
	}

	return shared.ErrUnavailable.Wrap(fmt.Errorf("ssm: %w", err))
}

// classifyAPICode maps a smithy API error code to a shared error kind.
// It returns nil when the code is unrecognised so the caller can apply the
// default (unavailable) classification.
func classifyAPICode(code string, err error) error {
	wrapped := fmt.Errorf("ssm: %w", err)
	switch {
	case isThrottleCode(code):
		return shared.ErrThrottled.Wrap(wrapped)
	case isAuthCode(code):
		return shared.ErrNotAuthorized.WithMessage("SSM access denied: " + code).Wrap(wrapped)
	case strings.HasPrefix(code, "KMS"):
		// KMS access-denied is an authorization problem; every other KMS
		// fault (disabled/invalid key state) is a dependency problem the
		// operator must fix, surfaced as unavailable with the concrete code.
		if strings.Contains(code, "AccessDenied") {
			return shared.ErrNotAuthorized.WithMessage("SSM KMS access denied: " + code).Wrap(wrapped)
		}
		return shared.ErrUnavailable.WithMessage("SSM KMS error: " + code).Wrap(wrapped)
	}
	return nil
}

func isThrottleCode(code string) bool {
	switch code {
	case "ThrottlingException",
		"Throttling",
		"ThrottledException",
		"TooManyUpdatesException",
		"ProvisionedThroughputExceededException",
		"RequestLimitExceeded",
		"RequestThrottled":
		return true
	default:
		return false
	}
}

func isAuthCode(code string) bool {
	switch code {
	case "AccessDeniedException",
		"AccessDenied",
		"UnauthorizedException",
		"UnrecognizedClientException",
		"MissingAuthenticationTokenException":
		return true
	default:
		return false
	}
}
