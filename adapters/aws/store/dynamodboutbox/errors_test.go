package dynamodboutbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain"
)

// TestMapError_PreservesContextErrors asserts policy Rule 1
// (`_design/error-wrapping-policy.adoc:100-104`): canonical context
// sentinels are returned identity-equal and never reclassified as
// domain.ErrTimeout / domain.ErrUnavailable.
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
				if errors.Is(out, domain.ErrTimeout) {
					t.Fatalf("ctx error must not be classified as domain.ErrTimeout")
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
				if errors.Is(out, domain.ErrUnavailable) {
					t.Fatalf("ctx error must not be classified as domain.ErrUnavailable")
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
				if errors.Is(out, domain.ErrTimeout) {
					t.Fatalf("wrapped ctx error must not be classified as domain.ErrTimeout")
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
				if errors.Is(out, domain.ErrUnavailable) {
					t.Fatalf("wrapped ctx error must not be classified as domain.ErrUnavailable")
				}
			},
		},
		{
			name:  "resource-not-found-regression",
			input: &ddbtypes.ResourceNotFoundException{},
			check: func(t *testing.T, in, out error) {
				if !errors.Is(out, domain.ErrNotFound) {
					t.Fatalf("ResourceNotFoundException must classify as domain.ErrNotFound, got %v", out)
				}
			},
		},
		{
			name:  "provisioned-throughput-regression",
			input: &ddbtypes.ProvisionedThroughputExceededException{},
			check: func(t *testing.T, in, out error) {
				if !errors.Is(out, domain.ErrThrottled) {
					t.Fatalf("ProvisionedThroughputExceededException must classify as domain.ErrThrottled, got %v", out)
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
