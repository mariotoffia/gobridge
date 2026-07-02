package ssm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	smithy "github.com/aws/smithy-go"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Regression for J8: SSM SDK errors must be classified by behaviour so
// callers can react correctly, instead of collapsing every failure into
// a generic "unavailable".
func TestMapAWSError_Classification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want *shared.BridgeError
	}{
		{
			name: "context_deadline_is_timeout",
			err:  fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
			want: shared.ErrTimeout,
		},
		{
			name: "context_canceled_is_timeout",
			err:  context.Canceled,
			want: shared.ErrTimeout,
		},
		{
			name: "parameter_not_found",
			err:  &ssmtypes.ParameterNotFound{},
			want: shared.ErrNotFound,
		},
		{
			name: "already_exists",
			err:  &ssmtypes.ParameterAlreadyExists{},
			want: shared.ErrAlreadyExists,
		},
		{
			name: "too_many_updates_typed_is_throttled",
			err:  &ssmtypes.TooManyUpdates{},
			want: shared.ErrThrottled,
		},
		{
			name: "throttling_code_is_throttled",
			err:  &smithy.GenericAPIError{Code: "ThrottlingException"},
			want: shared.ErrThrottled,
		},
		{
			name: "access_denied_is_not_authorized",
			err:  &smithy.GenericAPIError{Code: "AccessDeniedException"},
			want: shared.ErrNotAuthorized,
		},
		{
			name: "kms_access_denied_is_not_authorized",
			err:  &smithy.GenericAPIError{Code: "KMSAccessDeniedException"},
			want: shared.ErrNotAuthorized,
		},
		{
			name: "kms_disabled_is_unavailable",
			err:  &smithy.GenericAPIError{Code: "KMSInvalidStateException"},
			want: shared.ErrUnavailable,
		},
		{
			name: "unknown_defaults_to_unavailable",
			err:  errors.New("mystery"),
			want: shared.ErrUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapAWSError(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapAWSError(%v) = %v, want kind %v", tc.err, got, tc.want.Error())
			}
		})
	}
}

func TestMapAWSError_NilIsNil(t *testing.T) {
	if err := mapAWSError(nil); err != nil {
		t.Fatalf("mapAWSError(nil) = %v, want nil", err)
	}
}
