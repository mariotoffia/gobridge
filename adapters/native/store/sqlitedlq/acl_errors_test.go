package sqlitedlq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
)

// TestMapError_PreservesContextErrors asserts policy Rule 1
// (`_design/error-wrapping-policy.adoc:100-104`): canonical context
// sentinels are returned identity-equal and never reclassified as
// domain.ErrTimeout / domain.ErrUnavailable.
func TestMapError_PreservesContextErrors(t *testing.T) {
	wrappedDeadline := fmt.Errorf("sqlite call: %w", context.DeadlineExceeded)
	wrappedCanceled := fmt.Errorf("sqlite call: %w", context.Canceled)

	tests := []struct {
		name  string
		input error
		check func(t *testing.T, in, out error)
	}{
		{
			name:  "direct-deadline-exceeded",
			input: context.DeadlineExceeded,
			check: func(t *testing.T, _, out error) {
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
			check: func(t *testing.T, _, out error) {
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
			name:  "no-rows",
			input: sql.ErrNoRows,
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, domain.ErrNotFound) {
					t.Fatalf("sql.ErrNoRows must classify as domain.ErrNotFound, got %v", out)
				}
			},
		},
		{
			name:  "unique-violation-string-match",
			input: errors.New("constraint failed: UNIQUE constraint failed: dlq.id"),
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, domain.ErrDuplicateRecord) {
					t.Fatalf("UNIQUE constraint must classify as domain.ErrDuplicateRecord, got %v", out)
				}
			},
		},
		{
			name:  "busy-locked-string-match",
			input: errors.New("database is locked"),
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, domain.ErrThrottled) {
					t.Fatalf("database is locked must classify as domain.ErrThrottled, got %v", out)
				}
			},
		},
		{
			name:  "io-eof",
			input: io.EOF,
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, domain.ErrConnectionLost) {
					t.Fatalf("io.EOF must classify as domain.ErrConnectionLost, got %v", out)
				}
			},
		},
		{
			name:  "generic-error-fallback",
			input: errors.New("some random sqlite error"),
			check: func(t *testing.T, _, out error) {
				if !errors.Is(out, domain.ErrUnavailable) {
					t.Fatalf("unclassified error must default to domain.ErrUnavailable, got %v", out)
				}
			},
		},
		{
			name:  "nil-input",
			input: nil,
			check: func(t *testing.T, _, out error) {
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

// TestWrapErr_ContextSentinelPassThrough asserts that wrapErr does not
// wrap canonical ctx sentinels and never produces a *domain.BridgeError
// from them.
func TestWrapErr_ContextSentinelPassThrough(t *testing.T) {
	out := wrapErr(context.Canceled, "sqlitedlq: write", "entryID", "abc")
	if out != context.Canceled {
		t.Fatalf("wrapErr must pass ctx.Canceled through unchanged, got %v (%T)", out, out)
	}
	if _, ok := out.(*domain.BridgeError); ok {
		t.Fatalf("wrapErr must not produce a *domain.BridgeError from ctx sentinel")
	}
}

// TestWrapErr_AttachesKVs asserts wrapErr classifies and decorates a
// non-ctx error with the supplied kvs.
func TestWrapErr_AttachesKVs(t *testing.T) {
	out := wrapErr(sql.ErrNoRows, "sqlitedlq: get", "entryID", "abc")
	if !errors.Is(out, domain.ErrNotFound) {
		t.Fatalf("want errors.Is(out, ErrNotFound), got %v", out)
	}
	be, ok := domain.AsBridgeError(out)
	if !ok {
		t.Fatalf("want *domain.BridgeError, got %T", out)
	}
	if be.Message == "" {
		t.Fatalf("want message attached, got empty")
	}
}
