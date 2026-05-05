package ssm

import (
	"errors"
	"fmt"

	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/mariotoffia/gobridge/domain"
)

// SSM SDK error classification.
//
// This file is the error-classification half of the SSM ACL: it owns
// the only references to ssmtypes for error sentinel checks and is
// the single point that maps AWS SDK errors to domain.BridgeError
// kinds.

func mapAWSError(err error) error {
	if err == nil {
		return nil
	}

	var paramNotFound *ssmtypes.ParameterNotFound
	if errors.As(err, &paramNotFound) {
		return domain.ErrNotFound.WithMessage("SSM parameter not found")
	}

	var paramAlreadyExists *ssmtypes.ParameterAlreadyExists
	if errors.As(err, &paramAlreadyExists) {
		return domain.ErrAlreadyExists.WithMessage("SSM parameter already exists")
	}

	return domain.ErrUnavailable.Wrap(fmt.Errorf("ssm: %w", err))
}
