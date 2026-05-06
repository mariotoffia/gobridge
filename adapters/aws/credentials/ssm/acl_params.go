package ssm

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Per-call SDK request/response translation.
//
// This file is the request/response half of the SSM ACL: every
// awsssm.*Input / *Output value the adapter constructs or consumes
// lives here, alongside the only references to ssmtypes outside of
// error classification. Repository calls these methods using plain
// Go types and never sees an SDK request struct.

// parameterValue is the decrypted (or plaintext) value of an SSM
// parameter together with its current version. version == 0 indicates
// the SDK did not return a version for the parameter.
type parameterValue struct {
	value   string
	version int64
}

// getParameter fetches a single parameter by path. If the parameter
// exists but has no value, ErrNotFound is returned. SDK errors are
// classified through mapAWSError.
func (s *session) getParameter(ctx context.Context, path string, decrypt bool) (parameterValue, error) {
	api, err := s.ensure(ctx)
	if err != nil {
		return parameterValue{}, err
	}

	out, err := api.GetParameter(ctx, &awsssm.GetParameterInput{
		Name:           aws.String(path),
		WithDecryption: aws.Bool(decrypt),
	})
	if err != nil {
		return parameterValue{}, mapAWSError(err)
	}

	if out.Parameter == nil || out.Parameter.Value == nil {
		return parameterValue{}, shared.ErrNotFound.WithMessage(
			fmt.Sprintf("SSM parameter %s has no value", path),
		)
	}

	return parameterValue{
		value:   *out.Parameter.Value,
		version: out.Parameter.Version,
	}, nil
}

// getParameterVersion returns just the current version of a
// parameter (without decrypting the value). Used for optimistic
// concurrency checks where the value is irrelevant. A missing
// parameter (no Parameter struct in response) yields version 0 and
// no error so the caller can distinguish absent-from-mismatch.
func (s *session) getParameterVersion(ctx context.Context, path string) (int64, error) {
	api, err := s.ensure(ctx)
	if err != nil {
		return 0, err
	}

	out, err := api.GetParameter(ctx, &awsssm.GetParameterInput{
		Name:           aws.String(path),
		WithDecryption: aws.Bool(false),
	})
	if err != nil {
		return 0, mapAWSError(err)
	}
	if out.Parameter == nil {
		return 0, nil
	}
	return out.Parameter.Version, nil
}

// putParameter writes a SecureString parameter at path. When
// overwrite is false, an existing parameter triggers
// ErrAlreadyExists via mapAWSError.
func (s *session) putParameter(ctx context.Context, path, value string, overwrite bool) error {
	api, err := s.ensure(ctx)
	if err != nil {
		return err
	}

	_, err = api.PutParameter(ctx, &awsssm.PutParameterInput{
		Name:      aws.String(path),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(overwrite),
	})
	if err != nil {
		return mapAWSError(err)
	}
	return nil
}

// deleteParameter removes a parameter by path.
func (s *session) deleteParameter(ctx context.Context, path string) error {
	api, err := s.ensure(ctx)
	if err != nil {
		return err
	}

	_, err = api.DeleteParameter(ctx, &awsssm.DeleteParameterInput{
		Name: aws.String(path),
	})
	if err != nil {
		return mapAWSError(err)
	}
	return nil
}

// listParameterNames recursively collects all parameter names under
// the given path prefix, transparently handling SDK pagination.
func (s *session) listParameterNames(ctx context.Context, pathPrefix string) ([]string, error) {
	api, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	var nextToken *string

	for {
		page, err := api.GetParametersByPath(ctx, &awsssm.GetParametersByPathInput{
			Path:      aws.String(pathPrefix),
			Recursive: aws.Bool(true),
			NextToken: nextToken,
		})
		if err != nil {
			return nil, mapAWSError(err)
		}

		for _, param := range page.Parameters {
			if param.Name != nil {
				names = append(names, *param.Name)
			}
		}

		if page.NextToken == nil {
			break
		}
		nextToken = page.NextToken
	}

	return names, nil
}
