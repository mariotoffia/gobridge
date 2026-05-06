package dynamodboutbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestMapError_PreservesContextErrors asserts policy Rule 1
// (`_design/error-wrapping-policy.adoc:100-104`): canonical context
// sentinels are returned identity-equal and never reclassified as
// shared.ErrTimeout / shared.ErrUnavailable.
func TestMapError_PreservesContextErrors(t *testing.T) {
	wrappedDeadline := fmt.Errorf("sdk call: %w", context.DeadlineExceeded)
	wrappedCanceled := fmt.Errorf("sdk call: %w", context.Canceled)

	tests := []struct {
		name  string
		input error
		check func(t *testing.T, in, out error)
	}{
		{
			name:  "direct-deadline-exceeded",
			input: context.DeadlineExceeded,
			check: func(t *testing.T, in, out error) {
				if out != context.DeadlineExceeded {
					t.Fatalf("want identity-equal context.DeadlineExceeded, got %v (%T)", out, out)
				}
				if !errors.Is(out, context.DeadlineExceeded) {
					t.Fatalf("errors.Is(out, context.DeadlineExceeded) = false")
				}
				if errors.Is(out, shared.ErrTimeout) {
					t.Fatalf("ctx error must not be classified as shared.ErrTimeout")
				}
			},
		},
		{
			name:  "direct-canceled",
			input: context.Canceled,
			check: func(t *testing.T, in, out error) {
				if out != context.Canceled {
					t.Fatalf("want identity-equal context.Canceled, got %v (%T)", out, out)
				}
				if !errors.Is(out, context.Canceled) {
					t.Fatalf("errors.Is(out, context.Canceled) = false")
				}
				if errors.Is(out, shared.ErrUnavailable) {
					t.Fatalf("ctx error must not be classified as shared.ErrUnavailable")
				}
			},
		},
		{
			name:  "wrapped-deadline-exceeded",
			input: wrappedDeadline,
			check: func(t *testing.T, in, out error) {
				if out != in {
					t.Fatalf("want identity-equal wrapped input, got %v", out)
				}
				if !errors.Is(out, context.DeadlineExceeded) {
					t.Fatalf("errors.Is(out, context.DeadlineExceeded) = false")
				}
				if errors.Is(out, shared.ErrTimeout) {
					t.Fatalf("wrapped ctx error must not be classified as shared.ErrTimeout")
				}
			},
		},
		{
			name:  "wrapped-canceled",
			input: wrappedCanceled,
			check: func(t *testing.T, in, out error) {
				if out != in {
					t.Fatalf("want identity-equal wrapped input, got %v", out)
				}
				if !errors.Is(out, context.Canceled) {
					t.Fatalf("errors.Is(out, context.Canceled) = false")
				}
				if errors.Is(out, shared.ErrUnavailable) {
					t.Fatalf("wrapped ctx error must not be classified as shared.ErrUnavailable")
				}
			},
		},
		{
			name:  "resource-not-found-regression",
			input: &ddbtypes.ResourceNotFoundException{},
			check: func(t *testing.T, in, out error) {
				if !errors.Is(out, shared.ErrNotFound) {
					t.Fatalf("ResourceNotFoundException must classify as shared.ErrNotFound, got %v", out)
				}
			},
		},
		{
			name:  "provisioned-throughput-regression",
			input: &ddbtypes.ProvisionedThroughputExceededException{},
			check: func(t *testing.T, in, out error) {
				if !errors.Is(out, shared.ErrThrottled) {
					t.Fatalf("ProvisionedThroughputExceededException must classify as shared.ErrThrottled, got %v", out)
				}
			},
		},
		{
			name:  "nil-input",
			input: nil,
			check: func(t *testing.T, in, out error) {
				if out != nil {
					t.Fatalf("mapError(nil) want nil, got %v", out)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := mapError(tc.input)
			tc.check(t, tc.input, out)
		})
	}
}
