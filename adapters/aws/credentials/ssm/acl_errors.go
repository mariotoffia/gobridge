package ssm

import (
	"errors"
	"fmt"

	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// SSM SDK error classification.
//
// This file is the error-classification half of the SSM ACL: it owns
// the only references to ssmtypes for error sentinel checks and is
// the single point that maps AWS SDK errors to shared.BridgeError
// kinds.

func mapAWSError(err error) error {
	if err == nil {
		return nil
	}

	var paramNotFound *ssmtypes.ParameterNotFound
	if errors.As(err, &paramNotFound) {
		return shared.ErrNotFound.WithMessage("SSM parameter not found")
	}

	var paramAlreadyExists *ssmtypes.ParameterAlreadyExists
	if errors.As(err, &paramAlreadyExists) {
		return shared.ErrAlreadyExists.WithMessage("SSM parameter already exists")
	}

	return shared.ErrUnavailable.Wrap(fmt.Errorf("ssm: %w", err))
}
