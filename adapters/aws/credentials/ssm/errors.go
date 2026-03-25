package ssm

import (
	"errors"
	"fmt"

	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/mariotoffia/gobridge/domain"
)

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
